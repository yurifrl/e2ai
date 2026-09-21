// Transactional ingest path (INGE-01/02): one tx inserts the webhook_events
// delivery row (dedupe on X-GitHub-Delivery), the runs row (dedupe on
// app + workflow run id), and enqueues the wait-rollout job. Everything
// commits together or not at all — a crash between webhook receipt and job
// enqueue leaves nothing half-built for the pipeline to trip over.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/derive"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Trigger sources (runs.trigger_source).
const (
	TriggerWebhook   = "webhook"
	TriggerReconcile = "reconcile"
	TriggerSweep     = "sweep" // edge-sweep triggered runs (EDGE-01, 0007)
)

// Result is the outcome of one ingest attempt.
type Result int

const (
	// ResultAccepted: fresh delivery and run row, wait-rollout job enqueued.
	ResultAccepted Result = iota
	// ResultRecorded: delivery recorded without a run (filtered payload).
	ResultRecorded
	// ResultDuplicateEvent: this delivery GUID was already ingested.
	ResultDuplicateEvent
	// ResultDuplicateRun: this workflow run already has a run row (same run
	// redelivered under a new GUID, or the sweep and webhook raced).
	ResultDuplicateRun
)

// String renders the HTTP-facing status word.
func (r Result) String() string {
	switch r {
	case ResultAccepted:
		return "accepted"
	case ResultRecorded:
		return "recorded"
	case ResultDuplicateEvent, ResultDuplicateRun:
		return "already-ingested"
	default:
		return fmt.Sprintf("unknown(%d)", int(r))
	}
}

// JobEnqueuer is the transactional enqueue surface of a River client —
// satisfied by river.Client[pgx.Tx].
type JobEnqueuer interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// QueueRef is a late-bound JobEnqueuer for main.go's wiring order: the worker
// bundle is built before the River client (the client config takes the
// bundle), while the worker needs the client to enqueue. Set binds the client
// immediately after construction.
type QueueRef struct {
	mu     sync.Mutex
	client JobEnqueuer
}

// Set binds the River client.
func (q *QueueRef) Set(client JobEnqueuer) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.client = client
}

// InsertTx implements JobEnqueuer by delegating to the bound client.
func (q *QueueRef) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	q.mu.Lock()
	client := q.client
	q.mu.Unlock()
	if client == nil {
		return nil, errors.New("river client not bound to ingest.QueueRef")
	}
	return client.InsertTx(ctx, tx, args, opts)
}

// RunSeed is one ingest attempt's inputs: the matched registered app plus the
// workflow-run facts. Enqueue=false records the delivery without a run row
// (filtered payload kept for the evidence trail).
type RunSeed struct {
	App            app.App
	DeliveryID     string
	WorkflowRunID  int64
	RunNumber      int
	HeadSHA        string
	HeadBranch     string
	PRNumber       int    // PR attached to the workflow_run; 0 = none (derive falls back to head-SHA resolution)
	TriggerSource  string // TriggerWebhook | TriggerReconcile
	SignatureValid bool
	Payload        []byte
	Enqueue        bool
}

// Store owns the transactional ingest path shared by the webhook handler and
// the reconciliation sweep.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore binds the ingest path to the api's pgx pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Ingest applies one seed through the dedupe path. Duplicate deliveries and
// duplicate runs short-circuit to ResultDuplicate* without enqueuing a second
// wait-rollout job.
func (s *Store) Ingest(ctx context.Context, seed RunSeed, queue JobEnqueuer) (Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin ingest tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	appID, err := resolveAppID(ctx, tx, seed.App)
	if err != nil {
		return 0, err
	}

	eventInserted, err := insertEvent(ctx, tx, appID, seed)
	if err != nil {
		return 0, err
	}
	if !eventInserted {
		return ResultDuplicateEvent, nil
	}
	if !seed.Enqueue {
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit ingest tx: %w", err)
		}
		return ResultRecorded, nil
	}

	runID, runInserted, err := insertRun(ctx, tx, appID, seed)
	if err != nil {
		return 0, err
	}
	if !runInserted {
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit ingest tx: %w", err)
		}
		return ResultDuplicateRun, nil
	}
	if _, err := queue.InsertTx(ctx, tx, WaitRolloutArgs{RunID: runID}, waitRolloutOpts()); err != nil {
		return 0, fmt.Errorf("enqueue %s: %w", KindWaitRollout, err)
	}
	if seed.PRNumber > 0 {
		// The deploy touched a PR (DERV-01): enqueue the derive job in the
		// same tx so run creation and draft derivation enqueue commit
		// atomically. When the PR is unknown the job's own head-SHA
		// resolution covers it (reconcile seeds carry PRNumber 0).
		if _, err := queue.InsertTx(ctx, tx,
			derive.DeriveDraftArgs{RunID: runID.String(), PRNumber: seed.PRNumber},
			derive.DeriveOpts()); err != nil {
			return 0, fmt.Errorf("enqueue %s: %w", derive.KindDeriveDraft, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit ingest tx: %w", err)
	}
	return ResultAccepted, nil
}

// resolveAppID maps the registry-triplet app identity to its
// registered_apps id (App carries no surrogate id).
func resolveAppID(ctx context.Context, tx pgx.Tx, a app.App) (uuid.UUID, error) {
	var idText string
	if err := tx.QueryRow(ctx, `
		select id from registered_apps
		where owner = $1 and repo = $2 and workflow_name = $3 and environment = $4
	`, a.Owner, a.Repo, a.WorkflowName, a.Environment).Scan(&idText); err != nil {
		return uuid.Nil, fmt.Errorf("resolve app %s/%s %q (%s): %w",
			a.Owner, a.Repo, a.WorkflowName, a.Environment, err)
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return uuid.Nil, fmt.Errorf("parse app id %q: %w", idText, err)
	}
	return id, nil
}

// insertEvent records the delivery GUID. on-conflict-nothing keeps a
// redelivery from double-counting; RowsAffected reports first-insert.
func insertEvent(ctx context.Context, tx pgx.Tx, appID uuid.UUID, seed RunSeed) (bool, error) {
	tag, err := tx.Exec(ctx, `
		insert into webhook_events (app_id, delivery_id, workflow_run_id, signature_valid, payload)
		values ($1, $2, $3, $4, $5)
		on conflict (app_id, delivery_id) do nothing
	`, appID, seed.DeliveryID, seed.WorkflowRunID, seed.SignatureValid, string(seed.Payload))
	if err != nil {
		return false, fmt.Errorf("insert webhook event %q: %w", seed.DeliveryID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// insertRun creates the pipeline's run row. on-conflict-nothing on
// (app_id, workflow_run_id) is the run-level dedupe gate.
func insertRun(ctx context.Context, tx pgx.Tx, appID uuid.UUID, seed RunSeed) (uuid.UUID, bool, error) {
	var idText string
	err := tx.QueryRow(ctx, `
		insert into runs (
			app_id, workflow_run_id, run_number, head_sha, head_branch,
			environment, trigger_source
		)
		values ($1, $2, $3, $4, nullif($5, ''), $6, $7)
		on conflict (app_id, workflow_run_id) do nothing
		returning id
	`, appID, seed.WorkflowRunID, seed.RunNumber, seed.HeadSHA, seed.HeadBranch,
		seed.App.Environment, seed.TriggerSource).Scan(&idText)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("insert run %d: %w", seed.WorkflowRunID, err)
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("parse run id %q: %w", idText, err)
	}
	return id, true, nil
}
