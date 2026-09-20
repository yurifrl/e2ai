package suite

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Image canary (PLAT-05): a slow-cadence periodic job that exercises a
// candidate runner image through the REAL pinning path — it creates one
// pipeline run and enqueues e2ai:run:execute with ExecuteArgs.Image set to
// the candidate, so the run row is stamped (runner_image) and the suite
// executes in the candidate container exactly like a webhook run would.
//
// The job is inert unless E2AI_CANARY_IMAGE is set (read at fire time, not
// construction), so the wiring in main.go is harmless in every environment
// that hasn't opted into a pin rollout. E2AI_CANARY_APP ("owner/repo")
// optionally pins which registered app the canary run targets; the default
// is the most recently registered app. The run reuses the app's latest head
// SHA (clone the code the pipeline already saw) and skips the rollout gate
// (the app is by definition already deployed).

// CanaryInterval is the canary cadence main.go's periodic wiring uses.
func CanaryInterval() time.Duration { return canaryInterval }

const canaryInterval = 15 * time.Minute

// Env vars the canary reads. Kept here so the binary's .env is the single
// documentation point.
const (
	envCanaryImage = "E2AI_CANARY_IMAGE"
	envCanaryApp   = "E2AI_CANARY_APP"
)

// CanaryArgs is the periodic canary job's payload (stateless).
type CanaryArgs struct{}

func (CanaryArgs) Kind() string { return "e2ai:canary:image" }

// CanaryWorker works the periodic canary job on the default queue.
type CanaryWorker struct {
	river.WorkerDefaults[CanaryArgs]

	Pool *pgxpool.Pool
	// Queue enqueues the pinned execute job. Late-bound in main.go (the
	// worker bundle is built before the River client, same as QueueRef).
	Queue  JobEnqueuer
	Logger *slog.Logger
}

func (w *CanaryWorker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

func (w *CanaryWorker) Work(ctx context.Context, job *river.Job[CanaryArgs]) error {
	logger := w.logger()
	image := os.Getenv(envCanaryImage)
	if image == "" {
		logger.Debug("image canary skipping: E2AI_CANARY_IMAGE unset")
		return nil
	}

	runID, found, err := w.ensureRun(ctx, os.Getenv(envCanaryApp))
	if err != nil {
		return err
	}
	if !found {
		return nil // reason already logged
	}

	if _, err := w.Queue.Insert(ctx, ExecuteArgs{RunID: runID.String(), Image: image}, canaryExecuteOpts()); err != nil {
		return fmt.Errorf("canary execute enqueue: %w", err)
	}
	logger.Info("image canary enqueued pinned execute",
		"image", image, "run_id", runID.String())
	return nil
}

// ensureRun creates the canary run: one row, provisioned status (the
// runner-queue handoff state), pinned to the selected app's latest head.
// found=false means no app matched (or none exist) — a soft skip, unless an
// explicit app filter was given, in which case it's a configuration error.
func (w *CanaryWorker) ensureRun(ctx context.Context, appFilter string) (uuid.UUID, bool, error) {
	logger := w.logger()
	if appFilter != "" {
		var exists bool
		err := w.Pool.QueryRow(ctx, `
			select exists(select 1 from registered_apps where owner || '/' || repo = $1)
		`, appFilter).Scan(&exists)
		if err != nil {
			return uuid.Nil, false, fmt.Errorf("canary app lookup: %w", err)
		}
		if !exists {
			return uuid.Nil, false, fmt.Errorf("E2AI_CANARY_APP %q matches no registered app", appFilter)
		}
	}

	// Negative epoch-millisecond workflow_run_id: canary runs never collide
	// with webhook ids under the (app_id, workflow_run_id) unique key, and
	// are trivially greppable in SQL as run_number = 0.
	var runID uuid.UUID
	err := w.Pool.QueryRow(ctx, `
		with t as (
			select id, environment
			from registered_apps
			where ($1::text is null or owner || '/' || repo = $1)
			order by created_at desc
			limit 1
		)
		insert into runs
			(app_id, workflow_run_id, run_number, head_sha, environment,
			 trigger_source, status, started_at)
		select t.id,
		       -(extract(epoch from clock_timestamp()) * 1000)::bigint,
		       0,
		       coalesce((select r.head_sha from runs r
		                 where r.app_id = t.id
		                 order by r.created_at desc
		                 limit 1), 'HEAD'),
		       t.environment,
		       'reconcile',
		       'provisioning',  -- T7 gate's post-rollout handoff state
		       now()
		from t
		returning id
	`, nullIfEmpty(appFilter)).Scan(&runID)
	if err == pgx.ErrNoRows {
		logger.Info("image canary skipping: no registered apps")
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("canary run insert: %w", err)
	}
	return runID, true, nil
}

// canaryExecuteOpts mirrors ingest's execute handoff: the runner queue
// drains it, one-shot, no racing duplicates.
func canaryExecuteOpts() *river.InsertOpts {
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
