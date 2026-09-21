// Edge sweeps (EDGE-01, Phase 5 T4): a periodic tick that keeps opted-in
// apps under continuous edge exploration. Cadence config lives in the app
// registry (0007 columns edge_sweep_enabled / edge_sweep_interval_minutes /
// edge_sweep_step_ceiling — per-app config in DB, not env), and due-ness is
// computed in SQL, so the tick itself is stateless: it only starts work for
// apps whose interval has elapsed since their last sweep run.
//
// A sweep is a real run (trigger_source 'sweep') plus one queued
// edge_hunter session carrying the app's step ceiling; the loop itself runs
// in agentrunner (Phase 5 T3). Findings stay in corpus learnings — human
// promotion only, report-only posture.
package agents

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// edgesweepInterval is the tick cadence wired in main.go. Due-ness is per-app
// in SQL, so a fast tick with slow apps is the intended shape — the tick
// only bounds how long an app waits past its own interval.
const edgesweepInterval = 60 * time.Second

// EdgesweepInterval is main.go's periodic-job accessor for edgesweepInterval (distinct from the orphan sweep's SweepInterval).
func EdgesweepInterval() time.Duration { return edgesweepInterval }

// DefaultSweepStepCeiling bounds an edge_hunter session when the app has
// not configured edge_sweep_step_ceiling. Sweeps are unattended loops, so
// "unset" must never mean the budget engine's unlimited (0): a stuck
// explorer would run forever.
const DefaultSweepStepCeiling = 30

// EdgesweepArgs is the periodic tick job on the default queue.
type EdgesweepArgs struct{}

// Kind implements river.JobArgs.
func (EdgesweepArgs) Kind() string { return "e2ai:edgesweeps:tick" }

// EdgesweepWorker starts sweep runs for due apps on every tick.
type EdgesweepWorker struct {
	river.WorkerDefaults[EdgesweepArgs]
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Work implements river.Worker.
func (w *EdgesweepWorker) Work(ctx context.Context, job *river.Job[EdgesweepArgs]) error {
	started, err := SweepDueApps(ctx, w.Pool)
	if err != nil {
		return fmt.Errorf("edge sweep tick: %w", err)
	}
	if started > 0 {
		w.Logger.Info("edge sweep tick started sweep runs", "apps", started)
	}
	return nil
}

// dueApp is one registered app whose sweep window has elapsed.
type dueApp struct {
	ID          pgtype.UUID
	StepCeiling int
}

// SweepDueApps starts one sweep run + edge_hunter session per opted-in app
// whose edge_sweep_interval_minutes has elapsed since its last sweep run.
// Apps that are disabled, have no cadence, or swept within their interval
// are skipped. Returns the number of sweep runs started.
//
// The due check is advisory (read-then-insert): two ticks racing could both
// see an app due, but the 60s cadence against minute-scale intervals makes
// that unreachable in practice, and a duplicated sweep is a wasted bounded
// session, not corruption — the append-only log keeps both honestly.
func SweepDueApps(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := pool.Query(ctx, `
		select id, coalesce(edge_sweep_step_ceiling, $1::int)
		from registered_apps
		where edge_sweep_enabled
		  and coalesce(edge_sweep_interval_minutes, 0) > 0
		  and not exists (
			select 1 from runs
			where runs.app_id = registered_apps.id
			  and runs.trigger_source = 'sweep'
			  and runs.created_at > now()
			      - (registered_apps.edge_sweep_interval_minutes * interval '1 minute')
		  )
	`, DefaultSweepStepCeiling)
	if err != nil {
		return 0, fmt.Errorf("edge sweep: query due apps: %w", err)
	}
	defer rows.Close()

	var due []dueApp
	for rows.Next() {
		var a dueApp
		if err := rows.Scan(&a.ID, &a.StepCeiling); err != nil {
			return 0, fmt.Errorf("edge sweep: scan due app: %w", err)
		}
		due = append(due, a)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("edge sweep: iterate due apps: %w", err)
	}

	started := 0
	for _, app := range due {
		if err := startSweepRun(ctx, pool, app); err != nil {
			return started, fmt.Errorf("edge sweep: start sweep for app %s: %w", uuidString(app.ID), err)
		}
		started++
	}
	return started, nil
}

// startSweepRun creates the sweep run and its edge_hunter session in one
// transaction. workflow_run_id is the negated wall-clock millisecond:
// unique per (app_id, workflow_run_id) across ticks and disjoint from real
// GitHub workflow-run ids, which are positive. The run enters 'executing'
// directly — sweeps have no rollout/provisioning stages — which keeps it
// rollout-healthy for the spawn gate the agent-session worker enforces.
// Finalization (report/verdict of sweep runs) is the sweep driver's job
// (T3) once the hunter's session completes.
func startSweepRun(ctx context.Context, pool *pgxpool.Pool, app dueApp) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin sweep transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var nextNumber int
	if err := tx.QueryRow(ctx,
		`select coalesce(max(run_number), 0) + 1 from runs where app_id = $1`, app.ID,
	).Scan(&nextNumber); err != nil {
		return fmt.Errorf("read next run number: %w", err)
	}

	var runID pgtype.UUID
	err = tx.QueryRow(ctx, `
		insert into runs (app_id, workflow_run_id, run_number, head_sha, environment, trigger_source, status, started_at)
		select id, $2, $3, 'sweep', environment, 'sweep', 'executing', now()
		from registered_apps where id = $1
		returning id
	`, app.ID, -time.Now().UnixMilli(), nextNumber).Scan(&runID)
	if err != nil {
		return fmt.Errorf("insert sweep run: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		insert into agent_sessions (run_id, role, step_ceiling)
		values ($1, $2, $3)
	`, runID, RoleEdgeHunter, app.StepCeiling); err != nil {
		return fmt.Errorf("insert edge_hunter session: %w", err)
	}

	return tx.Commit(ctx)
}
