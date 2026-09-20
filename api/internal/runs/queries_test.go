// Tests for the runs package: state-machine rules (pure) and the transition
// lifecycle plus list/detail round-trips against a real scratch Postgres.
// Database tests are skipped unless E2AI_TEST_DATABASE_URL is set; CI/dev
// uses an ephemeral `docker run postgres:16-alpine` (same convention as
// internal/app, whose Migrate applies the schema here).
package runs

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/app"
)

// testPool dials the E2AI_TEST_DATABASE_URL scratch database, skipping the
// test when no database is configured.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("E2AI_TEST_DATABASE_URL")
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

// migrate applies the app schema. Fresh scratch databases migrate once; the
// migration runner is idempotent, so repeat calls are no-ops.
func migrate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := app.Migrate(context.Background(), pool, slog.Default()); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}
	// Clean slate: the scratch database persists across runs. Delete in FK
	// order (children first; test_results/artifacts cascade via runs).
	for _, table := range []string{"webhook_events", "pr_comments", "artifacts", "test_results", "runs", "registered_apps"} {
		if _, err := pool.Exec(context.Background(), "delete from "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
}

// newUUID returns a random valid pgtype.UUID.
func newUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if _, err := rand.Read(u.Bytes[:]); err != nil {
		t.Fatalf("random uuid: %v", err)
	}
	u.Valid = true
	return u
}

// seedApp inserts one registered_apps row and returns its id. Unique on
// (owner, repo, workflow_name, environment) — the caller varies the name.
func seedApp(t *testing.T, pool *pgxpool.Pool, name string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker)
		values ('NSXBet', 'bet-app', $1, '.github/workflows/deploy.yaml',
		        'staging', '[BetWeb] Deploy to staging', 'op://vault/secret',
		        $2)
		returning id
	`, name, "<!-- e2ai:"+name+" -->").Scan(&id)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return id
}

// seedRun inserts one runs row with an explicit created_at (ListRuns orders
// by created_at desc — wall-clock rows would tie) and returns its id.
func seedRun(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, workflowRunID int64, createdAt time.Time, status Status) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, head_sha, environment, status, created_at)
		values ($1, $2, $3, '0000000000000000000000000000000000000000', 'staging', $4, $5)
		returning id
	`, appID, workflowRunID, workflowRunID, string(status), createdAt).Scan(&id)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func TestTransitionRules(t *testing.T) {
	// One representative happy-path link per stage plus every failure edge:
	// specific behaviors, not a mirror of the transition table.
	happy := [][2]Status{
		{StatusQueued, StatusWaitingRollout},
		{StatusWaitingRollout, StatusProvisioning},
		{StatusProvisioning, StatusExecuting},
		{StatusExecuting, StatusReporting},
		{StatusReporting, StatusCompleted},
	}
	for _, pair := range happy {
		if !Can(pair[0], pair[1]) {
			t.Errorf("happy path %s → %s must be allowed", pair[0], pair[1])
		}
	}
	for _, s := range []Status{StatusQueued, StatusWaitingRollout, StatusProvisioning, StatusExecuting, StatusReporting} {
		if !Can(s, StatusFailed) {
			t.Errorf("%s → failed must be allowed: every active stage can fail", s)
		}
	}
	for _, s := range []Status{StatusCompleted, StatusFailed} {
		if !s.Terminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	for _, jump := range [][2]Status{
		{StatusQueued, StatusExecuting}, {StatusQueued, StatusCompleted},
		{StatusReporting, StatusExecuting}, {StatusCompleted, StatusFailed},
		{StatusFailed, StatusQueued},
	} {
		if Can(jump[0], jump[1]) {
			t.Errorf("invalid transition %s → %s accepted", jump[0], jump[1])
		}
	}
	if Can(Status("nonsense"), StatusQueued) {
		t.Error("unknown source status must never transition")
	}
}

func TestTransitionLifecycle(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	ctx := context.Background()
	appID := seedApp(t, pool, "lifecycle")
	runID := seedRun(t, pool, appID, 1, time.Now(), StatusQueued)

	created, err := GetRun(ctx, pool, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if created == nil {
		t.Fatal("run just inserted must exist")
	}
	if created.Status != StatusQueued {
		t.Errorf("status = %s, want queued", created.Status)
	}
	if created.WorkflowRunID != 1 || created.RunNumber != 1 || created.Environment != "staging" || created.HeadSHA == "" || created.TriggerSource != "webhook" {
		t.Errorf("scalar fields = %+v", created)
	}
	if created.StartedAt.Valid || created.FinishedAt.Valid {
		t.Errorf("started_at=%v finished_at=%v: fresh queued run must have neither stamped", created.StartedAt, created.FinishedAt)
	}
	if created.Verdict.Valid || created.ErrorNote.Valid {
		t.Errorf("verdict=%v error_note=%v: null until report", created.Verdict, created.ErrorNote)
	}

	// Walk the happy path. started_at stamps on first departure from queued;
	// finished_at stamps only on entering a terminal status.
	for i, next := range []Status{StatusWaitingRollout, StatusProvisioning, StatusExecuting, StatusReporting, StatusCompleted} {
		if err := Transition(ctx, pool, runID, next); err != nil {
			t.Fatalf("transition to %s: %v", next, err)
		}
		got, err := GetRun(ctx, pool, runID)
		if err != nil {
			t.Fatalf("get after %s: %v", next, err)
		}
		if got.Status != next {
			t.Errorf("status = %s, want %s", got.Status, next)
		}
		if !got.StartedAt.Valid {
			t.Errorf("started_at still null after step %d (%s)", i, next)
		}
		if next != StatusCompleted && got.FinishedAt.Valid {
			t.Errorf("finished_at stamped early at %s", next)
		}
		if next == StatusCompleted && !got.FinishedAt.Valid {
			t.Error("finished_at must stamp on completion")
		}
	}

	// Terminal states accept nothing.
	err = Transition(ctx, pool, runID, StatusFailed)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("completed → failed err = %v, want ErrInvalidTransition", err)
	}
	var te *TransitionError
	if !errors.As(err, &te) || te.From != StatusCompleted || te.To != StatusFailed {
		t.Errorf("err = %v (%+v), want *TransitionError{completed, failed}", err, te)
	}

	// Invalid jump rejected without mutating the row.
	err = Transition(ctx, pool, runID, StatusReporting)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("completed → reporting err = %v, want ErrInvalidTransition", err)
	}
	got, err := GetRun(ctx, pool, runID)
	if err != nil || got == nil {
		t.Fatalf("get after rejected transition: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %s after rejection, want completed", got.Status)
	}
}

func TestTransitionFailurePath(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	ctx := context.Background()
	appID := seedApp(t, pool, "failure-path")
	runID := seedRun(t, pool, appID, 1, time.Now(), StatusQueued)

	// Every active stage can fail; failed is terminal.
	for _, next := range []Status{StatusWaitingRollout, StatusProvisioning} {
		if err := Transition(ctx, pool, runID, next); err != nil {
			t.Fatalf("to %s: %v", next, err)
		}
	}
	if err := Transition(ctx, pool, runID, StatusFailed); err != nil {
		t.Fatalf("provisioning → failed: %v", err)
	}
	got, err := GetRun(ctx, pool, runID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusFailed || !got.FinishedAt.Valid {
		t.Errorf("status=%s finished_at=%v, want failed + stamped", got.Status, got.FinishedAt)
	}
	if err := Transition(ctx, pool, runID, StatusQueued); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("failed → queued err = %v, want ErrInvalidTransition", err)
	}
}

func TestGetRunMissing(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)

	missing := pgtype.UUID{Bytes: [16]byte{0xAA}, Valid: true}
	run, err := GetRun(context.Background(), pool, missing)
	if err != nil {
		t.Fatalf("missing run must scan to nil, got err %v", err)
	}
	if run != nil {
		t.Errorf("missing run = %+v, want nil", run)
	}
	detail, err := GetRunDetail(context.Background(), pool, missing)
	if err != nil || detail != nil {
		t.Errorf("missing detail = %+v err %v, want nil nil", detail, err)
	}
}

func TestListRunsPaginationAndFilters(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	ctx := context.Background()

	appA := seedApp(t, pool, "page-a")
	appB := seedApp(t, pool, "page-b")

	base := time.Now().Add(-time.Hour)
	var ids []pgtype.UUID
	for i := 0; i < 7; i++ {
		target := appA
		if i%3 == 2 { // rows 2 and 5 belong to app B
			target = appB
		}
		ids = append(ids, seedRun(t, pool, target, int64(i+1), base.Add(time.Duration(i)*time.Minute), StatusQueued))
	}
	// Drive run 0 → failed and run 1 → completed so the row spread includes
	// terminal states for later detail checks.
	if err := Transition(ctx, pool, ids[0], StatusFailed); err != nil {
		t.Fatalf("fail run 0: %v", err)
	}
	for _, next := range []Status{StatusWaitingRollout, StatusProvisioning, StatusExecuting, StatusReporting, StatusCompleted} {
		if err := Transition(ctx, pool, ids[1], next); err != nil {
			t.Fatalf("advance run 1 to %s: %v", next, err)
		}
	}

	page1, err := ListRuns(ctx, pool, appA, Page{Limit: 3})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page 1 len = %d, want 3", len(page1))
	}
	page2, err := ListRuns(ctx, pool, appA, Page{Limit: 3, Offset: 3})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("page 2 len = %d, want 2 (app A owns 5 runs)", len(page2))
	}

	// No overlap, no app bleed, newest first across page boundaries.
	seen := map[pgtype.UUID]bool{}
	for i, r := range append(append([]Run{}, page1...), page2...) {
		if seen[r.ID] {
			t.Errorf("run %s appears on both pages", r.ID)
		}
		seen[r.ID] = true
		if r.AppID != appA {
			t.Errorf("run %s belongs to app %s, want %s", r.ID, r.AppID, appA)
		}
		if i > 0 {
			prev := append(append([]Run{}, page1...), page2...)[i-1]
			if prev.CreatedAt.Time.Before(r.CreatedAt.Time) {
				t.Errorf("not newest-first: %v before %v", prev.CreatedAt.Time, r.CreatedAt.Time)
			}
		}
	}

	// Limit clamps: 0 → default 50, absurd → cap. All app-A rows come back.
	big, err := ListRuns(ctx, pool, appA, Page{Limit: 10_000})
	if err != nil {
		t.Fatalf("big page: %v", err)
	}
	if len(big) != 5 {
		t.Errorf("big page returned %d rows, want all 5 of app A", len(big))
	}
}

func TestGetRunDetailRoundTrip(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	ctx := context.Background()
	appID := seedApp(t, pool, "detail")
	runID := seedRun(t, pool, appID, 9, time.Now(), StatusCompleted)

	_, err := pool.Exec(ctx, `
		insert into test_results (run_id, suite, title, file, line, project, outcome, status, attempts, duration_ms, error_message, classification, attachments)
		values
			($1, 'auth', 'logs in', 'auth/login.spec.ts', 12, 'chromium', 'expected', 'passed', 1, 1200, null, null, $2::jsonb),
			($1, 'auth', 'blocks replay', 'auth/replay.spec.ts', 30, 'chromium', 'flaky', 'failed', 2, 3000, 'timeout', 'app_bug', null),
			($1, null, 'suiteless test', null, null, null, 'unexpected', 'timedOut', 1, null, 'timed out', null, null)
	`, runID, `[{"kind":"screenshot","path":"shots/a.png"},{"kind":"trace","path":"traces/a.zip"}]`)
	if err != nil {
		t.Fatalf("seed test results: %v", err)
	}

	_, err = pool.Exec(ctx, `
		insert into artifacts (run_id, kind, path, size_bytes, created_at)
		values
			($1, 'html_report', 'reports/run-9/index.html', 4096, now() - interval '10 minutes'),
			($1, 'results_json', 'reports/run-9/results.json', 512, now())
	`, runID)
	if err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}

	detail, err := GetRunDetail(ctx, pool, runID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail == nil {
		t.Fatal("detail just seeded must exist")
	}
	if detail.Run == nil || detail.Run.ID != runID || detail.Run.Status != StatusCompleted {
		t.Fatalf("run = %+v", detail.Run)
	}

	// test_results order: coalesce(suite,'') → coalesce(file,'') → title, so
	// the suiteless row sorts before the suite rows regardless of insert order.
	if len(detail.TestResults) != 3 {
		t.Fatalf("test results = %d, want 3", len(detail.TestResults))
	}
	if detail.TestResults[0].Title != "suiteless test" {
		t.Errorf("first = %q, want suiteless row first (null suite sorts before 'auth')", detail.TestResults[0].Title)
	}
	if detail.TestResults[1].Title != "logs in" || detail.TestResults[2].Title != "blocks replay" {
		t.Errorf("suite order = %q, %q", detail.TestResults[1].Title, detail.TestResults[2].Title)
	}

	// Spot-check the evidence fields verdict (Wave 3) will consume.
	flaky := detail.TestResults[2]
	if flaky.Outcome != "flaky" || flaky.Status != "failed" || flaky.Attempts != 2 || flaky.DurationMS.Int32 != 3000 || flaky.ErrorMessage.String != "timeout" || flaky.Classification.String != "app_bug" {
		t.Errorf("flaky row = %+v", flaky)
	}
	expected := detail.TestResults[1]
	if expected.Outcome != "expected" || expected.Status != "passed" || expected.Attempts != 1 {
		t.Errorf("expected row = %+v", expected)
	}
	// jsonb storage normalizes whitespace, so compare semantically.
	var gotAttachments []map[string]string
	if err := json.Unmarshal(expected.Attachments, &gotAttachments); err != nil {
		t.Fatalf("attachments not json: %v (%s)", err, expected.Attachments)
	}
	if len(gotAttachments) != 2 || gotAttachments[0]["kind"] != "screenshot" || gotAttachments[1]["path"] != "traces/a.zip" {
		t.Errorf("attachments = %v", gotAttachments)
	}

	// Artifacts newest first.
	if len(detail.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(detail.Artifacts))
	}
	if detail.Artifacts[0].Kind != "results_json" {
		t.Errorf("first artifact = %s, want results_json (newest)", detail.Artifacts[0].Kind)
	}
	if detail.Artifacts[1].Path != "reports/run-9/index.html" || detail.Artifacts[1].SizeBytes.Int64 != 4096 {
		t.Errorf("second artifact = %+v", detail.Artifacts[1])
	}
}
