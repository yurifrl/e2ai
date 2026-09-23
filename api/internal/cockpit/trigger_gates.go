// Manual-trigger internals: app lookup, pause gate, plan-pin resolution,
// insert-as-gate run insert, and the execute enqueue options. See trigger.go
// for the transaction narrative and the D1/P3 concurrency contract.
package cockpit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/NSXBet/e2ai/api/internal/plans"
	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errInvalidRequest marks request-shape violations the boundary maps to 422.
var errInvalidRequest = errors.New("invalid trigger request")

// validate enforces the request contract: app identity, environment, the
// pinned version, and the per-mode SHA sourcing (TRIG-04). Retest ignores a
// request head_sha by design — the resolver output replaces it.
func (r *TriggerRequest) validate() error {
	switch {
	case r.AppID == "":
		return fmt.Errorf("%w: app_id is required", errInvalidRequest)
	case r.Environment == "":
		return fmt.Errorf("%w: environment is required", errInvalidRequest)
	case r.PlanVersionID == "":
		return fmt.Errorf("%w: plan_version_id is required; manual runs pin an approved plan version", errInvalidRequest)
	}
	switch r.Mode {
	case "":
		if r.HeadSHA == "" {
			return fmt.Errorf("%w: head_sha is required for a plain manual run", errInvalidRequest)
		}
	case ModeReproduce:
		if r.SourceRunID == "" {
			return fmt.Errorf("%w: source_run_id is required for mode reproduce", errInvalidRequest)
		}
	case ModeRetest:
		if r.HeadSHA != "" {
			return fmt.Errorf("%w: retest resolves the head server-side; head_sha is not accepted", errInvalidRequest)
		}
	default:
		return fmt.Errorf("%w: unknown mode %q (use \"\", \"reproduce\" or \"retest\")", errInvalidRequest, r.Mode)
	}
	return nil
}

// appRow is the registered-app slice the trigger needs: existence proof plus
// the coordinates the retest resolver uses.
type appRow struct {
	ID          uuid.UUID
	Owner       string
	Repo        string
	Environment string
}

// lookupApp fails closed on an unknown app_id — a trigger against an
// unregistered app is a typed refusal before any gate runs.
func (s *Service) lookupApp(ctx context.Context, appID uuid.UUID) (appRow, error) {
	var a appRow
	err := s.Pool.QueryRow(ctx, `
		select id::text, owner, repo, environment
		from registered_apps
		where id = $1`, appID).Scan(&a.ID, &a.Owner, &a.Repo, &a.Environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return appRow{}, fmt.Errorf("%w: %s", ErrUnknownApp, appID)
	}
	return a, err
}

// checkPaused reads the arm state of record (Plan 01 domain): a paused arm
// blocks manual triggers and the refusal carries the pause reason (COCK-04).
func (s *Service) checkPaused(ctx context.Context) error {
	state, err := Read(ctx, s.Pool)
	if err != nil {
		return fmt.Errorf("cockpit state read: %w", err)
	}
	if state.Paused {
		return fmt.Errorf("%w: %s", ErrPaused, state.PausedReason)
	}
	return nil
}

// pin is the resolved trigger-time plan pin: the approved version's identity,
// its content hash, and the pre-shaped jsonb payloads stamped straight into
// run_plan_resolutions (selected + resolution). Resolving and serializing
// here — not at spawn time — is what makes the pin sticky across later
// approval changes (P4).
type pin struct {
	PlanID         string
	VersionID      string
	Version        string
	Hash           string
	VersionUUID    pgtype.UUID
	SelectedJSON   []byte
	ResolutionJSON []byte
}

// selectedPlanJSON mirrors suite's selectedPlan wire shape so the pinned row
// is indistinguishable to the spawn gate's reader.
type selectedPlanJSON struct {
	PlanID    string `json:"plan_id"`
	Version   string `json:"version"`
	Hash      string `json:"hash"`
	Source    string `json:"source"`
	VersionID string `json:"-"`
}

// resolvePin re-validates the requested version server-side (TRIG-02): it
// must be THIS app's approved version, parseable, and non-zero-step. An app
// with zero approved versions is refused with an Approvals-queue hint.
func (s *Service) resolvePin(ctx context.Context, appID pgtype.UUID, versionID string) (pin, error) {
	vid, err := uuid.Parse(versionID)
	if err != nil {
		return pin{}, fmt.Errorf("%w: plan_version_id is not a uuid", errInvalidRequest)
	}

	var row struct {
		PlanID    string
		Version   string
		Hash      string
		Content   string
		ApprovedV *string // this plan's currently-approved version id, if any
	}
	err = s.Pool.QueryRow(ctx, `
		select pv.plan_id, pv.version, pv.content_hash, pv.content,
		       (
		         select pv2.id::text
		         from plan_versions pv2
		         where pv2.plan_id = pv.plan_id and pv2.status = 'approved'
		         order by pv2.created_at desc
		         limit 1
		       )
		from plan_versions pv
		join plans p on p.id = pv.plan_id
		where pv.id = $1 and p.app_id = $2`,
		vid, appID).Scan(&row.PlanID, &row.Version, &row.Hash, &row.Content, &row.ApprovedV)
	if errors.Is(err, pgx.ErrNoRows) {
		return pin{}, fmt.Errorf("%w: version %s", ErrUnapprovedPlan, versionID)
	}
	if err != nil {
		return pin{}, fmt.Errorf("pin lookup: %w", err)
	}
	if row.ApprovedV == nil {
		return pin{}, fmt.Errorf("%w: plan %s has no approved versions; queue it in Approvals first",
			ErrZeroApprovedPlans, row.PlanID)
	}
	if *row.ApprovedV != vid.String() {
		return pin{}, fmt.Errorf("%w: version %s is superseded; approved version is %s",
			ErrUnapprovedPlan, versionID, *row.ApprovedV)
	}

	// Zero-step approved versions are rejected here (CONTEXT decision 3) —
	// the same plans.Parse the sync path uses; the engine's own zero-step
	// sentinel semantics stay untouched.
	parsed, err := plans.Parse([]byte(row.Content))
	if err != nil {
		return pin{}, fmt.Errorf("%w: pinned version does not parse: %v", ErrZeroStepPlan, err)
	}
	if len(parsed.Steps) == 0 {
		return pin{}, fmt.Errorf("%w: version %s has zero steps; never silently enqueued", ErrZeroStepPlan, vid)
	}

	selected, err := json.Marshal([]selectedPlanJSON{{
		PlanID: row.PlanID, Version: row.Version, Hash: row.Hash, Source: pinSource, VersionID: vid.String(),
	}})
	if err != nil {
		return pin{}, fmt.Errorf("marshal pinned selection: %w", err)
	}
	resolution, err := json.Marshal(map[string]any{
		"mode":    "manual_pinned",
		"plan_id": row.PlanID,
		"version": row.Version,
	})
	if err != nil {
		return pin{}, fmt.Errorf("marshal pinned resolution: %w", err)
	}
	return pin{
		PlanID: row.PlanID, VersionID: vid.String(), Version: row.Version, Hash: row.Hash,
		VersionUUID:  pgtype.UUID{Bytes: vid, Valid: true},
		SelectedJSON: selected, ResolutionJSON: resolution,
	}, nil
}

// resolveHead applies the mode's SHA sourcing (TRIG-04). reproduce copies the
// source run's head_sha (same-app enforced); retest asks the HeadResolver —
// GitHub-backed, read-only, fail-closed with no stored-run fallback.
func (s *Service) resolveHead(ctx context.Context, req TriggerRequest, a appRow) (string, error) {
	switch req.Mode {
	case ModeRetest:
		sha, err := s.Heads.DefaultBranchHead(ctx, a.Owner, a.Repo)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrHeadResolve, err)
		}
		return sha, nil
	case ModeReproduce:
		var sha string
		err := s.Pool.QueryRow(ctx, `
			select head_sha from runs where id = $1 and app_id = $2`,
			req.SourceRunID, a.ID).Scan(&sha)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: source run %s not found on this app", errInvalidRequest, req.SourceRunID)
		}
		if err != nil {
			return "", fmt.Errorf("source run lookup: %w", err)
		}
		return sha, nil
	default:
		return req.HeadSHA, nil
	}
}

// inFlightRun is the active-run refusal payload surfaced to the operator.
// (Type lives in trigger.go.)

// insertManualRun performs the insert-as-gate (D1/P3): the 0010 partial
// unique index admits at most one in-flight manual run per app, so a racing
// second INSERT's bare `on conflict do nothing` yields no row — surfaced as
// the active-run refusal with the winner's id. Synthetic ids come from the
// dedicated negative sequence; trigger_source and triggered_by are set
// explicitly (sweep.go discipline — the column default would misattribute the
// run as webhook-sourced). Run number 0 per the synthetic policy (RESEARCH D2).
func insertManualRun(ctx context.Context, pool *pgxpool.Pool, tx pgx.Tx, appID pgtype.UUID, headSHA string, req TriggerRequest) (uuid.UUID, int64, *inFlightRun, error) {
	var idText string
	var workflowRunID int64
	err := tx.QueryRow(ctx, `
		insert into runs (app_id, workflow_run_id, run_number, head_sha, head_branch,
		                  environment, trigger_source, triggered_by, status)
		values ($1, -nextval('cockpit_run_ids'), 0, $2, nullif($3, ''), $4,
		        'manual', $5, $6)
		on conflict do nothing
		returning id, workflow_run_id`,
		appID, headSHA, req.HeadBranch, req.Environment, req.actor, string(runs.StatusQueued)).Scan(&idText, &workflowRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Gate tripped: surface the winner's identity for the operator.
		var f inFlightRun
		if err := pool.QueryRow(ctx, `
			select id::text, status
			from runs
			where app_id = $1 and trigger_source = 'manual'
			  and status not in ('completed', 'failed')
			order by created_at desc
			limit 1`,
			appID).Scan(&f.ID, &f.Status); err != nil {
			return uuid.Nil, 0, nil, fmt.Errorf("in-flight manual run lookup after gate: %w", err)
		}
		return uuid.Nil, 0, &f, nil
	}
	if err != nil {
		return uuid.Nil, 0, nil, fmt.Errorf("insert manual run: %w", err)
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return uuid.Nil, 0, nil, fmt.Errorf("parse manual run id %q: %w", idText, err)
	}
	return id, workflowRunID, nil, nil
}
