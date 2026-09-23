// State store tests run against a real scratch Postgres (E2AI_TEST_DATABASE_URL
// — ephemeral docker postgres:16-alpine) so the single-row upsert guard, the
// pause attribution columns, and the same-transaction audit append are
// exercised against actual DDL, not mocks. The interesting invariants here are
// structural: a second cockpit_state row is impossible, an unpause clears
// attribution, and an audit row without an operator can never exist.
package cockpit

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("E2AI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("dial test database: %v", err)
	}
	t.Cleanup(pool.Close)

	// Cockpit tables are 0010_cockpit.sql — bring the schema up before use.
	if err := app.Migrate(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}
	// Tests assume a clean slate: repeated runs against one scratch database
	// must not see state or audit fixtures from earlier runs. Budget tests
	// also seed registered_apps/runs/agent_sessions — cascading the app wipe
	// clears every dependent row in one statement.
	if _, err := pool.Exec(ctx, `truncate registered_apps cascade`); err != nil {
		t.Fatalf("clear registered_apps: %v", err)
	}
	for _, table := range []string{"cockpit_state", "audit_events"} {
		if _, err := pool.Exec(ctx, `delete from `+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
	return pool
}

var testSeq atomic.Int64

// auditCount returns the number of audit_events rows, for attribution
// assertions.
func auditCount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`select count(*) from audit_events`).Scan(&n); err != nil {
		t.Fatalf("count audit_events: %v", err)
	}
	return n
}

func TestReadDefaultUnpaused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// testPool deleted the seed row: Read must treat the missing row as the
	// unpaused default rather than erroring.
	s, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("read missing state: %v", err)
	}
	if s.Paused {
		t.Fatalf("missing row read as paused: %+v", s)
	}
	if s.PausedReason != "" || s.PausedBy != "" || s.PausedAt != nil {
		t.Fatalf("missing row carried pause attribution: %+v", s)
	}
}

func TestSetPausedRequiresActor(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := SetPaused(ctx, pool, "", true, "no actor"); !errors.Is(err, ErrNoActor) {
		t.Fatalf("empty actor: want ErrNoActor, got %v", err)
	}
	if n := auditCount(t, pool); n != 0 {
		t.Fatalf("refused mutation still appended %d audit rows", n)
	}
}

func TestPauseUnpauseRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	actor := "operator-" + strconv.FormatInt(testSeq.Add(1), 10) + "@e2ai.test"

	// Pause with reason: flag, attribution, and audit row all land together.
	if err := SetPaused(ctx, pool, actor, true, "incident 4711"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("read paused state: %v", err)
	}
	if !s.Paused || s.PausedReason != "incident 4711" || s.PausedBy != actor || s.PausedAt == nil {
		t.Fatalf("paused state mismatch: %+v", s)
	}
	n := auditCount(t, pool)
	if n != 1 {
		t.Fatalf("pause: want 1 audit row, got %d", n)
	}
	var pausedDetail bool
	var auditAction string
	if err := pool.QueryRow(ctx, `
		select (details->>'paused')::bool, action from audit_events`).Scan(&pausedDetail, &auditAction); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if auditAction != ActionPause || !pausedDetail {
		t.Fatalf("pause audit row mismatch: action=%q details.paused=%v", auditAction, pausedDetail)
	}

	// Unpause: the row flips but clears attribution — it must not keep the
	// old pause's reason/actor, or the render would lie about who paused.
	if err := SetPaused(ctx, pool, actor, false, ""); err != nil {
		t.Fatalf("unpause: %v", err)
	}
	s, err = Read(ctx, pool)
	if err != nil {
		t.Fatalf("read unpaused state: %v", err)
	}
	if s.Paused || s.PausedReason != "" || s.PausedBy != "" || s.PausedAt != nil {
		t.Fatalf("unpause kept pause attribution: %+v", s)
	}
	n = auditCount(t, pool)
	if n != 2 {
		t.Fatalf("unpause: want 2 audit rows, got %d", n)
	}
	var actions []string
	rows, err := pool.Query(ctx, `select action from audit_events order by created_at`)
	if err != nil {
		t.Fatalf("query audit actions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scan audit action: %v", err)
		}
		actions = append(actions, a)
	}
	if len(actions) != 2 || actions[0] != ActionPause || actions[1] != ActionUnpause {
		t.Fatalf("audit sequence mismatch: %v", actions)
	}
}

func TestSetPausedUpsertSelfHeals(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	actor := "healer-" + strconv.FormatInt(testSeq.Add(1), 10) + "@e2ai.test"

	// The seed row is already gone (testPool truncates). The write path must
	// upsert — a lost seed may never wedge the pause control.
	if err := SetPaused(ctx, pool, actor, true, "self-heal"); err != nil {
		t.Fatalf("pause without seed row: %v", err)
	}
	s, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("read healed state: %v", err)
	}
	if !s.Paused || s.PausedBy != actor {
		t.Fatalf("upsert did not land: %+v", s)
	}

	// And a second write must update the single row, never insert a second
	// one (the check(id) primary key makes this a structural guarantee; the
	// count assertion confirms the upsert path, not a naive insert).
	if err := SetPaused(ctx, pool, actor, true, "still healing"); err != nil {
		t.Fatalf("second pause: %v", err)
	}
	var rowCount int64
	if err := pool.QueryRow(ctx, `select count(*) from cockpit_state`).Scan(&rowCount); err != nil {
		t.Fatalf("count cockpit_state: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("want single cockpit_state row, got %d", rowCount)
	}
	s, _ = Read(ctx, pool)
	if s.PausedReason != "still healing" {
		t.Fatalf("second pause did not update reason: %+v", s)
	}
}
