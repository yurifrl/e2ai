// The manual trigger boundary (TRIG-01..05): one transaction that mirrors the
// deploy path 1:1 — pause gate → budget gate → approved-plan resolution →
// insert-as-gate run row → trigger-time plan pin → audit row → provisioning
// transitions → ExecuteArgs enqueue (CONTEXT decision 3). The run is born as
// trigger_source='manual' with a synthetic negative workflow_run_id from the
// dedicated cockpit_run_ids sequence (0010), run_number 0, attributed to the
// operator via runs.triggered_by — never a silent webhook default.
//
// Concurrency (RESEARCH D1/P3): the 0010 partial unique index
// runs_one_manual_in_flight_per_app is the serializer. Two rapid triggers
// carry different synthetic workflow_run_ids, so River UniqueOpts alone cannot
// serialize them; the insert-as-gate index makes the racing second INSERT a
// no-op (bare `on conflict do nothing`), surfaced here as pgx.ErrNoRows and
// mapped to 409 with the in-flight run id. UniqueOpts on the execute enqueue
// remains the belt against double-enqueue of the SAME run.
package cockpit

import (
	"context"
	"errors"
	"fmt"

	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/NSXBet/e2ai/api/internal/suite"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// inFlightRun is the active-run refusal payload surfaced to the operator.
type inFlightRun struct {
	ID     string `json:"run_id"`
	Status string `json:"status"`
}

// Re-run modes (TRIG-04). Empty mode = a plain manual run at the request's
// head_sha; reproduce copies the source run's SHA; retest pulls the app's
// current default-branch head through HeadResolver — locked in CONTEXT
// decision 3: the resolver is authoritative, no fallback to stored runs.
const (
	ModeReproduce = "reproduce"
	ModeRetest    = "retest"
)

// pinSource is the provenance stamped in run_plan_resolutions.selected for a
// trigger-time pin — distinct from suite.resolutionSource ("plans_source",
// the diff-derived catalog selection): the pin came from an operator choice,
// not the catalog walk.
const pinSource = "manual_trigger"

// TriggerRequest is the POST /api/cockpit/triggers body. PlanVersionID is the
// pinned approved plan version (TRIG-02): the web picker offers approved
// versions, the API re-validates server-side — an unapproved or zero-step
// version never reaches the queue.
type TriggerRequest struct {
	AppID         string `json:"app_id"`
	HeadSHA       string `json:"head_sha"`
	HeadBranch    string `json:"head_branch,omitempty"`
	Environment   string `json:"environment"`
	Reason        string `json:"reason,omitempty"`
	Mode          string `json:"mode,omitempty"`          // "" | reproduce | retest
	SourceRunID   string `json:"source_run_id,omitempty"` // required for reproduce
	PlanVersionID string `json:"plan_version_id"`         // required: the pin

	// actor is set by the HTTP boundary from the middleware-resolved
	// identity; it never travels through the request body.
	actor string
}

// HeadResolver resolves a repo's current default-branch head SHA for the
// retest mode. GitHub-backed in production (wired beside the sweep client);
// tests fake it. A lookup failure is a fail-closed refusal — the trigger
// never silently falls back to a stored run's SHA.
type HeadResolver interface {
	DefaultBranchHead(ctx context.Context, owner, repo string) (string, error)
}

// GitHubHeads adapts the platform GitHub client to HeadResolver. A nil client
// (degraded GitHub) refuses at trigger time.
type GitHubHeads struct {
	Client *github.Client
}

// DefaultBranchHead implements HeadResolver.
func (g GitHubHeads) DefaultBranchHead(ctx context.Context, owner, repo string) (string, error) {
	if g.Client == nil {
		return "", errors.New("github client unavailable; retest cannot resolve the default-branch head")
	}
	return g.Client.DefaultBranchHead(ctx, owner, repo)
}

// Typed trigger refusals. The HTTP boundary maps each to its status + wire
// reason; the domain only produces them.
var (
	// ErrPaused carries the pause reason in its text (COCK-04).
	ErrPaused = errors.New("cockpit paused")
	// ErrBudgetBreach: day-scoped spend at or over the effective ceiling.
	ErrBudgetBreach = errors.New("budget exhausted")
	// ErrZeroCeiling: effective budget ceiling resolved to 0 (BUDG-03) —
	// never unlimited, refused at the boundary like a validation error.
	ErrZeroCeiling = errors.New("budget ceiling is 0; manual triggers are refused until a positive cap is set")
	// ErrUnapprovedPlan: the named version is not the app's approved one.
	ErrUnapprovedPlan = errors.New("plan version is not approved")
	// ErrZeroApprovedPlans: the app has no approved versions to pin.
	ErrZeroApprovedPlans = errors.New("no approved plan versions; queue it in Approvals first")
	// ErrZeroStepPlan: an approved version parsed to zero steps (CONTEXT
	// decision 3) — rejected here, never silently enqueued.
	ErrZeroStepPlan = errors.New("plan version has no steps")
	// ErrActiveManualRun: another manual run on this app is in flight.
	ErrActiveManualRun = errors.New("active manual run exists")
	// ErrHeadResolve: the retest head lookup failed — fail-closed.
	ErrHeadResolve = errors.New("default-branch head lookup failed")
	// ErrUnknownApp: app_id matches no registered app.
	ErrUnknownApp = errors.New("app not registered")
)

// Trigger validates and enacts one manual run in a single transaction. On
// success it returns the created run's id and synthetic workflow_run_id; every
// refusal is a typed error above, and nothing partial ever commits.
func (s *Service) Trigger(ctx context.Context, req TriggerRequest) (runID uuid.UUID, workflowRunID int64, err error) {
	if s.Pool == nil {
		return uuid.Nil, 0, errors.New("postgres unavailable")
	}
	if err := req.validate(); err != nil {
		return uuid.Nil, 0, err
	}
	appID, err := uuid.Parse(req.AppID)
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("%w: app_id is not a uuid", errInvalidRequest)
	}
	a, err := s.lookupApp(ctx, appID)
	if err != nil {
		return uuid.Nil, 0, err
	}

	// REVIEW WR-03: app_id pins the registered environment (registered_apps is
	// unique per owner/repo/workflow/environment) — a mismatched label would
	// stamp a phantom environment onto the run row. Refuse before spending.
	if req.Environment != a.Environment {
		return uuid.Nil, 0, fmt.Errorf("%w: environment %q is not the app's registered environment %q",
			errInvalidRequest, req.Environment, a.Environment)
	}

	// Gates that read outside the write tx (pause + budget + plan) happen
	// first; order safety comes from the insert-as-gate index, the final
	// arbiter inside the tx.
	if err := s.checkPaused(ctx); err != nil {
		return uuid.Nil, 0, err
	}
	pgAppID := pgtype.UUID{Bytes: appID, Valid: true}
	verdict, err := Evaluate(ctx, s.Pool, pgAppID)
	if err != nil {
		return uuid.Nil, 0, err
	}
	if verdict.Cap.Capped() && verdict.Cap.Millicents == 0 {
		return uuid.Nil, 0, ErrZeroCeiling
	}
	if verdict.Refused {
		return uuid.Nil, 0, fmt.Errorf("%w: %s", ErrBudgetBreach, verdict.Reason)
	}
	p, err := s.resolvePin(ctx, pgAppID, req.PlanVersionID)
	if err != nil {
		return uuid.Nil, 0, err
	}

	headSHA, err := s.resolveHead(ctx, req, a)
	if err != nil {
		return uuid.Nil, 0, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, fmt.Errorf("begin trigger tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// REVIEW WR-03: the pre-gate pause read races a pause() commit between
	// the boundary check and Begin — re-read paused on this tx before
	// spending anything; a pause landing in the gap stops the run here.
	if perr := s.checkPausedTx(ctx, tx); perr != nil {
		return uuid.Nil, 0, perr
	}

	runID, workflowRunID, inFlight, err := insertManualRun(ctx, s.Pool, tx, pgAppID, headSHA, req)
	if err != nil {
		return uuid.Nil, 0, err
	}
	if inFlight != nil {
		return uuid.Nil, 0, fmt.Errorf("%w: run %s is %s", ErrActiveManualRun, inFlight.ID, inFlight.Status)
	}

	// Walk the state machine exactly like the deploy path: insert at queued,
	// then queued → waiting-rollout → provisioning inside this tx
	// (validTransitions is the law; PATTERNS canary-analog correction).
	pgRunID := pgtype.UUID{Bytes: runID, Valid: true}
	for _, to := range []runs.Status{runs.StatusWaitingRollout, runs.StatusProvisioning} {
		if err := runs.Transition(ctx, tx, pgRunID, to); err != nil {
			return uuid.Nil, 0, fmt.Errorf("manual run transition to %s: %w", to, err)
		}
	}

	// Trigger-time plan pin: the spawn gate reuses this row and never
	// re-resolves, so a post-trigger approval change cannot swap what this
	// run executes. mode stays the legal 'approved'; provenance lives in the
	// resolution jsonb (the mode check constraint is closed).
	if _, err := tx.Exec(ctx, `
		insert into run_plan_resolutions (run_id, plan_id, plan_version_id, content_hash, selected, mode, resolution)
		values ($1, $2, $3, $4, $5, 'approved', $6)`,
		runID, p.PlanID, p.VersionUUID, p.Hash, p.SelectedJSON, p.ResolutionJSON); err != nil {
		return uuid.Nil, 0, fmt.Errorf("stamp pinned plan resolution: %w", err)
	}

	// Audit in the same tx as the mutation (AUDT-01); actor is context-only
	// — the HTTP boundary refuses placeholder identities before this call.
	if err := AppendAudit(ctx, tx, AuditEntry{
		ActorEmail: req.actor,
		Action:     ActionTrigger,
		TargetType: "run",
		TargetID:   runID.String(),
		Reason:     req.Reason,
		Details: map[string]any{
			"mode":            req.Mode,
			"head_sha":        headSHA,
			"plan_version_id": req.PlanVersionID,
			"environment":     req.Environment,
		},
	}); err != nil {
		return uuid.Nil, 0, err
	}

	// Enqueue INSIDE the tx (deploy-path shape); UniqueOpts is the belt
	// against a racing duplicate of the same run's job.
	if _, err := s.Queue.InsertTx(ctx, tx, suite.ExecuteArgs{RunID: runID.String()}, triggerExecuteOpts()); err != nil {
		return uuid.Nil, 0, fmt.Errorf("enqueue execute job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, 0, fmt.Errorf("commit trigger tx: %w", err)
	}
	return runID, workflowRunID, nil
}

// triggerExecuteOpts mirrors ingest/rollout.go executeOpts: the runner queue
// drains it, one-shot, no racing duplicates of the SAME run's job.
func triggerExecuteOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue:       "runner",
		MaxAttempts: 1,
		UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
			rivertype.JobStateScheduled,
			rivertype.JobStatePending,
			rivertype.JobStateAvailable,
			rivertype.JobStateRunning,
		}},
	}
}
