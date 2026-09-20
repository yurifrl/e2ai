// Agent-session orchestrator tests (Phase 3 T1): atomic claim race,
// guarded transition rejections, orphan sweep, and the budget engine —
// all against the scratch Postgres with the real 0004 constraints.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("E2AI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial scratch postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := app.Migrate(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := app.MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("migrate river: %v", err)
	}
	for _, table := range []string{"agent_sessions", "session_steps", "plans", "plan_versions", "plan_approvals", "corpus_behaviors", "corpus_learnings", "runs", "registered_apps", "river_job"} {
		if _, err := pool.Exec(ctx, "truncate table "+table+" cascade"); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return pool
}

func seedRun(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	// workflow_run_id is unique per (app_id, workflow_run_id) — the seed is
	// only idempotent on the app upsert, so give every call its own id.
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		with app as (
			insert into registered_apps (owner, repo, workflow_name, workflow_path, environment, display_title_pattern, webhook_secret_ref, comment_marker)
			values ('yuri', 'betweb', 'Release - Web', '.github/workflows/remote_workflow_betweb.yaml', 'staging', '\[BetWeb\] Deploy to staging', 'op://vault/betweb-webhook', '<!-- e2ai:run:betweb -->')
			on conflict (owner, repo, workflow_name, environment)
			do update set workflow_path = excluded.workflow_path
			returning id
		)
		insert into runs (app_id, workflow_run_id, run_number, head_sha, environment)
		select id, 424242 + $1, 7, 'deadbeef', 'staging' from app
		returning id
	`, seedRunSeq.Add(1)).Scan(&id)
	if err != nil {
		t.Fatalf("seed run fixture: %v", err)
	}
	return id
}

// seedRunSeq hands each seedRun call a distinct workflow_run_id.
var seedRunSeq atomic.Int64

func seedSession(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, role Role, stepCeiling int, costCeiling int64) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into agent_sessions (run_id, role, step_ceiling, cost_ceiling_millicents)
		values ($1, $2, $3, $4) returning id
	`, runID, role, stepCeiling, costCeiling).Scan(&id)
	if err != nil {
		t.Fatalf("seed agent session %s: %v", role, err)
	}
	return id
}

func sessionRow(t *testing.T, pool *pgxpool.Pool, id pgtype.UUID) (status Status, note string, worker *string, stepsUsed int, cost int64) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `
		select status, coalesce(error_note, ''), coalesce(worker_id, ''), steps_used, cost_millicents
		from agent_sessions where id = $1
	`, id).Scan(&status, &note, &worker, &stepsUsed, &cost)
	if err != nil {
		t.Fatalf("read agent session: %v", err)
	}
	return status, note, worker, stepsUsed, cost
}

func stepCount(t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from session_steps where session_id = $1`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count session steps: %v", err)
	}
	return n
}

// Transition table: pure guards — active-stage exits are always legal,
// terminal states have no outgoing edges.
func TestTransitionTable(t *testing.T) {
	if !Can(StatusQueued, StatusClaimed) {
		t.Error("queued -> claimed must be legal")
	}
	for _, bad := range []struct{ from, to Status }{
		{StatusQueued, StatusRunning},
		{StatusQueued, StatusCompleted},
		{StatusClaimed, StatusClaimed},
		{StatusClaimed, StatusCompleted},
		{StatusRunning, StatusClaimed},
		{StatusRunning, StatusRunning},
	} {
		if Can(bad.from, bad.to) {
			t.Errorf("%s -> %s must be rejected", bad.from, bad.to)
		}
	}
	// Terminal states: every exit forbidden.
	for _, from := range []Status{StatusCompleted, StatusFailed, StatusKilled, StatusBudgetExhausted} {
		if !from.Terminal() {
			t.Errorf("%s must be terminal", from)
		}
		for _, to := range []Status{StatusQueued, StatusClaimed, StatusRunning, StatusCompleted, StatusFailed, StatusKilled, StatusBudgetExhausted} {
			if Can(from, to) {
				t.Errorf("terminal %s -> %s must be rejected", from, to)
			}
		}
	}
}

// Guarded transitions against the live row: illegal jumps are typed
// rejections, legal ones stamp started_at/finished_at.
func TestTransitionGuard(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID := seedRun(t, pool)
	id := seedSession(t, pool, runID, RoleNavigator, 0, 0)

	// Forbidden from the table.
	err := Transition(ctx, pool, id, StatusRunning)
	var terr *TransitionError
	if !errors.As(err, &terr) || !errors.Is(err, ErrInvalidTransition) || terr.From != StatusQueued || terr.To != StatusRunning {
		t.Fatalf("queued -> running: want typed rejection, got %v", err)
	}

	// Legal climb queued -> claimed -> running stamps started_at once.
	if err := Transition(ctx, pool, id, StatusClaimed); err != nil {
		t.Fatalf("queued -> claimed: %v", err)
	}
	// Concurrent drift: a second queued->claimed is a typed rejection.
	if err := Transition(ctx, pool, id, StatusClaimed); !errors.As(err, &terr) {
		t.Fatalf("claimed -> claimed: want typed rejection, got %v", err)
	}
	if err := Transition(ctx, pool, id, StatusRunning); err != nil {
		t.Fatalf("claimed -> running: %v", err)
	}
	if err := Transition(ctx, pool, id, StatusCompleted); err != nil {
		t.Fatalf("running -> completed: %v", err)
	}
	if err := Transition(ctx, pool, id, StatusFailed); !errors.As(err, &terr) {
		t.Fatalf("completed -> failed: want typed rejection, got %v", err)
	}

	var startedAt, finishedAt *time.Time
	if err := pool.QueryRow(ctx,
		`select started_at, finished_at from agent_sessions where id = $1`, id).
		Scan(&startedAt, &finishedAt); err != nil {
		t.Fatal(err)
	}
	if startedAt == nil || finishedAt == nil {
		t.Fatalf("started_at/finished_at not stamped: %v %v", startedAt, finishedAt)
	}

	// Unknown id.
	var bogus pgtype.UUID
	copy(bogus.Bytes[:], "not-a-real-id")
	if err := Transition(ctx, pool, bogus, StatusClaimed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: want ErrNotFound, got %v", err)
	}
}

// Atomic claim race: two concurrent ClaimNext over ONE queued session —
// exactly one winner, one ErrNoSessions, no double-claim.
func TestClaimAtomicRace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID := seedRun(t, pool)
	_ = seedSession(t, pool, runID, RoleNavigator, 0, 0)

	const claimers = 2
	type result struct {
		id  pgtype.UUID
		err error
	}
	results := make(chan result, claimers)
	var start sync.WaitGroup
	start.Add(1)
	for i := range claimers {
		go func(i int) {
			start.Wait()
			id, err := ClaimNext(ctx, pool, "worker-1")
			results <- result{id, err}
		}(i)
	}
	start.Done()

	var wins []pgtype.UUID
	var losses int
	for range claimers {
		r := <-results
		if r.err == nil {
			wins = append(wins, r.id)
		} else if errors.Is(r.err, ErrNoSessions) {
			losses++
		} else {
			t.Fatalf("unexpected claim error: %v", r.err)
		}
	}
	if len(wins) != 1 || losses != claimers-1 {
		t.Fatalf("want exactly 1 winner, got %d wins / %d losses", len(wins), losses)
	}
	status, _, worker, _, _ := sessionRow(t, pool, wins[0])
	if status != StatusClaimed || worker == nil || *worker != "worker-1" {
		t.Fatalf("claimed row wrong: status=%s worker=%v", status, worker)
	}

	// Wider race: 6 claimers over 3 sessions -> exactly 3 distinct winners.
	const sessions, claimers2 = 3, 6
	for range sessions {
		_ = seedSession(t, pool, runID, RoleEdgeHunter, 0, 0)
	}
	out := make(chan result, claimers2)
	var start2 sync.WaitGroup
	start2.Add(1)
	for i := range claimers2 {
		go func(i int) {
			start2.Wait()
			id, err := ClaimNext(ctx, pool, "worker-2")
			out <- result{id, err}
		}(i)
	}
	start2.Done()
	seen := map[string]bool{}
	var wins2 int
	for range claimers2 {
		r := <-out
		if r.err == nil {
			key := string(r.id.Bytes[:])
			if seen[key] {
				t.Fatal("two claimers won the same session")
			}
			seen[key] = true
			wins2++
		} else if !errors.Is(r.err, ErrNoSessions) {
			t.Fatalf("unexpected claim error: %v", r.err)
		}
	}
	if wins2 != sessions {
		t.Fatalf("want %d unique wins over %d sessions, got %d", sessions, sessions, wins2)
	}
}

// Orphan sweep: a lapsed claimed session is failed+orphaned and a fresh
// queued session is requeued for the same run/role with identical
// ceilings; fresh and terminal sessions are untouched.
func TestOrphanSweep(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID := seedRun(t, pool)

	lapsed := seedSession(t, pool, runID, RoleNavigator, 2, 50)
	if _, err := ClaimNext(ctx, pool, "dead-worker"); err != nil {
		t.Fatalf("claim lapsed session: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`update agent_sessions set heartbeat_at = now() - interval '120 seconds' where id = $1`, lapsed); err != nil {
		t.Fatal(err)
	}

	// Fresh claimed session: heartbeat current -> must survive.
	fresh := seedSession(t, pool, runID, RoleVisionJudge, 0, 0)
	if _, err := ClaimNext(ctx, pool, "live-worker"); err != nil {
		t.Fatalf("claim fresh session: %v", err)
	}

	recovered, err := SweepOrphans(ctx, pool)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("want exactly 1 orphan recovered, got %d", recovered)
	}

	status, note, worker, _, _ := sessionRow(t, pool, lapsed)
	if status != StatusFailed {
		t.Fatalf("lapsed session status = %s, want failed", status)
	}
	if !strings.Contains(note, "orphaned") {
		t.Fatalf("orphan note missing: %q", note)
	}
	if worker == nil || *worker != "dead-worker" {
		t.Fatalf("dead attempt must keep its worker_id: %v", worker)
	}

	// Fresh requeue: same run/role, ceilings copied, queued again.
	var newID pgtype.UUID
	var newRole, newStatus string
	var newStep int
	var newCost int64
	err = pool.QueryRow(ctx, `
		select id, role, status, step_ceiling, cost_ceiling_millicents
		from agent_sessions
		where run_id = $1 and worker_id is null and created_at > now() - interval '10 seconds'
		order by created_at desc limit 1
	`, runID).Scan(&newID, &newRole, &newStatus, &newStep, &newCost)
	if err != nil {
		t.Fatalf("find requeued session: %v", err)
	}
	if newRole != string(RoleNavigator) || newStatus != "queued" || newStep != 2 || newCost != 50 {
		t.Fatalf("requeue mismatch: role=%s status=%s ceilings=%d/%d", newRole, newStatus, newStep, newCost)
	}

	// Fresh claimed session untouched.
	status, _, _, _, _ = sessionRow(t, pool, fresh)
	if status != StatusClaimed {
		t.Fatalf("fresh claimed session disturbed: %s", status)
	}
}

// Budget engine: step-ceiling breach flips budget_exhausted (terminal),
// further appends are ErrTerminal, and the cost accumulator math is exact
// from token counts.
func TestBudgetStepCeiling(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID := seedRun(t, pool)
	id := seedSession(t, pool, runID, RoleNavigator, 2, 0)

	// Cost math: (1000*40 + 500*40)/1000 = 60 millicents per step.
	stepCost := CostMillicents(1000, 500, 40, 40)
	if stepCost != 60 {
		t.Fatalf("CostMillicents math wrong: %d", stepCost)
	}
	// Truncation: sub-millicent drops.
	if got := CostMillicents(500, 0, 1, 1); got != 0 {
		t.Fatalf("CostMillicents must truncate, got %d", got)
	}

	step := Step{Kind: "navigate", TokensIn: 1000, TokensOut: 500, CostMillicents: stepCost}
	for range 2 {
		if err := AppendStep(ctx, pool, id, step); err != nil {
			t.Fatalf("append step: %v", err)
		}
	}
	if got := stepCount(t, pool, id); got != 2 {
		t.Fatalf("want 2 steps appended, got %d", got)
	}

	err := AppendStep(ctx, pool, id, step)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("third append: want ErrBudgetExhausted, got %v", err)
	}
	status, note, _, stepsUsed, cost := sessionRow(t, pool, id)
	if status != StatusBudgetExhausted {
		t.Fatalf("breached session status = %s, want budget_exhausted", status)
	}
	if !strings.Contains(note, "step ceiling 2") {
		t.Fatalf("breach note missing: %q", note)
	}
	if stepsUsed != 2 || cost != 2*stepCost {
		t.Fatalf("accumulator wrong: steps=%d cost=%d", stepsUsed, cost)
	}
	// Terminal: the step insert never landed (same-tx accounting).
	if got := stepCount(t, pool, id); got != 2 {
		t.Fatalf("breached step must not persist, got %d", got)
	}
	if err := AppendStep(ctx, pool, id, step); !errors.Is(err, ErrTerminal) {
		t.Fatalf("append after terminal: want ErrTerminal, got %v", err)
	}

	// Gapless idx.
	var idxs []int
	rows, err := pool.Query(ctx, `select idx from session_steps where session_id = $1 order by idx`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var i int
		if err := rows.Scan(&i); err != nil {
			t.Fatal(err)
		}
		idxs = append(idxs, i)
	}
	if len(idxs) != 2 || idxs[0] != 0 || idxs[1] != 1 {
		t.Fatalf("idx must be gapless 0,1: got %v", idxs)
	}
}

// Cost ceiling: a step that would cross the ceiling is rejected up front
// (BUDG-02, same boundary semantics as the step ceiling); cost math from
// token counts verified end to end.
func TestBudgetCostCeiling(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID := seedRun(t, pool)
	id := seedSession(t, pool, runID, RoleVisionJudge, 0, 100)

	// PROSPECTIVE semantics (BUDG-02): a step whose cost would cross the
	// ceiling is rejected up front — the accumulator never overshoots.
	stepCost := CostMillicents(1000, 500, 40, 40)
	step := Step{Kind: "judge", Decision: json.RawMessage(`{"verdict":"pass"}`), TokensIn: 1000, TokensOut: 500, CostMillicents: stepCost}

	if err := AppendStep(ctx, pool, id, step); err != nil {
		t.Fatalf("append step: %v", err)
	}
	// 60 + 60 = 120 > 100 -> this append must trip the cost ceiling.
	err := AppendStep(ctx, pool, id, step)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("cost breach: want ErrBudgetExhausted, got %v", err)
	}
	status, note, _, _, cost := sessionRow(t, pool, id)
	if status != StatusBudgetExhausted {
		t.Fatalf("status = %s, want budget_exhausted", status)
	}
	if !strings.Contains(note, "cost ceiling 100") {
		t.Fatalf("breach note missing: %q", note)
	}
	if cost != stepCost {
		t.Fatalf("accumulated cost = %d, want %d (no overshoot)", cost, stepCost)
	}
	if got := stepCount(t, pool, id); got != 1 {
		t.Fatalf("breached step must not persist, got %d", got)
	}
}
