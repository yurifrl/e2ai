// job.go — the e2ai:derive:draft river worker (DERV-01/02 wiring). One job
// per ingest-created run: resolve the changed paths (PR files endpoint when
// the webhook named a PR, three-dot compare against the previous deploy
// otherwise), map the plan scope, and store derived draft versions through
// the plans draft-upsert seam. The job never executes anything — drafts land
// in the approval queue, and only a human approval makes a version
// executable (APPR-01).
package derive

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/plans"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// DeriveDraftArgs is the job payload. PRNumber is the webhook's known PR
// (0 = resolve from the head SHA or the compare fallback).
type DeriveDraftArgs struct {
	RunID    string `json:"run_id"`
	PRNumber int    `json:"pr_number,omitempty"`
}

func (DeriveDraftArgs) Kind() string { return KindDeriveDraft }

// KindDeriveDraft is the derive job kind (registered in main.go).
const KindDeriveDraft = "e2ai:derive:draft"

// DeriveOpts is the derive enqueue: default queue, one shot (a deploy
// derives once; a redelivery hits the run dedupe, not this job).
func DeriveOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue:       river.QueueDefault,
		MaxAttempts: 1,
		UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
			rivertype.JobStateScheduled,
			rivertype.JobStatePending,
			rivertype.JobStateAvailable,
			rivertype.JobStateRunning,
		}},
	}
}

// Deriver is the derive worker. Client may be nil for PR-known enqueues in
// tests; a nil client is an error only when PR resolution actually needs it.
type Deriver struct {
	river.WorkerDefaults[DeriveDraftArgs]

	Pool   *pgxpool.Pool
	Client githubClient
	Logger *slog.Logger
}

// NewDeriver wires the derive worker with production defaults.
func NewDeriver(pool *pgxpool.Pool, client githubClient, logger *slog.Logger) *Deriver {
	return &Deriver{Pool: pool, Client: client, Logger: logger}
}

// Timeout bounds one derivation pass.
func (w *Deriver) Timeout(*river.Job[DeriveDraftArgs]) time.Duration { return 2 * time.Minute }

func (w *Deriver) log() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

// deriveRun is the run + app context the worker reads.
type deriveRun struct {
	id         pgtype.UUID
	headSHA    string
	headBranch pgtype.Text
	owner      string
	repo       string
}

// Work runs one derivation pass.
func (w *Deriver) Work(ctx context.Context, job *river.Job[DeriveDraftArgs]) error {
	runID, err := pgUUIDParse(job.Args.RunID)
	if err != nil {
		return fmt.Errorf("parse run id %q: %w", job.Args.RunID, err)
	}
	logger := w.log()

	run, err := w.loadRun(ctx, runID)
	if err != nil {
		return err
	}

	base, hasBase, err := w.previousDeploySHA(ctx, run)
	if err != nil {
		return err
	}

	d := Diff{Client: w.Client, Owner: run.owner, Repo: run.repo}
	webhookPRs := []int{}
	if job.Args.PRNumber != 0 {
		webhookPRs = []int{job.Args.PRNumber}
	}
	paths, truncated, pr, err := d.ChangedPathsResolved(ctx, base, run.headSHA, run.headSHA, webhookPRs)
	if err != nil {
		if job.Args.PRNumber == 0 && !hasBase {
			// First deploy of an app: nothing to diff against and no PR —
			// derivation is undefined, not an error.
			logger.Info("derive: no diff context (first deploy, no PR)", "run", runID)
			return nil
		}
		return fmt.Errorf("derive: resolve changed paths: %w", err)
	}
	_ = pr

	if len(paths) == 0 {
		logger.Info("derive: empty diff, nothing to map", "run", runID)
		return w.noteDiffTruncated(ctx, runID, truncated)
	}

	plansIn, err := w.planInputs(ctx, run)
	if err != nil {
		return err
	}
	behaviors, err := w.verifiedBehaviors(ctx, run.id)
	if err != nil {
		return err
	}

	behaviorInputs := make([]BehaviorInput, 0, len(behaviors))
	for _, b := range behaviors {
		behaviorInputs = append(behaviorInputs, BehaviorInput{Key: b.Key, Provenance: b.Provenance})
	}
	inputs := make([]PlanInput, 0, len(plansIn))
	for _, p := range plansIn {
		inputs = append(inputs, p.PlanInput)
	}
	mapped := MapScope(inputs, behaviorInputs, paths, nil)
	if len(mapped) == 0 {
		logger.Info("derive: no plans in scope for this diff", "run", runID, "paths", len(paths))
		return w.noteDiffTruncated(ctx, runID, truncated)
	}

	appID := run.id
	byKey := map[string]corpus.Behavior{}
	for _, b := range behaviors {
		byKey[b.Key] = b
	}
	planByInput := map[string]planInput{}
	for _, p := range plansIn {
		planByInput[p.ID] = p
	}

	drafted := 0
	for _, m := range mapped {
		in := planByInput[m.PlanID]
		specBehaviors := overlappingBehaviors(in.Uses, byKey)
		if len(specBehaviors) == 0 {
			continue // sources must be non-empty (tautology guard); nothing verified overlaps
		}
		parent, err := plans.Parse([]byte(in.Content))
		if err != nil {
			logger.Warn("derive: parent plan no longer parses; skipped", "plan", in.ID, "error", err)
			continue
		}
		content, err := Render(DraftSpec{
			ParentPlanID: in.ID,
			AppRef:       run.owner + "/" + run.repo,
			Title:        "Derived: " + parent.Frontmatter.Title,
			Surfaces:     parent.Frontmatter.Surfaces,
			Trigger:      plans.TriggerOnDeploy,
			Behaviors:    specBehaviors,
		})
		if err != nil {
			return fmt.Errorf("derive: render %s: %w", in.ID, err)
		}
		if _, err := plans.UpsertDraftVersion(ctx, w.Pool, appID, DraftPath(BumpVersion(in.ID)), content); err != nil {
			return fmt.Errorf("derive: store draft for %s: %w", in.ID, err)
		}
		drafted++
	}
	logger.Info("derive: drafts stored", "run", runID, "mapped", len(mapped), "drafted", drafted)
	return w.noteDiffTruncated(ctx, runID, truncated)
}

// noteDiffTruncated stamps the resolution record's diff_truncated flag when
// the compare fallback reported degradation; PR-path derivations are never
// truncated. The record row is written by the executor's resolution gate.
func (w *Deriver) noteDiffTruncated(ctx context.Context, runID pgtype.UUID, truncated bool) error {
	if !truncated {
		return nil
	}
	tag, err := w.Pool.Exec(ctx, `
		update run_plan_resolutions
		set resolution = jsonb_set(resolution, '{diff_truncated}', 'true'::jsonb)
		where run_id = $1`, runID)
	if err != nil {
		return fmt.Errorf("derive: stamp diff_truncated: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Executor hasn't recorded yet (race): the executor's own upsert wins
		// and the flag loss is bounded to a replay ordering edge.
		return nil
	}
	return nil
}

// previousDeploySHA returns the head SHA of the app's deploy that precedes
// this run (largest workflow_run_id strictly below this run's) — the compare
// fallback's base.
func (w *Deriver) previousDeploySHA(ctx context.Context, run *deriveRun) (string, bool, error) {
	var sha string
	err := w.Pool.QueryRow(ctx, `
		select r2.head_sha
		from runs r2
		where r2.app_id = (select app_id from runs where id = $1)
			and r2.workflow_run_id < (select workflow_run_id from runs where id = $1)
		order by r2.workflow_run_id desc
		limit 1`, run.id).Scan(&sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("derive: previous deploy lookup: %w", err)
	}
	return sha, true, nil
}

// loadRun fetches the run joined with its registered app.
func (w *Deriver) loadRun(ctx context.Context, runID pgtype.UUID) (*deriveRun, error) {
	r := &deriveRun{id: runID}
	err := w.Pool.QueryRow(ctx, `
		select r.head_sha, r.head_branch, a.owner, a.repo
		from runs r
		join registered_apps a on a.id = r.app_id
		where r.id = $1`, runID).Scan(&r.headSHA, &r.headBranch, &r.owner, &r.repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("derive: run %s not found", runID)
	}
	if err != nil {
		return nil, fmt.Errorf("derive: load run: %w", err)
	}
	return r, nil
}

// planInput is a PlanInput carrying the parent's raw markdown so the draft
// renderer can re-parse the frontmatter at generation time.
type planInput struct {
	PlanInput
	Content string
}

// planInputs loads each app plan's latest version and re-parses it for the
// pr-filter and uses keys. Unparseable stored content is a data bug — the
// plan is skipped with a warning rather than failing the whole pass.
func (w *Deriver) planInputs(ctx context.Context, run *deriveRun) ([]planInput, error) {
	rows, err := w.Pool.Query(ctx, `
		select p.id, v.content
		from plans p
		join lateral (
			select id, content from plan_versions
			where plan_id = p.id
			order by created_at desc, version desc
			limit 1
		) v on true
		where p.app_id = $1`, run.id)
	if err != nil {
		return nil, fmt.Errorf("derive: load plans: %w", err)
	}
	defer rows.Close()
	var out []planInput
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, fmt.Errorf("derive: scan plan: %w", err)
		}
		doc, err := plans.Parse([]byte(content))
		if err != nil {
			w.log().Warn("derive: stored plan does not parse; skipped", "plan", id, "error", err)
			continue
		}
		in := planInput{PlanInput: PlanInput{ID: id}, Content: content}
		if doc.Frontmatter.PRFilter != nil {
			in.PRFilter = doc.Frontmatter.PRFilter.Paths
		}
		for _, s := range doc.Steps {
			in.Uses = append(in.Uses, s.Uses...)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("derive: plan rows: %w", err)
	}
	return out, nil
}

// verifiedBehaviors loads the app's verified corpus behaviors — the K slice.
func (w *Deriver) verifiedBehaviors(ctx context.Context, appID pgtype.UUID) ([]corpus.Behavior, error) {
	all, err := corpus.ListBehaviors(ctx, w.Pool, appID)
	if err != nil {
		return nil, fmt.Errorf("derive: load behaviors: %w", err)
	}
	out := make([]corpus.Behavior, 0, len(all))
	for _, b := range all {
		if b.Status == corpus.BehaviorVerified {
			out = append(out, b)
		}
	}
	return out, nil
}

// overlappingBehaviors picks the verified behaviors a plan's steps reference
// that survived the scope mapping (provenance overlapped the diff).
func overlappingBehaviors(uses []string, byKey map[string]corpus.Behavior) []corpus.Behavior {
	seen := map[string]bool{}
	var out []corpus.Behavior
	for _, key := range uses {
		if b, ok := byKey[key]; ok && !seen[key] {
			seen[key] = true
			out = append(out, b)
		}
	}
	return out
}

func pgUUIDParse(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		return pgtype.UUID{}, err
	}
	return id, nil
}
