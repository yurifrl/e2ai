// Rollout gate (INGE-03): a deploy completing is not the app being live.
// The wait-rollout job polls the registered app's health_url until it answers
// 2xx — replacing bet-app's Checkly-era blind `sleep 300` — then hands the run
// to the runner queue. Apps without a health_url fall back to the fixed 5m
// wait (house precedent). A gate that never becomes healthy fails the run as
// INFRA_ERROR so a dead rollout never blocks on a missing suite.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/NSXBet/e2ai/api/internal/suite"
	"github.com/NSXBet/e2ai/api/internal/verdict"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// pgtypeUUID maps the google uuid job args onto runs.Transition's pgtype.UUID.
func pgtypeUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// Production gate tuning: poll every 2s for at most 15 minutes of health
// checks; health_url-less apps wait the historical fixed 5 minutes.
const (
	rolloutBudget       = 15 * time.Minute
	rolloutPollInterval = 2 * time.Second
	rolloutFallbackWait = 5 * time.Minute
)

// probeTimeout bounds each individual health request so a half-open
// connection can't eat the whole polling window.
const probeTimeout = 5 * time.Second

// executeOpts keeps the handoff one-shot and single-run: the runner queue is
// drained by a different container, and a racing second enqueue (webhook vs
// sweep) must not double the suite.
func executeOpts() *river.InsertOpts {
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

// WaitRolloutWorker works KindWaitRollout on the default queue: transition the
// run to waiting-rollout, poll health_url (or fall back to a fixed wait) until
// the rollout is live or the budget expires, then either transition to
// provisioning and enqueue the execute job on the runner queue, or fail the
// run as INFRA_ERROR.
type WaitRolloutWorker struct {
	river.WorkerDefaults[WaitRolloutArgs]

	pool         *pgxpool.Pool
	queue        JobEnqueuer
	http         *http.Client
	budget       time.Duration
	pollInterval time.Duration
	fallbackWait time.Duration
	logger       *slog.Logger
}

// NewWaitRolloutWorker wires the gate with production tuning; tests override
// the exported timing fields directly.
func NewWaitRolloutWorker(pool *pgxpool.Pool, queue JobEnqueuer, logger *slog.Logger) *WaitRolloutWorker {
	return &WaitRolloutWorker{
		pool:         pool,
		queue:        queue,
		http:         &http.Client{Timeout: probeTimeout},
		budget:       rolloutBudget,
		pollInterval: rolloutPollInterval,
		fallbackWait: rolloutFallbackWait,
		logger:       logger,
	}
}

// Timeout keeps a single gate job from squatting on the default queue past
// its own budget; the worker's deadline is authoritative, this is the belt.
func (w *WaitRolloutWorker) Timeout(*river.Job[WaitRolloutArgs]) time.Duration {
	return w.budget + probeTimeout
}

// Work runs one gate.
func (w *WaitRolloutWorker) Work(ctx context.Context, job *river.Job[WaitRolloutArgs]) error {
	id := pgtypeUUID(job.Args.RunID)

	// The gate only advances freshly ingested runs; a run already past the
	// gate (replayed job after a crash between transition and enqueue) is a
	// no-op — the execute enqueue's unique opts own idempotence from there.
	var status runs.Status
	var healthURL *string
	if err := w.pool.QueryRow(ctx, `
		select r.status, a.health_url
		from runs r join registered_apps a on a.id = r.app_id
		where r.id = $1
	`, id).Scan(&status, &healthURL); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			w.logger.Warn("wait-rollout: run row missing", "run_id", job.Args.RunID)
			return nil
		}
		return fmt.Errorf("wait-rollout: load run %s: %w", job.Args.RunID, err)
	}

	switch status {
	case runs.StatusQueued:
		if err := runs.Transition(ctx, w.pool, id, runs.StatusWaitingRollout); err != nil {
			return fmt.Errorf("wait-rollout: enter waiting-rollout: %w", err)
		}
	case runs.StatusWaitingRollout:
		// Resume of a replayed job; keep polling from wherever it stands.
	default:
		w.logger.Info("wait-rollout: run already past the gate", "run_id", job.Args.RunID, "status", status)
		return nil
	}

	if healthURL == nil || *healthURL == "" {
		w.logger.Info("wait-rollout: no health_url; falling back to fixed wait",
			"run_id", job.Args.RunID, "wait", w.fallbackWait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.fallbackWait):
		}
	} else if !w.awaitHealth(ctx, *healthURL) {
		return w.failGate(ctx, job.Args.RunID)
	}

	if err := runs.Transition(ctx, w.pool, id, runs.StatusProvisioning); err != nil {
		return fmt.Errorf("wait-rollout: enter provisioning: %w", err)
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("wait-rollout: begin handoff tx: %w", err)
	}
	if _, err := w.queue.InsertTx(ctx, tx, suite.ExecuteArgs{RunID: job.Args.RunID.String()}, executeOpts()); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("wait-rollout: enqueue execute: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("wait-rollout: commit handoff tx: %w", err)
	}
	return nil
}

// awaitHealth polls health_url until it answers 2xx, the budget expires, or
// the job context is cancelled. Network and non-2xx responses are retried:
// a rollout in progress answers 503s, not errors worth aborting on.
func (w *WaitRolloutWorker) awaitHealth(ctx context.Context, healthURL string) bool {
	deadline := time.Now().Add(w.budget)
	for {
		if w.probe(ctx, healthURL) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(w.pollInterval):
		}
	}
}

// probe reports whether the health endpoint currently answers 2xx.
func (w *WaitRolloutWorker) probe(ctx context.Context, healthURL string) bool {
	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// failGate records the gate timeout as an infra failure: verdict + error_note
// first (the evidence trail, VERD-01), then the guarded transition to failed,
// which stamps finished_at. The job itself returns nil — the run is failed,
// the timeout was reported, River must not retry a one-shot gate.
func (w *WaitRolloutWorker) failGate(ctx context.Context, runID uuid.UUID) error {
	note := KindWaitRollout + ": health_url never returned a healthy response within the rollout gate budget"
	if _, err := w.pool.Exec(ctx,
		`update runs set verdict = $1, error_note = $2 where id = $3`,
		verdict.VerdictInfraError, note, pgtypeUUID(runID)); err != nil {
		return fmt.Errorf("wait-rollout: record gate timeout: %w", err)
	}
	if err := runs.Transition(ctx, w.pool, pgtypeUUID(runID), runs.StatusFailed); err != nil {
		return fmt.Errorf("wait-rollout: fail run after gate timeout: %w", err)
	}
	w.logger.Warn("wait-rollout: rollout never became healthy; run failed", "run_id", runID)
	return nil
}
