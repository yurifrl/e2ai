// Orphan sweep (AGNT-02): a worker that dies mid-session stops heart-
// beating, so sessions left in claimed|running past the lapse threshold are
// orphans. A periodic River job marks each orphan failed (error_note says
// so — the append-only log keeps the dead attempt) and requeues a fresh
// queued session for the same run + role with the same ceilings.
package agents

import (
	"context"
	"fmt"
	"log/slog"

	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// OrphanThreshold: a claimed/running session whose heartbeat lapsed this
// long is orphaned. Beats any legit long step (screenshot + LLM < 30s
// typical, RESEARCH pitfalls).
const OrphanThreshold = 90 * time.Second

// sweepInterval is the sweep cadence for main.go's periodic wiring.
const sweepInterval = 30 * time.Second

// SweepInterval exposes the sweep cadence.
func SweepInterval() time.Duration { return sweepInterval }

// SweepArgs is the periodic sweep job's payload (stateless).
type SweepArgs struct{}

// Kind implements river.JobArgs.
func (SweepArgs) Kind() string { return "e2ai:agent:sweep" }

// SweepWorker runs one orphan-recovery pass per tick.
type SweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Work executes one pass.
func (w *SweepWorker) Work(ctx context.Context, job *river.Job[SweepArgs]) error {
	recovered, err := SweepOrphans(ctx, w.Pool)
	if err != nil {
		return fmt.Errorf("orphan sweep: %w", err)
	}
	if recovered > 0 {
		w.Logger.Info("orphan sweep recovered lapsed agent sessions", "recovered", recovered)
	}
	return nil
}

// SweepOrphans marks every claimed/running session whose heartbeat lapsed
// past OrphanThreshold as failed ('orphaned' error_note) and inserts a
// fresh queued session for the same run/role with identical ceilings. One
// CTE does both halves atomically: the UPDATE returning feeds the INSERT.
// Returns the number of orphans recovered.
func SweepOrphans(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tag, err := pool.Exec(ctx, `
		with orphans as (
			update agent_sessions set
				status = 'failed',
				finished_at = now(),
				error_note = coalesce(error_note || ' | ', '') || 'orphaned: heartbeat lapsed beyond 90s'
			where status in ('claimed', 'running')
			  and coalesce(heartbeat_at, created_at) < now() - interval '90 seconds'
			returning run_id, role, step_ceiling, cost_ceiling_millicents
		)
		insert into agent_sessions (run_id, role, step_ceiling, cost_ceiling_millicents)
		select run_id, role, step_ceiling, cost_ceiling_millicents from orphans
	`)
	if err != nil {
		return 0, fmt.Errorf("orphan sweep: %w", err)
	}
	// RowsAffected counts the inserted requeues — one per orphan.
	return int(tag.RowsAffected()), nil
}
