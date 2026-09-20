// Plan sync (APPR-01, PLAN-02): every registered app's declarative plans are
// mirrored into the plans/plan_versions tables under content-hash identity.
// The lifecycle is deliberately asymmetric:
//
//   - a content hash never seen before on a plan that has NEVER been
//     approved lands as `draft` — it is new, and drafts never execute;
//   - a new hash on a plan with an approved ancestor lands as
//     `pending_approval` — the last approved version keeps running until a
//     human approves the change;
//   - a hash already stored is a no-op — sync is idempotent.
//
// Invalid plans (parse or validation failures) are recorded as skipped and
// never stored: the pipeline must never be able to execute a plan that does
// not satisfy the format contract.
//
// The sync runs inside River jobs (e2ai:plans:sync per app, unique in-flight)
// so triggers — report stage after a deploy, boot after seeding, the manual
// REST trigger — all converge on one queue.
package plans

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	plriver "github.com/NSXBet/e2ai/api/internal/platform/river"
	"github.com/goccy/go-yaml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Job kinds worked by this package on the default queue.
const (
	KindSync       = "e2ai:plans:sync"
	KindCorpusSeed = "e2ai:corpus:seed"
)

// SyncArgs carries the registered app whose plans (and corpus seed) to
// refresh. String-carrying because River args round-trip through JSON.
type SyncArgs struct {
	AppID string `json:"app_id"`
}

// Kind implements river.JobArgs.
func (SyncArgs) Kind() string { return KindSync }

// InsertOpts pins the sync one-shot: a racing second enqueue (webhook vs
// manual vs boot) must not double the pass.
func (SyncArgs) InsertOpts() *river.InsertOpts {
	return &river.InsertOpts{UniqueOpts: river.UniqueOpts{
		ByState: []rivertype.JobState{
			rivertype.JobStateScheduled,
			rivertype.JobStatePending,
			rivertype.JobStateAvailable,
			rivertype.JobStateRunning,
		}},
	}
}

// CorpusSeedArgs mirrors SyncArgs for the corpus seeding job (CORP-01): a
// deploy also refreshes the expected-behavior oracle from the app's specs.
type CorpusSeedArgs struct {
	AppID string `json:"app_id"`
}

// Kind implements river.JobArgs.
func (CorpusSeedArgs) Kind() string { return KindCorpusSeed }

// InsertOpts mirrors SyncArgs's one-shot uniqueness.
func (CorpusSeedArgs) InsertOpts() *river.InsertOpts { return SyncArgs{}.InsertOpts() }

// SyncEnqueuer is the narrow seam other stages use to fire a sync (the
// report stage enqueues it best-effort after a deploy's verdict commits).
type SyncEnqueuer interface {
	EnqueueSync(ctx context.Context, appID uuid.UUID) error
}

// QueueRef holds the default-queue River client so stages built before the
// client exists can still enqueue later (mirror of ingest's queue ref).
type QueueRef struct {
	mu     sync.Mutex
	client *plriver.Client
}

// Set installs the client (called once at boot after client construction).
func (r *QueueRef) Set(client *plriver.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.client = client
}

// EnqueueSync fires the per-app plan sync job.
func (r *QueueRef) EnqueueSync(ctx context.Context, appID uuid.UUID) error {
	return r.enqueue(ctx, SyncArgs{AppID: appID.String()})
}

// EnqueueCorpusSeed fires the per-app corpus seeding job.
func (r *QueueRef) EnqueueCorpusSeed(ctx context.Context, appID uuid.UUID) error {
	return r.enqueue(ctx, CorpusSeedArgs{AppID: appID.String()})
}

func (r *QueueRef) enqueue(ctx context.Context, args plriver.JobArgs) error {
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client == nil {
		return errors.New("plans: queue client not ready")
	}
	if _, err := client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("plans: enqueue %s: %w", args.Kind(), err)
	}
	return nil
}

// SkipRecord explains why one plan file was not stored: the path plus the
// parse or validation failure. Skips are sync output, not errors — a bad
// plan must not block the valid ones, but it must be visible.
type SkipRecord struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// SyncResult reports one sync pass over one app's plan source.
type SyncResult struct {
	Synced    int          `json:"synced"`    // plan files read from the source
	Added     int          `json:"added"`     // new plans landing as draft
	Changed   int          `json:"changed"`   // new versions landing as pending_approval
	Unchanged int          `json:"unchanged"` // hashes already stored (no-op)
	Skipped   []SkipRecord `json:"skipped"`   // invalid plans, never stored
}

// SyncApp runs one sync pass for one registered app: walk the declared plan
// source at HEAD, parse + validate every markdown file, and upsert by
// content hash. Validation lookups (app surfaces, corpus behavior status)
// go through the pgx-backed adapters below so plans.Validate stays pure.
func SyncApp(ctx context.Context, pool *pgxpool.Pool, client *github.Client, appID pgtype.UUID, owner, repo string, ps *app.PlansSource) (SyncResult, error) {
	if pool == nil {
		return SyncResult{}, errors.New("plans: nil pool")
	}
	if client == nil {
		return SyncResult{}, errors.New("plans: github client unavailable")
	}
	srcOwner, srcRepo, srcPath, err := resolveSource(owner, repo, ps)
	if err != nil {
		return SyncResult{}, err
	}
	files, err := listPlanFiles(ctx, client, srcOwner, srcRepo, srcPath, "HEAD")
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{Synced: len(files), Skipped: []SkipRecord{}}
	lookups := dbLookups{pool: pool, appID: appID}
	for _, file := range files {
		plan, perr := Parse([]byte(file.Content))
		if perr == nil {
			perr = Validate(plan, lookups, lookups)
		}
		if perr == nil {
			// A plan must live under the app it references: a file synced
			// from app A that declares app B is a source-layout mistake —
			// record it, store nothing.
			refID, ok := lookupPlanApp(ctx, pool, plan.Frontmatter.App)
			if !ok {
				result.Skipped = append(result.Skipped, SkipRecord{Path: file.Path,
					Reason: fmt.Sprintf("frontmatter app %q matches no registered app", plan.Frontmatter.App)})
				continue
			}
			if refID != appID {
				result.Skipped = append(result.Skipped, SkipRecord{Path: file.Path,
					Reason: fmt.Sprintf("plan declares app %q but syncs under a different registered app", plan.Frontmatter.App)})
				continue
			}
			outcome, uerr := upsertPlanVersion(ctx, pool, appID, file.Path, plan, file.Content)
			if uerr != nil {
				return result, fmt.Errorf("plans: store %s: %w", file.Path, uerr)
			}
			switch outcome {
			case "added":
				result.Added++
			case "changed":
				result.Changed++
			default:
				result.Unchanged++
			}
			continue
		}
		result.Skipped = append(result.Skipped, SkipRecord{Path: file.Path, Reason: perr.Error()})
	}
	return result, nil
}

// dbLookups adapts plans.Validate's pure interfaces to pgx. App lookups are
// global (a plan's frontmatter app may be any registered app — validated
// again against the syncing app before storage); behavior lookups are
// scoped to the syncing app's corpus.
type dbLookups struct {
	pool  *pgxpool.Pool
	appID pgtype.UUID
}

// LookupApp resolves an owner/repo (or owner/repo/environment, or workflow
// name) ref to its registered surfaces.
func (l dbLookups) LookupApp(ref string) ([]string, bool) {
	var raw []byte
	err := l.pool.QueryRow(context.Background(), `
		select surfaces from registered_apps
		where owner || '/' || repo = $1
		   or owner || '/' || repo || '/' || environment = $1
		   or workflow_name = $1
		limit 1`, ref).Scan(&raw)
	if err != nil {
		return nil, false
	}
	var surfaces []string
	if err := json.Unmarshal(raw, &surfaces); err != nil || len(surfaces) == 0 {
		return nil, false
	}
	return surfaces, true
}

// BehaviorStatus resolves a corpus behavior key to its review status for the
// syncing app.
func (l dbLookups) BehaviorStatus(key string) (string, bool) {
	var status string
	err := l.pool.QueryRow(context.Background(),
		`select status from corpus_behaviors where app_id = $1 and key = $2`,
		l.appID, key).Scan(&status)
	if err != nil {
		return "", false
	}
	return status, true
}

// lookupPlanApp resolves a frontmatter app ref to the registered app's id
// (same ref grammar as LookupApp).
func lookupPlanApp(ctx context.Context, pool *pgxpool.Pool, ref string) (pgtype.UUID, bool) {
	var id pgtype.UUID
	err := pool.QueryRow(ctx, `
		select id from registered_apps
		where owner || '/' || repo = $1
		   or owner || '/' || repo || '/' || environment = $1
		   or workflow_name = $1
		limit 1`, ref).Scan(&id)
	if err != nil {
		return pgtype.UUID{}, false
	}
	return id, true
}

// upsertPlanVersion stores one valid plan under content-hash identity
// (research §Pattern 4). Returns the outcome: "added" (new plan, draft),
// "changed" (new hash over an approved ancestor, pending_approval), or
// "unchanged" (hash already stored).
func upsertPlanVersion(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, path string, plan *Plan, content string) (string, error) {
	return upsertVersion(ctx, pool, appID, path, plan, content, "", "plans-sync")
}

// UpsertDraftVersion stores one derivation-produced plan version (the draft
// seam the derive package uses, DERV-02). Content is parsed and validated
// on the same code path as sync ingest — app registration, surface subset,
// verified uses: references, the tautology guard — so a draft can never
// bypass the ingest rules. The version always lands with status draft and
// attribution 'derive-draft': pending_approval is reachable only through
// the approval REST flow, never by a machine write (an approved ancestor
// does not pre-approve a new draft). Content-hash identity applies
// unchanged, so re-running a deterministic derivation is a no-op. Returns
// the outcome: "added" | "changed" (new hash over a previously stored
// version) | "unchanged".
func UpsertDraftVersion(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, path, content string) (string, error) {
	plan, err := Parse([]byte(content))
	if err != nil {
		return "", fmt.Errorf("parse draft: %w", err)
	}
	lookups := dbLookups{pool: pool, appID: appID}
	if err := Validate(plan, lookups, lookups); err != nil {
		return "", fmt.Errorf("validate draft: %w", err)
	}
	// The same app-ownership rule sync enforces: a draft must declare the
	// app it stores under.
	if refID, ok := lookupPlanApp(ctx, pool, plan.Frontmatter.App); !ok || refID != appID {
		return "", fmt.Errorf("draft declares app %q but stores under a different registered app", plan.Frontmatter.App)
	}
	return upsertVersion(ctx, pool, appID, path, plan, content, "draft", "derive-draft")
}

// upsertVersion is the shared storage path. status "" infers the sync
// lifecycle (draft, or pending_approval over an approved ancestor); a
// non-empty status is forced through (the derivation draft seam). created_by
// stamps the plan_versions audit column.
func upsertVersion(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, path string, plan *Plan, content, forceStatus, createdBy string) (string, error) {
	hash := ContentHash([]byte(content))
	version := planVersion(plan.Frontmatter.ID)

	// plans row must exist and belong to the syncing app.
	var rowApp pgtype.UUID
	err := pool.QueryRow(ctx, `select app_id from plans where id = $1`, plan.Frontmatter.ID).Scan(&rowApp)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := pool.Exec(ctx, `
			insert into plans (id, app_id, source_path, status) values ($1, $2, $3, 'draft')`,
			plan.Frontmatter.ID, appID, path); err != nil {
			return "", fmt.Errorf("insert plan row: %w", err)
		}
	case err != nil:
		return "", fmt.Errorf("lookup plan row: %w", err)
	case rowApp != appID:
		return "", fmt.Errorf("plan %q already registered under another app", plan.Frontmatter.ID)
	default:
		if _, err := pool.Exec(ctx, `
			update plans set source_path = $2, updated_at = now() where id = $1`,
			plan.Frontmatter.ID, path); err != nil {
			return "", fmt.Errorf("refresh plan row: %w", err)
		}
	}

	// Content-hash identity: an already-stored hash is a no-op.
	var existing string
	err = pool.QueryRow(ctx, `
		select status from plan_versions where plan_id = $1 and content_hash = $2`,
		plan.Frontmatter.ID, hash).Scan(&existing)
	if err == nil {
		return "unchanged", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("lookup version: %w", err)
	}

	// Lifecycle: approved ancestor → the new content waits for approval;
	// never-approved → draft. A forced status (derivation drafts) bypasses
	// the inference entirely — pending_approval is human-only.
	status := forceStatus
	if status == "" {
		status = "draft"
		var approved bool
		err = pool.QueryRow(ctx, `
			select exists (
				select 1 from plan_approvals pa
				join plan_versions pv on pv.id = pa.plan_version_id
				where pv.plan_id = $1 and pa.decision = 'approved')`,
			plan.Frontmatter.ID).Scan(&approved)
		if err != nil {
			return "", fmt.Errorf("check approval ancestry: %w", err)
		}
		if approved {
			status = "pending_approval"
		}
	}

	fm, err := frontmatterJSON(plan.Frontmatter)
	if err != nil {
		return "", fmt.Errorf("marshal frontmatter: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into plan_versions
			(plan_id, version, content_hash, content, frontmatter, status, sources, created_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8)`,
		plan.Frontmatter.ID, version, hash, content, fm, status, jsonStrings(plan.Frontmatter.Sources), createdBy); err != nil {
		return "", fmt.Errorf("insert version: %w", err)
	}
	if _, err := pool.Exec(ctx, `
		update plans set
			latest_version_id = (select id from plan_versions where plan_id = $1 and content_hash = $2),
			status = (select case max(rank)
					when 4 then 'approved'
					when 3 then 'pending'
					when 2 then 'rejected'
					else 'draft' end
				from (select case status
						when 'approved' then 4
						when 'pending_approval' then 3
						when 'superseded' then 3
						when 'rejected' then 2
						else 1 end as rank
					from plan_versions where plan_id = $1) ranks),
			updated_at = now()
		where id = $1`,
		plan.Frontmatter.ID, hash); err != nil {
		return "", fmt.Errorf("refresh plan status: %w", err)
	}
	if status == "draft" {
		return "added", nil
	}
	return "changed", nil
}

// planVersion extracts the semver suffix from the plan id's `-vX.Y.Z` tail.
func planVersion(planID string) string {
	if i := strings.LastIndex(planID, "-v"); i >= 0 {
		return planID[i+2:]
	}
	return "0"
}

// frontmatterJSON renders the parsed frontmatter as the plan_versions
// frontmatter jsonb document (snake_case keys, timeout as its duration
// string, sources carried through).
func frontmatterJSON(fm Frontmatter) ([]byte, error) {
	m := map[string]any{
		"id":       fm.ID,
		"app":      fm.App,
		"title":    fm.Title,
		"surfaces": fm.Surfaces,
		"surface":  fm.Surface,
		"trigger":  fm.Trigger,
		"timeout":  fm.Timeout.D().String(),
	}
	if fm.PRFilter != nil {
		m["pr_filter"] = map[string]any{"paths": fm.PRFilter.Paths}
	}
	if fm.Derived {
		m["derived"] = true
	}
	if len(fm.Sources) > 0 {
		m["sources"] = fm.Sources
	}
	return json.Marshal(m)
}

// jsonStrings guards a string slice into a non-nil JSON value.
func jsonStrings(values []string) []byte {
	if values == nil {
		values = []string{}
	}
	b, _ := json.Marshal(values)
	return b
}

// SyncAppByID runs one sync pass for a registered app id: load the app row,
// parse its plans_source, and sync. Shared by the River worker and the REST
// trigger so manual and queued syncs follow one code path.
func SyncAppByID(ctx context.Context, pool *pgxpool.Pool, client *github.Client, appID pgtype.UUID) (SyncResult, error) {
	var owner, repo string
	var psRaw pgtype.Text
	err := pool.QueryRow(ctx, `select owner, repo, plans_source from registered_apps where id = $1`, appID).
		Scan(&owner, &repo, &psRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return SyncResult{}, ErrNotFound
	}
	if err != nil {
		return SyncResult{}, fmt.Errorf("plans: load app: %w", err)
	}
	if !psRaw.Valid || strings.TrimSpace(psRaw.String) == "" {
		return SyncResult{}, ErrNoPlansSource
	}
	var ps app.PlansSource
	if err := yaml.Unmarshal([]byte(psRaw.String), &ps); err != nil {
		return SyncResult{}, fmt.Errorf("plans: parse plans_source for %s/%s: %w", owner, repo, err)
	}
	return SyncApp(ctx, pool, client, appID, owner, repo, &ps)
}

// SyncWorker works KindSync on the default queue: resolve the app's plans
// source and run one sync pass. A missing plans_source is a logged no-op —
// apps without plans are a registry state, not a failure.
type SyncWorker struct {
	river.WorkerDefaults[SyncArgs]
	Pool   *pgxpool.Pool
	Client *github.Client
	Logger *slog.Logger
}

// Work runs the sync for the job's app.
func (w *SyncWorker) Work(ctx context.Context, job *river.Job[SyncArgs]) error {
	appID, err := uuid.Parse(job.Args.AppID)
	if err != nil {
		return fmt.Errorf("plans: bad app_id %q: %w", job.Args.AppID, err)
	}
	result, err := SyncAppByID(ctx, w.Pool, w.Client, pgUUID(appID))
	switch {
	case errors.Is(err, ErrNotFound):
		w.log().Warn("plans sync: app vanished; skipping", "app_id", appID)
		return nil
	case errors.Is(err, ErrNoPlansSource):
		w.log().Info("plans sync: app has no plans_source; skipping", "app_id", appID)
		return nil
	case err != nil:
		return err
	}
	w.log().Info("plans sync complete",
		"synced", result.Synced, "added", result.Added, "changed", result.Changed,
		"unchanged", result.Unchanged, "skipped", len(result.Skipped))
	return nil
}

// Timeout bounds one sync pass (contents-API walk + per-file fetches).
func (w *SyncWorker) Timeout(*river.Job[SyncArgs]) time.Duration { return 5 * time.Minute }

func (w *SyncWorker) log() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

// CorpusSeedWorker works KindCorpusSeed: refresh the app's expected-behavior
// oracle from its Playwright specs at HEAD (CORP-01). The corpus root falls
// back to the house default specs dir.
type CorpusSeedWorker struct {
	river.WorkerDefaults[CorpusSeedArgs]
	Pool   *pgxpool.Pool
	Client *github.Client
	Logger *slog.Logger
}

// Work runs the spec seeding for the job's app.
func (w *CorpusSeedWorker) Work(ctx context.Context, job *river.Job[CorpusSeedArgs]) error {
	appID, err := uuid.Parse(job.Args.AppID)
	if err != nil {
		return fmt.Errorf("corpus seed: bad app_id %q: %w", job.Args.AppID, err)
	}
	var owner, repo string
	var root pgtype.Text
	err = w.Pool.QueryRow(ctx, `select owner, repo, corpus_root from registered_apps where id = $1`, pgUUID(appID)).Scan(&owner, &repo, &root)
	if errors.Is(err, pgx.ErrNoRows) {
		w.log().Warn("corpus seed: app vanished; skipping", "app_id", appID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("corpus seed: load app: %w", err)
	}
	dir := corpus.DefaultSpecsDir
	if root.Valid && strings.TrimSpace(root.String) != "" {
		dir = strings.TrimSpace(root.String)
	}
	summary, err := corpus.SeedSpecs(ctx, w.Pool, w.Client, pgUUID(appID), owner, repo, dir, "HEAD")
	if err != nil {
		return fmt.Errorf("corpus seed: %s/%s: %w", owner, repo, err)
	}
	w.log().Info("corpus seed complete", "app", owner+"/"+repo,
		"scanned", summary.FilesScanned, "behaviors", summary.Behaviors)
	return nil
}

// Timeout bounds one seeding pass.
func (w *CorpusSeedWorker) Timeout(*river.Job[CorpusSeedArgs]) time.Duration { return 5 * time.Minute }

func (w *CorpusSeedWorker) log() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

// AddWorkers registers this package's workers on a shared river workers
// bundle (the api server runs ingest + plans workers on one client).
func AddWorkers(workers *plriver.Workers, pool *pgxpool.Pool, client *github.Client, logger *slog.Logger) {
	plriver.AddWorker(workers, &SyncWorker{Pool: pool, Client: client, Logger: logger})
	plriver.AddWorker(workers, &CorpusSeedWorker{Pool: pool, Client: client, Logger: logger})
}
