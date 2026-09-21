// Edge sweep + flake-analyst trigger tests (Phase 5 T4): the periodic tick
// against the scratch Postgres with the real 0007 registry columns —
// enabled apps get a sweep run + edge_hunter session, disabled apps don't,
// and a crossed flake threshold fires one alert + one analyst session per
// cooldown.
package agents

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedHardenedApp inserts a registry row with the 0007 sweep + threshold
// columns set. threshold nil leaves flake_alert_threshold unset.
func seedHardenedApp(t *testing.T, pool *pgxpool.Pool, repo string, sweepEnabled bool, intervalMinutes int, stepCeiling int, threshold *float64) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker,
			 edge_sweep_enabled, edge_sweep_interval_minutes,
			 edge_sweep_step_ceiling, flake_alert_threshold)
		values ('yuri', $1, 'e2ai', '.github/workflows/e2ai.yml', 'prod',
			 '*', 'ref', 'marker', $2, $3, $4, $5)
		returning id
	`, repo, sweepEnabled, intervalMinutes, stepCeiling, threshold).Scan(&id)
	if err != nil {
		t.Fatalf("seed app %s: %v", repo, err)
	}
	return id
}

// seedVerdictRun inserts a completed run with the given verdict.
func seedVerdictRun(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, verdict string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, head_sha, environment, status, verdict, finished_at)
		values ($1, $2, 1, 'abc123', 'prod', 'completed', $3, now())
		returning id
	`, appID, uuid.New().ID(), verdict).Scan(&id)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

// seedTestResult inserts one test result row on the run.
func seedTestResult(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, title, outcome string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		insert into test_results (run_id, title, file, outcome, status)
		values ($1, $2, 'tests/login.spec.ts', $3, $4)
	`, runID, title, outcome, "passed")
	if err != nil {
		t.Fatalf("seed test result: %v", err)
	}
}

// Enabled app produces exactly one sweep run (trigger_source 'sweep',
// status 'executing') + one edge_hunter session with the app's configured
// step ceiling; a second tick inside the interval starts nothing; a
// disabled app never produces a sweep.
func TestSweepDueApps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	enabled := seedHardenedApp(t, pool, "yuri/sweep-on", true, 60, 12, nil)
	seedHardenedApp(t, pool, "yuri/sweep-off", false, 60, 12, nil)

	started, err := SweepDueApps(ctx, pool)
	if err != nil {
		t.Fatalf("SweepDueApps: %v", err)
	}
	if started != 1 {
		t.Fatalf("SweepDueApps = %d started, want 1 (only the enabled app)", started)
	}

	var triggerSource, runStatus string
	var runNumber int
	if err := pool.QueryRow(ctx, `
		select trigger_source, status, run_number from runs where app_id = $1
	`, enabled).Scan(&triggerSource, &runStatus, &runNumber); err != nil {
		t.Fatalf("read sweep run: %v", err)
	}
	if triggerSource != "sweep" {
		t.Errorf("trigger_source = %q, want sweep", triggerSource)
	}
	if runStatus != "executing" {
		t.Errorf("status = %q, want executing (sweeps skip rollout stages)", runStatus)
	}

	var role, sessionStatus string
	var ceiling int
	if err := pool.QueryRow(ctx, `
		select s.role, s.status, s.step_ceiling
		from agent_sessions s join runs r on r.id = s.run_id
		where r.app_id = $1
	`, enabled).Scan(&role, &sessionStatus, &ceiling); err != nil {
		t.Fatalf("read edge_hunter session: %v", err)
	}
	if role != string(RoleEdgeHunter) {
		t.Errorf("role = %q, want edge_hunter", role)
	}
	if sessionStatus != string(StatusQueued) {
		t.Errorf("session status = %q, want queued", sessionStatus)
	}
	if ceiling != 12 {
		t.Errorf("step ceiling = %d, want 12 (app-configured)", ceiling)
	}

	// Within the 60-minute interval: the second tick starts nothing.
	started, err = SweepDueApps(ctx, pool)
	if err != nil {
		t.Fatalf("second SweepDueApps: %v", err)
	}
	if started != 0 {
		t.Fatalf("second SweepDueApps = %d, want 0 inside the interval", started)
	}

	// Disabled app: no sweep run, no edge_hunter session.
	var n int
	if err := pool.QueryRow(ctx, `
		select count(*) from agent_sessions s
		join runs r on r.id = s.run_id
		join registered_apps a on a.id = r.app_id
		where a.repo = 'yuri/sweep-off'
	`).Scan(&n); err != nil {
		t.Fatalf("count disabled-app sessions: %v", err)
	}
	if n != 0 {
		t.Errorf("disabled app produced %d edge_hunter sessions, want 0", n)
	}
}

// A flake rate above the app's alert threshold fires exactly one alert
// event + one flake_analyst session per cooldown; a second tick inside the
// cooldown is fully suppressed.
func TestFlakesTickThresholdCrossing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	threshold := 0.25
	appID := seedHardenedApp(t, pool, "yuri/flaky-app", false, 60, 12, &threshold)
	// 5 verdict-bearing runs; 'login flow' flaky in 4 → rate 0.8 > 0.25.
	for i := range 5 {
		runID := seedVerdictRun(t, pool, appID, "pass")
		outcome := "flaky"
		if i == 4 {
			outcome = "expected"
		}
		seedTestResult(t, pool, runID, "login flow", outcome)
	}

	res, err := FlakesTick(ctx, pool)
	if err != nil {
		t.Fatalf("FlakesTick: %v", err)
	}
	if res.AppsChecked != 1 {
		t.Errorf("AppsChecked = %d, want 1", res.AppsChecked)
	}
	if res.Alerts != 1 {
		t.Fatalf("Alerts = %d, want 1", res.Alerts)
	}
	if res.Sessions != 1 {
		t.Fatalf("Sessions = %d, want 1", res.Sessions)
	}

	var kind string
	var acknowledged bool
	var payload map[string]any
	if err := pool.QueryRow(ctx, `
		select kind, acknowledged, payload from alert_events where app_id = $1
	`, appID).Scan(&kind, &acknowledged, &payload); err != nil {
		t.Fatalf("read alert event: %v", err)
	}
	if kind != "flake_threshold" {
		t.Errorf("alert kind = %q, want flake_threshold", kind)
	}
	if acknowledged {
		t.Error("fresh alert must be unacknowledged")
	}
	if payload["threshold"] != threshold {
		t.Errorf("payload threshold = %v, want %v", payload["threshold"], threshold)
	}
	if payload["max_rate"] != 0.8 {
		t.Errorf("payload max_rate = %v, want 0.8", payload["max_rate"])
	}

	var role, sessionStatus string
	var ceiling int
	if err := pool.QueryRow(ctx, `
		select s.role, s.status, s.step_ceiling
		from agent_sessions s join runs r on r.id = s.run_id
		where r.app_id = $1
	`, appID).Scan(&role, &sessionStatus, &ceiling); err != nil {
		t.Fatalf("read flake_analyst session: %v", err)
	}
	if role != string(RoleFlakeAnalyst) {
		t.Errorf("role = %q, want flake_analyst", role)
	}
	if sessionStatus != string(StatusQueued) {
		t.Errorf("session status = %q, want queued", sessionStatus)
	}
	if ceiling != 20 {
		t.Errorf("analyst step ceiling = %d, want 20", ceiling)
	}

	// Second tick inside the cooldown: the alert is dedupe-suppressed and
	// the queued analyst suppresses a second session.
	res, err = FlakesTick(ctx, pool)
	if err != nil {
		t.Fatalf("second FlakesTick: %v", err)
	}
	if res.Alerts != 0 || res.Sessions != 0 {
		t.Fatalf("second tick = %+v, want no alerts and no sessions inside the cooldown", res)
	}
	var alerts, sessions int
	if err := pool.QueryRow(ctx, `select count(*) from alert_events where app_id = $1`, appID).Scan(&alerts); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		select count(*) from agent_sessions s join runs r on r.id = s.run_id
		where r.app_id = $1 and s.role = $2
	`, appID, RoleFlakeAnalyst).Scan(&sessions); err != nil {
		t.Fatalf("count analyst sessions: %v", err)
	}
	if alerts != 1 || sessions != 1 {
		t.Errorf("after two ticks: %d alerts, %d analyst sessions, want 1 and 1", alerts, sessions)
	}
}

// A rate exactly at the configured threshold is the tolerance, not an
// incident — no alert, no analyst.
func TestFlakesTickAtThresholdNoCross(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	threshold := 0.8
	appID := seedHardenedApp(t, pool, "yuri/at-threshold", false, 60, 12, &threshold)
	for i := range 5 {
		runID := seedVerdictRun(t, pool, appID, "pass")
		outcome := "flaky"
		if i == 4 {
			outcome = "expected"
		}
		seedTestResult(t, pool, runID, "login flow", outcome)
	}

	res, err := FlakesTick(ctx, pool)
	if err != nil {
		t.Fatalf("FlakesTick: %v", err)
	}
	if res.Alerts != 0 || res.Sessions != 0 {
		t.Fatalf("tick at threshold = %+v, want no alert and no session", res)
	}
}
