// Tests for the report job: verdict derivation + persistence, the
// reporting → completed transition, the failure path (error note, never
// silent), and replay idempotency — against the same scratch Postgres as
// internal/runs and internal/suite. Skipped unless E2AI_TEST_DATABASE_URL
// is set.
package report

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/NSXBet/e2ai/api/internal/app"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := envDatabaseURL()
	if url == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("dial scratch db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrate applies the app schema and clears the pipeline tables so the
// scratch database starts clean (same contract as the other packages).
func migrate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := app.Migrate(context.Background(), pool, slog.Default()); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}
	// agent tables clear first (they cascade from runs, but an explicit
	// delete keeps the scratch db deterministic across suites).
	for _, table := range []string{"session_steps", "agent_sessions", "webhook_events", "pr_comments", "artifacts", "test_results", "runs", "registered_apps"} {
		if _, err := pool.Exec(context.Background(), "delete from "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
}

// seedRunAt inserts one app + run with status executing (where the report
// job picks the run up) and returns its id.
func seedRunAt(t *testing.T, pool *pgxpool.Pool, status string) uuid.UUID {
	t.Helper()
	appID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		insert into registered_apps
			(id, owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker)
		values ($1, 'NSXBet', 'bet-app', 'deploy-e2ai', '.github/workflows/deploy.yaml',
		        'staging', '[BetWeb] Deploy to staging', 'op://vault/secret', '<!-- e2ai -->')
	`, appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	runID, err := uuid.NewRandom()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
		insert into runs (id, app_id, workflow_run_id, run_number, head_sha, environment, status)
		values ($1, $2, 42, 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'staging', $3)
	`, runID, appID, status); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// seedTest inserts one test_results row for the run.
func seedTest(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, title, file, outcome, status string, attempts, durationMS int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into test_results
			(run_id, title, file, outcome, status, attempts, duration_ms)
		values ($1, $2, $3, $4, $5, $6, $7)
	`, runID, title, file, outcome, status, attempts, durationMS); err != nil {
		t.Fatalf("seed test result: %v", err)
	}
}

// runState reads status, verdict, summary-count columns and error_note for
// assertions. Nullable columns collapse to "" / 0 when NULL.
func runState(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) map[string]any {
	t.Helper()
	vals := map[string]any{}
	scan := func(col string, dst any) {
		if err := pool.QueryRow(context.Background(),
			`select `+col+` from runs where id = $1`, runID).Scan(dst); err != nil {
			t.Fatalf("read %s: %v", col, err)
		}
	}
	readString := func(col string) string {
		var s *string
		scan(col, &s)
		if s == nil {
			return ""
		}
		return *s
	}
	readInt := func(col string) int {
		var n *int
		scan(col, &n)
		if n == nil {
			return 0
		}
		return *n
	}
	vals["status"] = readString("status")
	vals["verdict"] = readString("verdict")
	vals["total_tests"] = readInt("total_tests")
	vals["passed"] = readInt("passed")
	vals["failed"] = readInt("failed")
	vals["flaky"] = readInt("flaky")
	vals["skipped"] = readInt("skipped")
	vals["duration_ms"] = readInt("duration_ms")
	vals["error_note"] = readString("error_note")
	return vals
}

// riverJob builds a river job handle for direct Work invocation.
func riverJob(runID string) *river.Job[ReportArgs] {
	return &river.Job[ReportArgs]{Args: ReportArgs{RunID: runID}}
}

// envDatabaseURL is the scratch-database gate.
func envDatabaseURL() string { return os.Getenv("E2AI_TEST_DATABASE_URL") }

// errBoom is a distinct sentinel cause for the failure path.
func errBoom() error { return errors.New("boom: verdict derivation blew up") }

// TestJobHappyPath runs the full Work flow against a seeded executing run:
// two passing tests and one flaky retry must derive FLAKY, persist the
// folded counts, and complete the run.
func TestJobHappyPath(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedRunAt(t, pool, "executing")

	seedTest(t, pool, runID, "logs in", "tests/auth.spec.ts", "expected", "passed", 1, 500)
	seedTest(t, pool, runID, "lists bets", "tests/bets.spec.ts", "expected", "passed", 1, 700)
	seedTest(t, pool, runID, "edits a bet", "tests/bets.spec.ts", "flaky", "passed", 2, 1200)

	w := &Worker{Pool: pool, Logger: slog.Default()}
	job := riverJob(runID.String())
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("work: %v", err)
	}

	st := runState(t, pool, runID)
	if st["status"] != "completed" {
		t.Errorf("status = %q, want completed", st["status"])
	}
	if got, want := st["verdict"], "flaky"; got != want {
		t.Errorf("verdict = %v, want %v", got, want)
	}
	if got, want := st["passed"], 2; got != want {
		t.Errorf("passed = %v, want %v", got, want)
	}
	if got, want := st["flaky"], 1; got != want {
		t.Errorf("flaky = %v, want %v", got, want)
	}
	if got, want := st["total_tests"], 3; got != want {
		t.Errorf("total_tests = %v, want %v", got, want)
	}
	if got, want := st["duration_ms"], 2400; got != want {
		t.Errorf("duration_ms = %v, want %v", got, want)
	}
}

// TestJobEmptyEvidence exercises the zero-tests rule: no parseable rows is
// INFRA_ERROR, never PASS — the run still completes; nothing was evaluated,
// so every summary count (including folded skipped) is zero.
func TestJobEmptyEvidence(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedRunAt(t, pool, "executing")

	w := &Worker{Pool: pool, Logger: slog.Default()}
	job := riverJob(runID.String())
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("work: %v", err)
	}

	st := runState(t, pool, runID)
	if st["status"] != "completed" {
		t.Errorf("status = %q, want completed", st["status"])
	}
	if got, want := st["verdict"], "infra_error"; got != want {
		t.Errorf("verdict = %v, want %v", got, want)
	}
	if got, want := st["skipped"], 0; got != want {
		t.Errorf("skipped = %v, want %v", got, want)
	}
}

// TestJobUnexpectedFailure derives FAIL from unexpected outcomes with a
// failed last attempt.
func TestJobUnexpectedFailure(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedRunAt(t, pool, "executing")

	seedTest(t, pool, runID, "logs in", "tests/auth.spec.ts", "expected", "passed", 1, 500)
	seedTest(t, pool, runID, "edits a bet", "tests/bets.spec.ts", "unexpected", "failed", 3, 900)

	w := &Worker{Pool: pool, Logger: slog.Default()}
	job := riverJob(runID.String())
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("work: %v", err)
	}

	st := runState(t, pool, runID)
	if got, want := st["verdict"], "fail"; got != want {
		t.Errorf("verdict = %v, want %v", got, want)
	}
	if got, want := st["failed"], 1; got != want {
		t.Errorf("failed = %v, want %v", got, want)
	}
	if st["status"] != "completed" {
		t.Errorf("status = %q, want completed", st["status"])
	}
}

// TestJobReplayIsNoop: a run already completed by a prior attempt must not
// error on replay (terminal states have no outgoing edges).
func TestJobReplayIsNoop(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedRunAt(t, pool, "executing")
	seedTest(t, pool, runID, "logs in", "tests/auth.spec.ts", "expected", "passed", 1, 500)

	w := &Worker{Pool: pool, Logger: slog.Default()}
	job := riverJob(runID.String())
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("first work: %v", err)
	}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("replay: %v", err)
	}

	st := runState(t, pool, runID)
	if st["status"] != "completed" {
		t.Errorf("status = %q, want completed", st["status"])
	}
}

// TestJobBadRunID: an unparsable run id errors and the job is retried by
// River (no run row exists to fail).
func TestJobBadRunID(t *testing.T) {
	w := &Worker{Pool: nil, Logger: slog.Default()}
	job := riverJob("not-a-uuid")
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("expected error for unparsable run id")
	}
}

// TestJobMissingRunRow: the run row vanished before the job ran — the
// reporting transition must fail loudly (never silently swallow).
func TestJobMissingRunRow(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	w := &Worker{Pool: pool, Logger: slog.Default()}
	job := riverJob(uuid.New().String())
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("expected error for missing run row")
	}
}

// TestJobFailPath stamps error_note with the stage tag and moves the run to
// failed — the never-silent rule.
func TestJobFailPath(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedRunAt(t, pool, "reporting")

	w := &Worker{Pool: pool, Logger: slog.Default()}
	// fail always returns the wrapped cause so River retries the job; the
	// run-level side effects (status, error_note) are what the test asserts.
	cause := errBoom()
	_ = w.fail(context.Background(), toPgUUID(runID), "verdict persist", cause, w.logger())

	st := runState(t, pool, runID)
	if st["status"] != "failed" {
		t.Errorf("status = %q, want failed", st["status"])
	}
	note, ok := st["error_note"].(string)
	if !ok || note == "" {
		t.Fatalf("error_note = %v, want non-empty", st["error_note"])
	}
	if want := "report verdict persist:"; len(note) < len(want) || note[:len(want)] != want {
		t.Errorf("error_note = %q, want prefix %q", note, want)
	}
}
