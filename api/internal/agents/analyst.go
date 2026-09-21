// Flake Analyst trigger (FLAK-01/02, Phase 5 T4): a periodic tick that
// compares each app's flake rates against its configured alert threshold
// (0007 flake_alert_threshold). A crossing fires a durable alert event
// (internal/flakes alert store — dedupe-gated per cooldown) and enqueues a
// flake_analyst session on the app's latest run. The analyst reads the
// flake-rate API and PROPOSES quarantine candidates as records; activation
// stays an admin-gated HTTP action — the agent never quarantines (report
// -only posture, human-gated).
package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/NSXBet/e2ai/api/internal/flakes"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// flakesInterval is the tick cadence wired in main.go — the same shape as
// the edge-sweep tick: stateless tick, per-app state in SQL.
const flakesInterval = 60 * time.Second

// FlakesTickInterval is main.go's periodic-job accessor for flakesInterval.
func FlakesTickInterval() time.Duration { return flakesInterval }

// analystStepCeiling bounds a flake_analyst session. The analyst is an
// unattended bounded read-and-propose loop, not a plan-bounded navigator,
// so "unset" must never mean unlimited (0 per the budget engine).
const analystStepCeiling = 20

// FlakesTickArgs is the periodic tick job on the default queue.
type FlakesTickArgs struct{}

// Kind implements river.JobArgs.
func (FlakesTickArgs) Kind() string { return "e2ai:flakes:tick" }

// FlakesTickWorker checks flake thresholds on every tick.
type FlakesTickWorker struct {
	river.WorkerDefaults[FlakesTickArgs]
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Work implements river.Worker.
func (w *FlakesTickWorker) Work(ctx context.Context, job *river.Job[FlakesTickArgs]) error {
	res, err := FlakesTick(ctx, w.Pool)
	if err != nil {
		return fmt.Errorf("flakes tick: %w", err)
	}
	if res.Alerts > 0 || res.Sessions > 0 {
		w.Logger.Info("flakes tick fired",
			"alerts", res.Alerts, "analyst_sessions", res.Sessions, "apps_checked", res.AppsChecked)
	}
	return nil
}

// FlakesTickResult summarizes one tick for the worker's log line.
type FlakesTickResult struct {
	AppsChecked int
	Alerts      int
	Sessions    int
}

// FlakesTick is the tick body: for every app with a flake_alert_threshold,
// compute flake rates and act on the first crossing. Returns a summary.
func FlakesTick(ctx context.Context, pool *pgxpool.Pool) (*FlakesTickResult, error) {
	res := &FlakesTickResult{}
	rows, err := pool.Query(ctx,
		`select id, flake_alert_threshold from registered_apps where flake_alert_threshold is not null`)
	if err != nil {
		return nil, fmt.Errorf("flakes tick: query threshold apps: %w", err)
	}
	defer rows.Close()

	type thresholdApp struct {
		ID        pgtype.UUID
		Threshold float64
	}
	var apps []thresholdApp
	for rows.Next() {
		var a thresholdApp
		if err := rows.Scan(&a.ID, &a.Threshold); err != nil {
			return nil, fmt.Errorf("flakes tick: scan threshold app: %w", err)
		}
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("flakes tick: iterate threshold apps: %w", err)
	}
	rows.Close()

	for _, app := range apps {
		res.AppsChecked++
		rates, err := flakes.FlakeRates(ctx, pool, app.ID, flakes.DefaultWindow)
		if err != nil {
			return res, fmt.Errorf("flakes tick: flake rates for app %s: %w", uuidString(app.ID), err)
		}
		var worst *flakes.Rate
		for i := range rates {
			if worst == nil || rates[i].Rate > worst.Rate {
				worst = &rates[i]
			}
		}
		// Strictly above the threshold crosses it; a rate exactly at the
		// threshold is the configured tolerance, not an incident.
		if worst == nil || worst.Rate <= app.Threshold {
			continue
		}

		payload, err := json.Marshal(map[string]any{
			"max_rate":  worst.Rate,
			"threshold": app.Threshold,
			"window":    worst.WindowRuns,
			"test":      map[string]any{"file": worst.File, "title": worst.Title, "rate": worst.Rate},
		})
		if err != nil {
			return res, fmt.Errorf("flakes tick: build alert payload: %w", err)
		}
		inserted, err := flakes.InsertFlakeThresholdAlert(ctx, pool, app.ID, payload)
		if err != nil {
			return res, fmt.Errorf("flakes tick: insert alert for app %s: %w", uuidString(app.ID), err)
		}
		if inserted {
			res.Alerts++
		}

		enqueued, err := EnqueueFlakeAnalyst(ctx, pool, app.ID)
		if err != nil {
			return res, fmt.Errorf("flakes tick: enqueue analyst for app %s: %w", uuidString(app.ID), err)
		}
		if enqueued {
			res.Sessions++
		}
	}
	return res, nil
}

// EnqueueFlakeAnalyst enqueues one queued flake_analyst session for the
// app, attached to the app's most recent run (a session needs a run FK; the
// analyst reads rates across the window, the run is only the anchor).
// Suppressed while an active flake_analyst session from within
// flakes.AlertCooldownHours exists, so a persistently crossed threshold
// cannot stack duplicate analysts. Reports whether a session landed.
func EnqueueFlakeAnalyst(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) (bool, error) {
	tag, err := pool.Exec(ctx, `
		insert into agent_sessions (run_id, role, step_ceiling)
		select latest.id, $2, $3
		from (
			select id from runs where app_id = $1
			order by created_at desc limit 1
		) latest
		where not exists (
			select 1 from agent_sessions s
			join runs r on r.id = s.run_id
			where r.app_id = $1
			  and s.role = $2
			  and s.status in ('queued', 'claimed', 'running')
			  and s.created_at > now() - make_interval(hours => $4::int)
		)
	`, appID, RoleFlakeAnalyst, analystStepCeiling, flakes.AlertCooldownHours)
	if err != nil {
		return false, fmt.Errorf("enqueue flake analyst: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
