// Feed, KPI, per-app budget, and agent-rail read-model tests run against a
// real scratch Postgres (the E2AI_TEST_DATABASE_URL harness): keyset
// pagination, the UTC-day window boundary, and the >90s stuck predicate
// are structural properties of actual SQL — a mocked pool would only test
// the mock. testPool self-skips when the scratch database is absent.
package cockpit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/approvals"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// feedService builds a read-model-only Service: the Feed/KPIs/Budget/Rail
// methods touch the pool (and Logger on the HTTP 500 path) only — no river
// client, heads resolver, ingest store, or secret resolver needed.
func feedService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()
	return &Service{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// utcTodaySeed returns a timestamp guaranteed inside the current UTC-day
// window [midnight, now): 45 minutes ago, or 1ms after midnight when 45
// minutes back crosses into the previous day (early-UTC-morning runs). The
// KPIs and DaySpend windows are [UTC-midnight, now) — future stamps would
// be silently excluded, so the boundary case must never overshoot now.
func utcTodaySeed() time.Time {
	now := time.Now().UTC()
	t := now.Add(-45 * time.Minute)
	if t.Day() != now.Day() || t.Month() != now.Month() || t.Year() != now.Year() {
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(time.Millisecond)
	}
	return t
}

// utcYesterdaySeed is always inside the previous UTC day.
func utcYesterdaySeed() time.Time {
	return time.Now().UTC().Add(-26 * time.Hour)
}

// feedRun seeds one runs row with an explicit status/verdict/created_at and
// a globally unique workflow_run_id (the (app_id, workflow_run_id) unique
// index forbids the budget helper's constant id in bulk); returns its id.
func feedRun(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, status, verdict string, createdAt time.Time) pgtype.UUID {
	t.Helper()
	var verdictArg any
	if verdict != "" {
		verdictArg = verdict
	}
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, head_sha, environment,
		                  trigger_source, status, verdict, created_at)
		values ($1, $2, 1, 'feed-test', 'test', 'webhook', $3, $4, $5)
		returning id`,
		appID, 90_000_000+testSeq.Add(1), status, verdictArg, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

// feedSession seeds one agent_sessions row with explicit cost/status stamps;
// a nil heartbeat leaves heartbeat_at null (the stuck-predicate coalesce
// path).
func feedSession(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, role, status string, cost int64, createdAt time.Time, heartbeat *time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into agent_sessions (run_id, role, status, cost_millicents, created_at, heartbeat_at)
		values ($1, $2, $3, $4, $5, $6)
		returning id`,
		runID, role, status, cost, createdAt, heartbeat).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent session: %v", err)
	}
	return id
}

// feedAlert seeds one alert_events row with an explicit arrival stamp;
// returns its id.
func feedAlert(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, kind string, createdAt time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into alert_events (app_id, kind, payload, created_at)
		values ($1, $2, '{}'::jsonb, $3)
		returning id`,
		appID, kind, createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	return id
}

// feedPendingPlanVersion seeds one plans row joined to appID plus one
// pending_approval plan_versions row — the approvals queue depth the KPI
// header counts. Same shape as seededPlanVersion (trigger_test.go) with the
// pending status.
func feedPendingPlanVersion(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID) {
	t.Helper()
	vid := uuid.New()
	planID := "feed-plan-" + vid.String()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		insert into plans (id, app_id, source_path, status, latest_version_id)
		values ($1, $2, 'plans/seed.md', 'pending', $3)`,
		planID, appID, vid); err != nil {
		t.Fatalf("seed plans row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into plan_versions (id, plan_id, version, content_hash, content, frontmatter, status, sources, created_by)
		values ($1, $2, '1.0.0', $3, $4, $5, $6, '[]', 'feed-test')`,
		vid, planID, "hash-"+vid.String(), seedPlanContent,
		`{"id":"seed-v1.0.0","app":"seed-app","title":"seeded plan","surfaces":["web"],"trigger":"on-demand"}`,
		approvals.StatusPendingApproval); err != nil {
		t.Fatalf("seed pending plan version: %v", err)
	}
}

func TestFeedKeysetPagination(t *testing.T) {
	pool := testPool(t)
	s := feedService(t, pool)
	ctx := context.Background()
	appID := budgetApp(t, pool, nil)
	base := time.Now().UTC().Add(-2 * time.Hour)

	// The session host run is itself a feed entry; 12 interleaved items plus
	// the host make 13, over three pages of 5.
	host := feedRun(t, pool, appID, "executing", "", base.Add(-5*time.Minute))
	want := map[string]bool{uuidString(host): true}
	for j := range 12 {
		at := base.Add(time.Duration(j) * time.Minute)
		var id pgtype.UUID
		switch j % 3 {
		case 0:
			id = feedRun(t, pool, appID, "executing", "", at)
		case 1:
			id = feedSession(t, pool, host, "navigator", "running", 0, at, nil)
		case 2:
			id = feedAlert(t, pool, appID, "flake_threshold", at)
		}
		if id.Valid {
			want[uuidString(id)] = true
		}
	}

	// Page through until the terminal page: entries strictly descending by
	// created_at, no id seen twice, every seeded id exactly once.
	seen := map[string]bool{}
	var last time.Time
	var cursor string
	pages := 0
	for {
		page, err := s.Feed(ctx, 5, "", cursor)
		if err != nil {
			t.Fatalf("feed page %d: %v", pages, err)
		}
		if len(page.Entries) > 5 {
			t.Fatalf("page %d returned %d entries, limit is 5", pages, len(page.Entries))
		}
		for _, e := range page.Entries {
			stamp, err := time.Parse(time.RFC3339Nano, e.CreatedAt)
			if err != nil {
				t.Fatalf("entry %s created_at %q: %v", e.ID, e.CreatedAt, err)
			}
			if last != (time.Time{}) && stamp.After(last) {
				t.Fatalf("entries out of order: %s after %s", stamp, last)
			}
			if seen[e.ID] {
				t.Fatalf("entry %s repeated across pages", e.ID)
			}
			seen[e.ID] = true
			last = stamp
		}
		pages++
		cursor = page.Cursor
		if cursor == "" {
			break
		}
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages for 13 entries at limit 5, got %d", pages)
	}
	if len(seen) != len(want) {
		t.Fatalf("collected %d distinct entries, seeded %d", len(seen), len(want))
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("seeded entry %s never surfaced", id)
		}
	}
}

func TestFeedAppFilter(t *testing.T) {
	pool := testPool(t)
	s := feedService(t, pool)
	ctx := context.Background()
	appA := budgetApp(t, pool, nil)
	appB := budgetApp(t, pool, nil)
	today := utcTodaySeed()

	runA := feedRun(t, pool, appA, "executing", "", today)
	feedSession(t, pool, runA, "navigator", "running", 0, today, nil)
	feedAlert(t, pool, appA, "flip_rate", today)
	feedRun(t, pool, appB, "executing", "", today)
	feedRun(t, pool, appB, "executing", "", today)

	page, err := s.Feed(ctx, 50, uuid.UUID(appA.Bytes).String(), "")
	if err != nil {
		t.Fatalf("feed app A: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("app A feed must carry exactly its 3 sources, got %d", len(page.Entries))
	}
	for _, e := range page.Entries {
		if e.AppID != uuid.UUID(appA.Bytes).String() {
			t.Fatalf("app filter leaked a foreign app row: %s", e.AppID)
		}
	}

	pageB, err := s.Feed(ctx, 50, uuid.UUID(appB.Bytes).String(), "")
	if err != nil {
		t.Fatalf("feed app B: %v", err)
	}
	if len(pageB.Entries) != 2 {
		t.Fatalf("app B feed must carry exactly its 2 runs, got %d", len(pageB.Entries))
	}
}

func TestFeedCursorContract(t *testing.T) {
	pool := testPool(t)
	s := feedService(t, pool)
	ctx := context.Background()
	appID := budgetApp(t, pool, nil)
	today := utcTodaySeed()
	feedRun(t, pool, appID, "executing", "", today)
	feedRun(t, pool, appID, "executing", "", today)
	feedRun(t, pool, appID, "executing", "", today)

	// Malformed cursors are request-shape violations (422 upstream), never
	// silent empty pages.
	for _, bad := range []string{"!!!", "aGVsbG8="} {
		if _, err := s.Feed(ctx, 10, "", bad); !errors.Is(err, errInvalidRequest) {
			t.Fatalf("cursor %q must map to errInvalidRequest, got %v", bad, err)
		}
	}

	// Limit 0 means the default page size; 500 clamps to the max. Both are
	// valid requests, not errors.
	for _, limit := range []int{0, 500} {
		page, err := s.Feed(ctx, limit, "", "")
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if len(page.Entries) != 3 {
			t.Fatalf("limit %d must still return all 3 entries, got %d", limit, len(page.Entries))
		}
	}
}

func TestKPIsDayWindowAndScopes(t *testing.T) {
	pool := testPool(t)
	s := feedService(t, pool)
	ctx := context.Background()
	today := utcTodaySeed()
	yesterday := utcYesterdaySeed()

	cap10k := new(int64)
	*cap10k = 10_000
	appA := budgetApp(t, pool, cap10k)
	appB := budgetApp(t, pool, nil)

	// App A today: one pass, one fail, one still-executing run.
	runPass := feedRun(t, pool, appA, "completed", "pass", today)
	feedRun(t, pool, appA, "completed", "fail", today)
	runExec := feedRun(t, pool, appA, "executing", "", today)
	// App A yesterday: a pass run outside the UTC-day window.
	feedRun(t, pool, appA, "completed", "pass", yesterday)

	// Sessions carry the spend: A today 750, B today 100, A yesterday 999.
	feedSession(t, pool, runPass, "navigator", "completed", 500, today, nil)
	feedSession(t, pool, runExec, "navigator", "running", 250, today, nil)
	runB := feedRun(t, pool, appB, "executing", "", today)
	feedSession(t, pool, runB, "vision_judge", "running", 100, today, nil)
	feedSession(t, pool, runPass, "navigator", "completed", 999, yesterday, nil)

	// Alerts land only in the feed, never the KPI header.
	feedAlert(t, pool, appA, "flake_threshold", today)
	feedAlert(t, pool, appA, "flip_rate", today)
	feedAlert(t, pool, appB, "flake_threshold", today)

	// Two pending approvals on A, one on B.
	feedPendingPlanVersion(t, pool, appA)
	feedPendingPlanVersion(t, pool, appA)
	feedPendingPlanVersion(t, pool, appB)

	scopedA, err := s.KPIs(ctx, uuid.UUID(appA.Bytes).String())
	if err != nil {
		t.Fatalf("kpis app A: %v", err)
	}
	if scopedA.RunsToday != 3 {
		t.Fatalf("app A runs today must be 3 (yesterday excluded), got %d", scopedA.RunsToday)
	}
	if scopedA.PassRateToday != 0.5 {
		t.Fatalf("app A pass rate must be 1 pass of 2 completed = 0.5, got %v", scopedA.PassRateToday)
	}
	if scopedA.Budget.SpentToday != 750 {
		t.Fatalf("app A spend today must be 750 (yesterday's 999 excluded), got %d", scopedA.Budget.SpentToday)
	}
	if scopedA.Budget.CapsSum != 10_000 || scopedA.Budget.UncappedCount != 1 {
		t.Fatalf("caps block is arm-wide: expected sum 10000 + 1 uncapped (B), got %d/%d",
			scopedA.Budget.CapsSum, scopedA.Budget.UncappedCount)
	}

	armWide, err := s.KPIs(ctx, "")
	if err != nil {
		t.Fatalf("kpis arm-wide: %v", err)
	}
	if armWide.RunsToday != 4 {
		t.Fatalf("arm-wide runs today must be 4, got %d", armWide.RunsToday)
	}
	if armWide.Budget.SpentToday != 850 {
		t.Fatalf("arm-wide spend must be 750+100, got %d", armWide.Budget.SpentToday)
	}
	if armWide.PendingApprovals != 3 {
		t.Fatalf("pending approvals are global by design: expected 3, got %d", armWide.PendingApprovals)
	}
	if scopedA.PendingApprovals != 3 {
		t.Fatalf("app filter must not narrow the global approvals depth, got %d", scopedA.PendingApprovals)
	}

	scopedB, err := s.KPIs(ctx, uuid.UUID(appB.Bytes).String())
	if err != nil {
		t.Fatalf("kpis app B: %v", err)
	}
	if scopedB.RunsToday != 1 || scopedB.PassRateToday != 0 || scopedB.Budget.SpentToday != 100 {
		t.Fatalf("app B aggregates wrong: %+v", scopedB)
	}
}

func TestBudgetEndpointLayers(t *testing.T) {
	pool := testPool(t)
	s := feedService(t, pool)
	ctx := context.Background()
	today := utcTodaySeed()

	// Capped app: spend under the ceiling.
	cap10k := new(int64)
	*cap10k = 10_000
	capped := budgetApp(t, pool, cap10k)
	runC := feedRun(t, pool, capped, "executing", "", today)
	feedSession(t, pool, runC, "navigator", "running", 750, today, nil)

	b, err := s.Budget(ctx, uuid.UUID(capped.Bytes).String())
	if err != nil {
		t.Fatalf("budget capped: %v", err)
	}
	if b.CapMillicents != 10_000 || b.CapKind != string(CapApp) {
		t.Fatalf("capped budget must resolve the app column: %+v", b)
	}
	if b.SpentToday != 750 || b.Remaining != 9_250 || !b.RemainingCapped {
		t.Fatalf("capped remaining math wrong: %+v", b)
	}
	if b.Refused || b.Reason != "" {
		t.Fatalf("under-cap app must not refuse: %+v", b)
	}

	// Uncapped app: explicit no-cap state, never a fabricated ceiling.
	uncapped := budgetApp(t, pool, nil)
	t.Setenv(EnvDefaultCapMillicents, "")
	runU := feedRun(t, pool, uncapped, "executing", "", today)
	feedSession(t, pool, runU, "vision_judge", "running", 100, today, nil)

	bu, err := s.Budget(ctx, uuid.UUID(uncapped.Bytes).String())
	if err != nil {
		t.Fatalf("budget uncapped: %v", err)
	}
	if bu.CapKind != string(CapNone) || bu.Capped || bu.RemainingCapped {
		t.Fatalf("uncapped budget must be the explicit no-cap state: %+v", bu)
	}
	if bu.SpentToday != 100 || bu.Remaining != 0 {
		t.Fatalf("uncapped spend must still surface, remaining 0: %+v", bu)
	}
	if bu.Refused {
		t.Fatalf("uncapped app must never refuse: %+v", bu)
	}

	// Zero ceiling: the 0-sentinel convention — no spend permitted today.
	zero := new(int64)
	zeroApp := budgetApp(t, pool, zero)
	bz, err := s.Budget(ctx, uuid.UUID(zeroApp.Bytes).String())
	if err != nil {
		t.Fatalf("budget zero: %v", err)
	}
	if !bz.Refused || !strings.Contains(bz.Reason, "zero") {
		t.Fatalf("zero cap must refuse with the zero reason: %+v", bz)
	}

	// A non-uuid app_id is a request-shape violation.
	if _, err := s.Budget(ctx, "not-a-uuid"); !errors.Is(err, errInvalidRequest) {
		t.Fatalf("bad app_id must map to errInvalidRequest, got %v", err)
	}
}

func TestAgentRailStuckPredicate(t *testing.T) {
	pool := testPool(t)
	s := feedService(t, pool)
	ctx := context.Background()
	appA := budgetApp(t, pool, nil)
	appB := budgetApp(t, pool, nil)
	base := time.Now().UTC()

	runA := feedRun(t, pool, appA, "executing", "", base)
	runB := feedRun(t, pool, appB, "executing", "", base)

	// Heartbeat offsets are absolute (anchored to base), so the rail's
	// oldest-first order is deterministic regardless of insert timing.
	fresh := base.Add(-1 * time.Second)
	feedSession(t, pool, runA, "navigator", "running", 0, fresh, &fresh)
	stale := base.Add(-120 * time.Second)
	feedSession(t, pool, runA, "navigator", "running", 0, stale, nil) // heartbeat null → coalesce created_at → stuck
	claimedAt := base.Add(-5 * time.Second)
	feedSession(t, pool, runA, "vision_judge", "claimed", 0, claimedAt, &claimedAt)
	feedSession(t, pool, runA, "navigator", "completed", 0, base.Add(-2*time.Second), nil) // terminal → drops off
	bHeartbeat := base.Add(-3 * time.Second)
	feedSession(t, pool, runB, "navigator", "running", 0, bHeartbeat, &bHeartbeat)

	rail, err := s.AgentRail(ctx, "")
	if err != nil {
		t.Fatalf("rail: %v", err)
	}
	if len(rail) != 4 {
		t.Fatalf("rail must carry exactly the 4 live sessions (terminal dropped), got %d", len(rail))
	}
	// Oldest heartbeats first: stale (null hb → 120s), claimed (5s), B (3s),
	// fresh (1s). Run placement pins the order to real rows.
	if rail[0].RunID != uuid.UUID(runA.Bytes).String() || rail[1].RunID != uuid.UUID(runA.Bytes).String() ||
		rail[2].RunID != uuid.UUID(runB.Bytes).String() || rail[3].RunID != uuid.UUID(runA.Bytes).String() {
		t.Fatalf("rail order wrong: %v", rail)
	}
	if rail[0].Role != "navigator" || rail[0].Status != "running" ||
		rail[1].Role != "vision_judge" || rail[1].Status != "claimed" ||
		rail[3].Role != "navigator" || rail[3].Status != "running" {
		t.Fatalf("rail role/status mismatch: %v", rail)
	}
	if rail[0].HeartbeatAgeSeconds < 90 {
		t.Fatalf("stale session must flag >90s, got age %d", rail[0].HeartbeatAgeSeconds)
	}
	if rail[3].HeartbeatAgeSeconds > 90 {
		t.Fatalf("fresh session must not flag, got age %d", rail[3].HeartbeatAgeSeconds)
	}

	// App filter narrows the rail.
	railA, err := s.AgentRail(ctx, uuid.UUID(appA.Bytes).String())
	if err != nil {
		t.Fatalf("rail app A: %v", err)
	}
	if len(railA) != 3 {
		t.Fatalf("app A rail must carry its 3 live sessions, got %d", len(railA))
	}
	railB, err := s.AgentRail(ctx, uuid.UUID(appB.Bytes).String())
	if err != nil {
		t.Fatalf("rail app B: %v", err)
	}
	if len(railB) != 1 || railB[0].RunID != uuid.UUID(runB.Bytes).String() {
		t.Fatalf("app B rail must carry only its session: %+v", railB)
	}
}
