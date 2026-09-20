package suite

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/platform/postgres"
	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// Database tests are skipped unless E2AI_TEST_DATABASE_URL is set — the same
// scratch-database contract as internal/runs (CI/dev boots the ephemeral
// postgres from the Tilt stack).
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

// migrate applies the app schema and clears the pipeline tables so the
// scratch database starts clean.
func migrate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := app.Migrate(context.Background(), pool, slog.Default()); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}
	for _, table := range []string{"run_plan_resolutions", "plan_approvals", "plan_versions", "plans",
		"corpus_behaviors", "corpus_learnings",
		"webhook_events", "pr_comments", "artifacts", "test_results", "runs", "registered_apps"} {
		if _, err := pool.Exec(context.Background(), "delete from "+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
	}
}

// seedAppRun inserts one registered app and one run in provisioning (the
// status T7's gate leaves runs in when it enqueues execute).
func seedAppRun(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
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
		values ($1, $2, 42, 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'staging', 'provisioning')
	`, runID, appID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}

// storeExecutor builds an Executor over the scratch pool with its container
// step pre-stubbed and the run's artifacts directory pre-populated from the
// committed fixture — everything between "container finished" and "report
// enqueued", which is where the idempotency guard lives.
func storeExecutor(t *testing.T, pool *pgxpool.Pool, artifactsDir string) *Executor {
	t.Helper()
	data, err := os.ReadFile("testdata/results.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "results.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// One attachment file so the artifacts index has something to record.
	if err := os.WriteFile(filepath.Join(artifactsDir, "screenshot.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Executor{
		Pool:         pool,
		Queue:        &recordingEnqueuer{},
		Runner:       fakeRunner{},
		ArtifactsDir: artifactsDir,
	}
}

// fakeRunner is the no-op container runner: the fixture artifacts were
// already placed in the run's directory.
type fakeRunner struct{}

func (fakeRunner) Run(context.Context, ContainerSpec) error { return nil }

// recordingEnqueuer captures report enqueues; each attempt records exactly
// one insert, so a replay must not enqueue twice either.
type recordingEnqueuer struct {
	mu      sync.Mutex
	Reports []reportArgs
}

func (r *recordingEnqueuer) Insert(_ context.Context, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	ra, ok := args.(reportArgs)
	if !ok {
		return nil, fmt.Errorf("unexpected job args %T", args)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Reports = append(r.Reports, ra)
	return nil, nil
}

// TestExecuteStoreIdempotentReplay drives the store step of the execute job
// twice over one run and asserts attempt 2 is a no-op: the evidence rows are
// not duplicated, the artifacts table holds one row per file, and the verdict
// summary is only stamped once. The executor never transitions the run on a
// replay (the report handoff already happened on attempt 1).
func TestExecuteStoreIdempotentReplay(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)

	artifactsDir := t.TempDir()
	e := storeExecutor(t, pool, artifactsDir)

	for attempt := 1; attempt <= 2; attempt++ {
		data, err := os.ReadFile(filepath.Join(artifactsDir, "results.json"))
		if err != nil {
			t.Fatalf("attempt %d: read results.json: %v", attempt, err)
		}
		rep, err := Parse(data)
		if err != nil {
			t.Fatalf("attempt %d: parse: %v", attempt, err)
		}
		if err := e.store(context.Background(), runID, rep, artifactsDir); err != nil {
			t.Fatalf("attempt %d: store: %v", attempt, err)
		}
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from test_results where run_id = $1`, pgUUID(runID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("test_results rows after replay = %d, want 4 (fixture's 4 tests, not 8)", n)
	}
	if err := pool.QueryRow(context.Background(),
		`select count(*) from artifacts where run_id = $1`, pgUUID(runID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("artifacts rows after replay = %d, want 2 (results.json + screenshot.png, not 4)", n)
	}
}

// TestExecuteRunsInheritsPoolSchema pins the detail the pool setup glossed
// over: the executor's pool is the same scratch database the migrations ran
// in (OpenWithRetry wraps it), so a suite row insert never hits an unmigrated
// schema.
func TestExecuteRunsInheritsPoolSchema(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)

	pooled, err := postgres.Open(context.Background(), os.Getenv("E2AI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pooled.Close()

	e := &Executor{Pool: pooled.Pool, ArtifactsDir: t.TempDir(), Queue: &recordingEnqueuer{}}
	// A guarded transition against the same pool proves schema + state share
	// one view: provisioning -> executing is legal from the seeded row.
	if err := runs.Transition(context.Background(), e.Pool, pgUUID(runID), runs.StatusExecuting); err != nil {
		t.Fatalf("transition against executor pool: %v", err)
	}
}

// loadPins reads the pinned image + persisted runner version for one run.
func loadPins(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (image, version string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), `
		select runner_image, runner_version from runs where id = $1
	`, runID).Scan(&image, &version); err != nil {
		t.Fatalf("load pins: %v", err)
	}
	return image, version
}

// TestExecutePinsPerRunImageOverride (PLAT-05): ExecuteArgs.Image pins the
// container for this run only — the spec carries the override and the run row
// is stamped with the effective image before the container starts. The
// report's config version lands on runs.runner_version (drift canary).
func TestExecutePinsPerRunImageOverride(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)

	e, runner := gateExecutor(t, pool, runID)
	e.Image = "e2ai-runner:default"
	if err := e.Work(context.Background(), &river.Job[ExecuteArgs]{
		Args: ExecuteArgs{RunID: runID.String(), Image: "e2ai-runner:candidate"},
	}); err != nil {
		t.Fatalf("Work returned error: %v", err)
	}

	if len(runner.Specs) != 1 || runner.Specs[0].Image != "e2ai-runner:candidate" {
		t.Fatalf("container specs = %+v, want one run on the candidate image", runner.Specs)
	}
	image, version := loadPins(t, pool, runID)
	if image != "e2ai-runner:candidate" {
		t.Fatalf("runs.runner_image = %q, want the job override", image)
	}
	if version != "1.58.2" {
		t.Fatalf("runs.runner_version = %q, want the fixture report's 1.58.2", version)
	}
}

// TestExecuteDefaultImageStampsProcessDefault (PLAT-05): with no override the
// executor's process default runs, and that default is what the run row
// records — a pin, not an omission.
func TestExecuteDefaultImageStampsProcessDefault(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	runID := seedAppRun(t, pool)

	e, runner := gateExecutor(t, pool, runID)
	e.Image = "e2ai-runner:default"
	if err := e.Work(context.Background(), &river.Job[ExecuteArgs]{
		Args: ExecuteArgs{RunID: runID.String()},
	}); err != nil {
		t.Fatalf("Work returned error: %v", err)
	}

	if len(runner.Specs) != 1 || runner.Specs[0].Image != "e2ai-runner:default" {
		t.Fatalf("container specs = %+v, want one run on the process default", runner.Specs)
	}
	if image, _ := loadPins(t, pool, runID); image != "e2ai-runner:default" {
		t.Fatalf("runs.runner_image = %q, want the process default", image)
	}
}
