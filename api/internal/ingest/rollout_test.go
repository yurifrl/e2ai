// Tests for the rollout gate against a real scratch Postgres: the healthy
// path hands the run to the runner queue (provisioning + execute job), an
// unhealthy rollout fails the run as INFRA_ERROR with the stage-tagged note
// (VERD-01), and a health_url-less app falls back to the fixed wait instead
// of polling. Database tests are skipped unless E2AI_TEST_DATABASE_URL is set
// (same contract as the rest of the package).
package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	plapp "github.com/NSXBet/e2ai/api/internal/app"
	plriver "github.com/NSXBet/e2ai/api/internal/platform/river"
	"github.com/NSXBet/e2ai/api/internal/verdict"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// gateWorker builds the worker with test-scale timing and a QueueRef bound to
// a real (never-started) River client, so both the ingest enqueue and the
// execute handoff land in river_job exactly as production would.
func gateWorker(t *testing.T, pool *pgxpool.Pool, budget, poll, fallback time.Duration) (*WaitRolloutWorker, *QueueRef) {
	t.Helper()

	worker := NewWaitRolloutWorker(pool, nil, noDefaultLogger())
	worker.budget, worker.pollInterval, worker.fallbackWait = budget, poll, fallback

	// Ingest's transactional path enqueues onto river_job, so the River schema
	// must exist (MigrateRiver is idempotent, safe across files sharing the
	// scratch database).
	if err := plapp.MigrateRiver(context.Background(), pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	bundle := plriver.NewWorkers()
	plriver.AddWorker(bundle, worker)
	client, err := plriver.DefaultClient(pool, 1,
		plriver.WithQueues(map[string]river.QueueConfig{
			"runner": {MaxWorkers: 1},
		}),
		plriver.WithWorkers(bundle),
	)
	if err != nil {
		t.Fatalf("build river client: %v", err)
	}
	qr := &QueueRef{}
	qr.Set(client)
	worker.queue = qr
	return worker, qr
}

// seedRun ingests a fresh run through the package's transactional ingest path
// so the gate is exercised against the exact row shape production produces.
// The app row is seeded first: the ingest path resolves registered_apps by
// identity triplet and never writes health_url itself.
func seedRun(t *testing.T, pool *pgxpool.Pool, qr *QueueRef, a app.App) uuid.UUID {
	t.Helper()
	const workflowRunID = int64(771) // unique to this file; seedSchema drops all tables between tests
	if _, err := app.SeedApps(context.Background(), pool, []app.App{a}); err != nil {
		t.Fatalf("seed app row: %v", err)
	}
	// River tables survive the app-schema teardown, so execute rows from a
	// sibling test would pollute enqueueCount; start each run's gate from an
	// empty job table.
	if _, err := pool.Exec(context.Background(), `delete from river_job`); err != nil {
		t.Fatalf("clear river_job: %v", err)
	}
	payload := mustJSON(map[string]any{
		"workflow": map[string]any{"name": a.WorkflowName},
		"workflow_run": dispatchRun(workflowRunID, "[BetWeb] Deploy to production",
			"workflow_dispatch", "success"),
	})
	runID, err := NewStore(pool).Ingest(context.Background(), RunSeed{
		App:            a,
		DeliveryID:     "d-rollout-gate",
		WorkflowRunID:  workflowRunID,
		RunNumber:      42,
		HeadSHA:        "9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e",
		HeadBranch:     "release/web",
		TriggerSource:  TriggerWebhook,
		SignatureValid: true,
		Payload:        payload,
		Enqueue:        true,
	}, qr)
	if err != nil {
		t.Fatalf("ingest seed run: %v", err)
	}
	if runID != ResultAccepted {
		t.Fatalf("ingest seed run: got result %d, want accepted", runID)
	}
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`select id from runs where workflow_run_id = $1`, workflowRunID).Scan(&id); err != nil {
		t.Fatalf("locate seeded run: %v", err)
	}
	return id
}

// runRow reads the run's pipeline fields straight from the scratch database.
func runRow(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (status, runVerdict, note string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`select status, coalesce(verdict, ''), coalesce(error_note, '') from runs where id = $1`,
		runID).Scan(&status, &runVerdict, &note); err != nil {
		t.Fatalf("load run row: %v", err)
	}
	return status, runVerdict, note
}

// enqueueCount counts river_job rows by kind (the execute handoff's evidence).
func enqueueCount(t *testing.T, pool *pgxpool.Pool, kind string) (n int, queue string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`select count(*), coalesce(max(queue), '') from river_job where kind = $1`, kind).
		Scan(&n, &queue); err != nil {
		t.Fatalf("count %q jobs: %v", kind, err)
	}
	return n, queue
}

func TestWaitRolloutHealthyPath(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	ctx := context.Background()

	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer health.Close()

	worker, qr := gateWorker(t, pool, time.Second, 20*time.Millisecond, time.Minute)
	seed := testApp
	seed.HealthURL = health.URL
	runID := seedRun(t, pool, qr, seed)

	job := &river.Job[WaitRolloutArgs]{Args: WaitRolloutArgs{RunID: runID}}
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("Work: %v", err)
	}

	if status, gotVerdict, note := runRow(t, pool, runID); status != "provisioning" {
		t.Errorf("status = %q, want provisioning", status)
	} else if gotVerdict != "" || note != "" {
		t.Errorf("healthy run must not record verdict/note, got %q/%q", gotVerdict, note)
	}

	if n, queue := enqueueCount(t, pool, "e2ai:run:execute"); n != 1 {
		t.Fatalf("execute jobs enqueued = %d, want 1", n)
	} else if queue != "runner" {
		t.Errorf("execute enqueued on queue %q, want runner", queue)
	}

	// The gate is one-shot: a replayed job must not double the handoff.
	if err := worker.Work(ctx, job); err != nil {
		t.Fatalf("replay Work: %v", err)
	}
	if n, _ := enqueueCount(t, pool, "e2ai:run:execute"); n != 1 {
		t.Errorf("replay doubled the execute enqueue: %d jobs", n)
	}
}

func TestWaitRolloutTimeoutFailsRun(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	ctx := context.Background()

	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sick.Close()

	worker, qr := gateWorker(t, pool, 200*time.Millisecond, 20*time.Millisecond, time.Minute)
	seed := testApp
	seed.HealthURL = sick.URL
	runID := seedRun(t, pool, qr, seed)

	if err := worker.Work(ctx, &river.Job[WaitRolloutArgs]{Args: WaitRolloutArgs{RunID: runID}}); err != nil {
		t.Fatalf("Work: %v", err)
	}

	status, gotVerdict, note := runRow(t, pool, runID)
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if gotVerdict != string(verdict.VerdictInfraError) {
		t.Errorf("verdict = %q, want %q", gotVerdict, verdict.VerdictInfraError)
	}
	if want := KindWaitRollout + ": health_url never returned a healthy response within the rollout gate budget"; note != want {
		t.Errorf("error_note = %q, want %q", note, want)
	}

	if n, _ := enqueueCount(t, pool, "e2ai:run:execute"); n != 0 {
		t.Errorf("failed gate enqueued %d execute jobs, want 0", n)
	}
}

func TestWaitRolloutFallbackWait(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	ctx := context.Background()

	// health_url absent: the gate waits the fixed budget, then hands off —
	// the Checkly-era blind sleep, preserved for apps without a probe. The
	// environment key (staging → production) keeps the app registration
	// distinct from the seeded bet-app row.
	worker, qr := gateWorker(t, pool, time.Minute, 20*time.Millisecond, 100*time.Millisecond)
	seed := testApp
	seed.Environment = "production"
	seed.HealthURL = ""
	runID := seedRun(t, pool, qr, seed)

	start := time.Now()
	if err := worker.Work(ctx, &river.Job[WaitRolloutArgs]{Args: WaitRolloutArgs{RunID: runID}}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("fallback returned after %s, want ~fixed wait", elapsed)
	}

	if status, _, _ := runRow(t, pool, runID); status != "provisioning" {
		t.Errorf("status = %q, want provisioning", status)
	}
	if n, queue := enqueueCount(t, pool, "e2ai:run:execute"); n != 1 || queue != "runner" {
		t.Errorf("execute jobs = %d on queue %q, want 1 on runner", n, queue)
	}
}
