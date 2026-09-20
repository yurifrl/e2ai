// Package report is the api-side pipeline tail (T9/T10): the
// e2ai:run:report job that derives the run verdict from the test evidence
// the runner stored, persists the verdict + summary counts + duration, and
// transitions the run to completed (or failed on internal error).
//
// T9 ships the job wrapper, the verdict persistence and the state
// transition, plus a Publisher seam where T10 will mount comment /
// check-run / report-data assembly. The publisher is nil until T10: the
// job stays report-only and never mutates the app under test.
package report

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/NSXBet/e2ai/api/internal/suite"
	"github.com/NSXBet/e2ai/api/internal/verdict"
)

// JobKindReport mirrors suite.JobKindReport — the runner enqueues the handoff
// under that kind; this package is the api-side owner of the payload shape.
const JobKindReport = suite.JobKindReport

// ReportArgs is the report handoff payload (json-shape twin of suite's
// reportArgs: {"run_id": "<uuid string>"}).
type ReportArgs struct {
	RunID string `json:"run_id"` // uuid string
}

func (ReportArgs) Kind() string { return JobKindReport }

// InsertOpts are this job's defaults: default queue (this process drains it),
// MaxAttempts 3 so T10's GitHub publishing survives 5xx bursts via River's
// backoff. Note: insert-time opts set by the runner (suite.reportOpts) take
// precedence until T8 aligns its enqueue.
func (ReportArgs) InsertOpts() *river.InsertOpts {
	return &river.InsertOpts{
		Queue:       river.QueueDefault,
		MaxAttempts: 3,
	}
}

// ReportPublisher is the T10 seam: comment / check-run / report-data
// assembly mounts here once the verdict is committed. Nil in T9.
type ReportPublisher interface {
	Publish(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, rep verdict.Report) error
}

// Worker is the api-side e2ai:run:report worker (VERD-01, SUIT-02).
type Worker struct {
	river.WorkerDefaults[ReportArgs]

	Pool *pgxpool.Pool

	// Logger receives stage transitions; nil uses slog.Default().
	Logger *slog.Logger

	// Publisher is the T10 handoff; nil disables it (T9, tests).
	Publisher ReportPublisher

	// Sync is the plans seam (T4): a deploy's verdict triggers a plan
	// catalog sync so new declarative plans land as drafts. Best-effort —
	// nil disables it (tests), failure is logged, never re-fails the run.
	Sync SyncEnqueuer
}

// SyncEnqueuer is the narrow seam the plans queue ref satisfies; defined
// here so report never imports the plans package.
type SyncEnqueuer interface {
	EnqueueSync(ctx context.Context, appID uuid.UUID) error
}

func (w *Worker) logger() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// evidenceRow is the minimal test_results projection the verdict engine
// needs: outcome (test-level enum), status (last attempt), attempt count.
type evidenceRow struct {
	title      string
	file       string
	outcome    string
	status     string
	attempts   int32
	durationMS int32
}

// Work runs the report job:
//
//	executing → reporting (guarded; replay-tolerant)
//	→ load test_results → rebuild per-test evidence → verdict.Derive
//	→ one tx: verdict + summary counts + duration, then reporting → completed
//	→ publisher handoff (T10; nil in T9)
func (w *Worker) Work(ctx context.Context, job *river.Job[ReportArgs]) error {
	runID, err := uuid.Parse(job.Args.RunID)
	if err != nil {
		return fmt.Errorf("report: parse run id %q: %w", job.Args.RunID, err)
	}
	logger := w.logger().With("run_id", runID.String())

	id := toPgUUID(runID)
	if err := w.enterReporting(ctx, id); err != nil {
		return err
	}

	rows, err := w.loadEvidence(ctx, runID)
	if err != nil {
		return w.fail(ctx, id, "evidence load", err, logger)
	}
	rep := verdict.Derive(verdict.RunInput{Stage: "execute", Results: evidence(rows)})

	if err := w.commit(ctx, id, runID, rep, totalDuration(rows)); err != nil {
		return w.fail(ctx, id, "verdict persist", err, logger)
	}

	logger.Info("run reported",
		"verdict", rep.Verdict,
		"counts", rep.Counts,
		"duration_ms", totalDuration(rows))

	// Plan catalog sync (T4): a completed run means the deploy the run
	// exercised is observable, so fire the per-app sync so new/changed
	// declarative plans land in the catalog (drafts never execute). The
	// sync is content-hash idempotent, so firing on every report is cheap;
	// failure is logged, never re-fails the completed run.
	if w.Sync != nil {
		if appID, err := w.runApp(ctx, runID); err != nil {
			logger.Warn("plans sync enqueue skipped", "reason", err)
		} else if err := w.Sync.EnqueueSync(ctx, appID); err != nil {
			logger.Warn("plans sync enqueue failed", "app_id", appID, "reason", err)
		}
	}

	if w.Publisher != nil {
		if err := w.Publisher.Publish(ctx, w.Pool, runID, rep); err != nil {
			// The run is already completed and its verdict committed; the
			// publisher only adds sugar on top (comments, report link), so a
			// failure here must not re-fail the run. River retries the job,
			// whose replay path is a no-op.
			return fmt.Errorf("report: publish: %w", err)
		}
	}
	return nil
}

// enterReporting performs the executing → reporting move, tolerating a
// replayed job (the stage already moved) and a run that already finished
// (terminal states have no outgoing edges — the prior attempt committed).
func (w *Worker) enterReporting(ctx context.Context, id pgtype.UUID) error {
	err := runs.Transition(ctx, w.Pool, id, runs.StatusReporting)
	if err == nil {
		return nil
	}
	var terr *runs.TransitionError
	if errors.As(err, &terr) {
		switch terr.From {
		case runs.StatusReporting:
			return nil // replayed: the stage already moved
		case runs.StatusCompleted, runs.StatusFailed:
			w.logger().Info("report: run already finished; replay is a no-op",
				"run_id", id, "status", terr.From)
			return nil
		}
	}
	return fmt.Errorf("report: enter reporting: %w", err)
}

// loadEvidence reads the run's test_results in stable order (file, title).
func (w *Worker) loadEvidence(ctx context.Context, runID uuid.UUID) ([]evidenceRow, error) {
	rows, err := w.Pool.Query(ctx, `
		select title, file, outcome, status, attempts, duration_ms
		from test_results
		where run_id = $1
		order by file, title
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("query test results: %w", err)
	}
	defer rows.Close()

	var out []evidenceRow
	for rows.Next() {
		var r evidenceRow
		var file pgtype.Text
		if err := rows.Scan(&r.title, &file, &r.outcome, &r.status,
			&r.attempts, &r.durationMS); err != nil {
			return nil, fmt.Errorf("scan test result: %w", err)
		}
		r.file = file.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate test results: %w", err)
	}
	return out, nil
}

// evidence rebuilds per-test verdict evidence from the stored rows. The
// runner persisted the collapsed view (outcome + last-attempt status +
// attempt count); retry rows that Playwright folded into "flaky"/retried
// outcomes are reconstructed as failed earlier attempts, which is the only
// evidence shape the verdict engine's rules accept for them.
func evidence(rows []evidenceRow) []verdict.TestEvidence {
	out := make([]verdict.TestEvidence, 0, len(rows))
	for _, r := range rows {
		var atts []verdict.Attempt
		// Earlier attempts: with retries, every attempt before the last one
		// ran and failed (an earlier pass would have ended the test).
		for i := int32(0); i < r.attempts-1; i++ {
			atts = append(atts, verdict.Attempt{Retry: int(i), Status: verdict.AttemptFailed})
		}
		if r.status != "" {
			atts = append(atts, verdict.Attempt{Retry: int(r.attempts - 1), Status: r.status})
		}
		out = append(out, verdict.TestEvidence{
			Title:    r.title,
			File:     r.file,
			Status:   r.outcome,
			Attempts: atts,
		})
	}
	return out
}

// commit persists the verdict, summary counts and total duration, then
// completes the run — one transaction, so the run never shows a verdict
// while still reporting (and finished_at stamps via the transition).
func (w *Worker) commit(ctx context.Context, id pgtype.UUID, runID uuid.UUID, rep verdict.Report, durationMS int64) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin commit tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		update runs set
			verdict = $2,
			total_tests = $3,
			passed = $4,
			failed = $5,
			flaky = $6,
			skipped = $7,
			duration_ms = $8
		where id = $1
	`, runID, lower(rep.Verdict),
		rep.Counts.Total(),
		rep.Counts.Pass,
		rep.Counts.Fail,
		rep.Counts.Flaky,
		rep.Counts.Inconclusive+rep.Counts.InfraError,
		durationMS,
	); err != nil {
		return fmt.Errorf("persist verdict: %w", err)
	}

	if err := runs.Transition(ctx, tx, id, runs.StatusCompleted); err != nil {
		var terr *runs.TransitionError
		if errors.As(err, &terr) && terr.From == runs.StatusCompleted {
			// Replayed job: the prior attempt already completed the run.
			// Overwriting identical evidence is harmless; treat as done.
			return tx.Commit(ctx)
		}
		return fmt.Errorf("transition completed: %w", err)
	}
	return tx.Commit(ctx)
}

// fail transitions the run to failed with a stage-tagged note (VERD-01:
// internal errors are never silent). Transition failures (a concurrent
// replay already failed or completed the run) are logged, not returned —
// the underlying job error takes precedence.
func (w *Worker) fail(ctx context.Context, id pgtype.UUID, stage string, cause error, logger *slog.Logger) error {
	err := runs.Transition(ctx, w.Pool, id, runs.StatusFailed)
	if err != nil {
		logger.Warn("report: fail transition not applied", "from_error", err)
	}
	if _, err := w.Pool.Exec(ctx, `update runs set error_note = $1 where id = $2`,
		fmt.Sprintf("report %s: %v", stage, cause), id); err != nil {
		logger.Warn("report: error note persist failed", "error", err)
	}
	logger.Error("report job failed", "stage", stage, "error", cause)
	return fmt.Errorf("report %s: %w", stage, cause)
}

// totalDuration sums the stored per-test durations.
func totalDuration(rows []evidenceRow) int64 {
	var sum int64
	for _, r := range rows {
		sum += int64(r.durationMS)
	}
	return sum
}

// lower maps the verdict enum to the runs.verdict column vocabulary
// (lowercase: pass|fail|flaky|infra_error|inconclusive).
func lower(v verdict.Verdict) string {
	switch v {
	case verdict.VerdictPass:
		return "pass"
	case verdict.VerdictFail:
		return "fail"
	case verdict.VerdictFlaky:
		return "flaky"
	case verdict.VerdictInfraError:
		return "infra_error"
	case verdict.VerdictInconclusive:
		return "inconclusive"
	default:
		return string(v)
	}
}

// toPgUUID converts a uuid.UUID to the pgtype the runs package expects.
func toPgUUID(id uuid.UUID) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(id.String())
	return u
}

// runApp resolves the owning app of a run (the plans sync target).
func (w *Worker) runApp(ctx context.Context, runID uuid.UUID) (uuid.UUID, error) {
	var appID pgtype.UUID
	if err := w.Pool.QueryRow(ctx,
		`select app_id from runs where id = $1`, toPgUUID(runID)).Scan(&appID); err != nil {
		return uuid.Nil, fmt.Errorf("report: load run app: %w", err)
	}
	if !appID.Valid {
		return uuid.Nil, fmt.Errorf("report: run %s has no app_id", runID)
	}
	return uuid.UUID(appID.Bytes), nil
}
