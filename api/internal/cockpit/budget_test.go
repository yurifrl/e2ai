// Budget gate tests run against a real scratch Postgres (same
// E2AI_TEST_DATABASE_URL harness) so the layered cap resolution, the
// day-scoped spend aggregate, and the typed zero-ceiling distinction are
// exercised against actual rows — including the boundary case the engine's
// 0-sentinel convention forbids: 0 here means "no spend permitted", never
// "unlimited".
package cockpit

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var budgetSeq atomic.Int64

// budgetApp inserts one registered_apps row with the given cap (nil = NULL
// — no app-level cap) and returns its id.
func budgetApp(t *testing.T, pool *pgxpool.Pool, cap *int64) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment, display_title_pattern,
			 webhook_secret_ref, comment_marker, budget_cap_millicents)
		values ($1, 'budget-app', 'Release - Web', '.github/workflows/budget.yml', 'test',
			'[Budget] Deploy', 'op://x/secret', '<!-- e2ai:run:budget -->', $2)
		returning id`,
		"budget-owner-"+strconv.FormatInt(budgetSeq.Add(1), 10), cap).Scan(&id)
	if err != nil {
		t.Fatalf("seed app fixture: %v", err)
	}
	return id
}

// budgetRun seeds one runs row for app, returning its id.
func budgetRun(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, environment, head_sha)
		values ($1, $2, 1, 'test', 'deadbeef') returning id`,
		appID, 77_000_000+budgetSeq.Add(1)).Scan(&id)
	if err != nil {
		t.Fatalf("seed run fixture: %v", err)
	}
	return id
}

// budgetSession seeds one completed agent session with the given cost at the
// given created_at (day-window control).
func budgetSession(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, costMillicents int64, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into agent_sessions (run_id, role, status, cost_millicents, created_at)
		values ($1, 'navigator', 'completed', $2, $3)`,
		runID, costMillicents, createdAt); err != nil {
		t.Fatalf("seed session fixture: %v", err)
	}
}

func TestBudgetZeroCeilingRefuses(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Explicit zero cap: a refusal candidate with no spend at all — never
	// unlimited, unlike the engine's 0-sentinel session ceilings.
	appID := budgetApp(t, pool, new(int64(0)))
	v, err := Evaluate(ctx, pool, appID)
	if err != nil {
		t.Fatalf("evaluate zero cap: %v", err)
	}
	if v.Cap.Kind != CapApp || v.Cap.Millicents != 0 || !v.Cap.Zero() {
		t.Fatalf("cap mismatch: %+v", v.Cap)
	}
	if !v.Refused || !strings.Contains(v.Reason, "zero") {
		t.Fatalf("zero cap must refuse via the zero branch, got %+v", v)
	}

	// Env floor zero on an app without its own cap: same refusal, env kind.
	envApp := budgetApp(t, pool, nil)
	t.Setenv(EnvDefaultCapMillicents, "0")
	v, err = Evaluate(ctx, pool, envApp)
	if err != nil {
		t.Fatalf("evaluate env-zero cap: %v", err)
	}
	if v.Cap.Kind != CapEnvFloor || !v.Cap.Zero() || !v.Refused {
		t.Fatalf("env-zero cap mismatch: %+v", v)
	}
}

func TestBudgetUncappedNeverRefuses(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// No app cap, env floor explicitly empty: the gate is open even under
	// real spend — no-cap is never a refusal.
	appID := budgetApp(t, pool, nil)
	runID := budgetRun(t, pool, appID)
	budgetSession(t, pool, runID, 123_456, time.Now().UTC())
	t.Setenv(EnvDefaultCapMillicents, "")
	v, err := Evaluate(ctx, pool, appID)
	if err != nil {
		t.Fatalf("evaluate uncapped: %v", err)
	}
	if v.Cap.Capped() || v.Refused || v.Reason != "" {
		t.Fatalf("uncapped app must never refuse: %+v", v)
	}
	if v.Spend != 123_456 {
		t.Fatalf("uncapped verdict must still carry spend: %+v", v)
	}
}

func TestBudgetDayScopeAndBreach(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cap := new(int64(10_000))
	appID := budgetApp(t, pool, cap)
	runID := budgetRun(t, pool, appID)

	// Day window boundaries: today's UTC midnight exactly (included) and one
	// microsecond earlier (excluded).
	todayMidnight := time.Now().UTC().Truncate(24 * time.Hour)
	budgetSession(t, pool, runID, 999_999, todayMidnight.Add(-time.Microsecond)) // yesterday
	budgetSession(t, pool, runID, 5_000, todayMidnight)                          // today

	spend, err := DaySpend(ctx, pool, appID)
	if err != nil {
		t.Fatalf("day spend: %v", err)
	}
	if spend != 5_000 {
		t.Fatalf("day window excluded yesterday's 999999, want 5000, got %d", spend)
	}

	v, err := Evaluate(ctx, pool, appID)
	if err != nil {
		t.Fatalf("evaluate under cap: %v", err)
	}
	if v.Refused || v.Cap.Kind != CapApp || v.Cap.Millicents != 10_000 || v.Spend != 5_000 || v.Remaining != 5_000 {
		t.Fatalf("under-cap verdict mismatch: %+v", v)
	}

	// Breach: a second today session crosses the cap — refusal carries the
	// remaining-budget reason (CONTEXT decision 1) and a floored remainder.
	budgetSession(t, pool, runID, 5_000, todayMidnight.Add(time.Hour))
	v, err = Evaluate(ctx, pool, appID)
	if err != nil {
		t.Fatalf("evaluate over cap: %v", err)
	}
	if !v.Refused || v.Spend != 10_000 || v.Remaining != 0 || !strings.Contains(v.Reason, "remaining") {
		t.Fatalf("over-cap verdict mismatch: %+v", v)
	}

	// App isolation: spend under another app does not leak into this one.
	otherApp := budgetApp(t, pool, new(int64(10_000)))
	otherRun := budgetRun(t, pool, otherApp)
	budgetSession(t, pool, otherRun, 42_000, todayMidnight)
	spend, err = DaySpend(ctx, pool, appID)
	if err != nil {
		t.Fatalf("day spend after other app: %v", err)
	}
	if spend != 10_000 {
		t.Fatalf("other app's spend leaked: want 10000, got %d", spend)
	}
}

func TestBudgetCapForEnvGarbage(t *testing.T) {
	pool := testPool(t)
	appID := budgetApp(t, pool, nil)
	t.Setenv(EnvDefaultCapMillicents, "not-a-number")
	if _, err := CapFor(context.Background(), pool, appID); err == nil {
		t.Fatalf("garbage env floor must error, not silently pass")
	}
}
