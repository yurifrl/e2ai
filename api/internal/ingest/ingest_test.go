// Tests for the ingest package: webhook handler plumbing (pure, fake store),
// the transactional dedupe path and the reconciliation sweep against a real
// scratch Postgres. Database tests are skipped unless E2AI_TEST_DATABASE_URL
// is set (same contract as internal/app's tests).
package ingest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const testSecret = "whsec-test"

// testApp mirrors registry/bet-app.yaml's identity for the scratch database.
var testApp = app.App{
	Owner:               "NSXBet",
	Repo:                "bet-app",
	WorkflowName:        "Release - Web",
	WorkflowPath:        ".github/workflows/deploy-web.yml",
	Environment:         "staging",
	DisplayTitlePattern: `\[BetWeb\] Deploy to staging`,
	HealthURL:           "https://stg-bet-app.nsx.dev/healthz",
	Surfaces:            []string{"web"},
	WebhookSecretRef:    "op://SRE Team/e2ai/github_webhook_secret",
	CommentMarker:       "<!-- e2ai:run:bet-app-web -->",
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err) // literal maps of plain values cannot fail
	}
	return data
}

// dispatchRun mirrors GitHub's workflow_run payload fragment.
func dispatchRun(id int64, displayTitle, event, conclusion string) map[string]any {
	return map[string]any{
		"id":            id,
		"run_number":    42,
		"head_sha":      "e2a1111111111111111111111111111111111111",
		"head_branch":   "main",
		"display_title": displayTitle,
		"html_url":      "https://github.com/NSXBet/bet-app/actions/runs/" + strconv.FormatInt(id, 10),
		"event":         event,
		"conclusion":    conclusion,
	}
}

// validPayload is an accepted deploy delivery: a completed, successful
// workflow_dispatch whose display_title matches the registered pattern.
func validPayload() []byte {
	return mustJSON(map[string]any{
		"workflow":     map[string]any{"name": "Release - Web"},
		"workflow_run": dispatchRun(9001, "[BetWeb] Deploy to staging", "workflow_dispatch", "success"),
	})
}

func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func post(body []byte, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/github/webhook/", bytes.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func noDefaultLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// ---- fakes ----

type fakeStore struct {
	result Result
	err    error
	seeds  []RunSeed
}

func (s *fakeStore) Ingest(_ context.Context, seed RunSeed, _ JobEnqueuer) (Result, error) {
	s.seeds = append(s.seeds, seed)
	return s.result, s.err
}

type fakeEnqueuer struct{ args []river.JobArgs }

func (q *fakeEnqueuer) InsertTx(_ context.Context, _ pgx.Tx, args river.JobArgs, _ *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	q.args = append(q.args, args)
	return &rivertype.JobInsertResult{}, nil
}

type fakeLister struct {
	runs      []RemoteRun
	err       error
	workflows []string
}

func (l *fakeLister) ListCompletedRuns(_ context.Context, owner, repo, workflow string, _ time.Time) ([]RemoteRun, error) {
	l.workflows = append(l.workflows, owner+"/"+repo+":"+workflow)
	return l.runs, l.err
}

// ---- handler (pure) ----

func newTestHandler(store *fakeStore, queue *fakeEnqueuer) *Handler {
	return NewHandler(testSecret, []app.App{testApp}, store, queue, noDefaultLogger())
}

func TestHandlerRejectsBadSignature(t *testing.T) {
	store := &fakeStore{result: ResultAccepted}
	queue := &fakeEnqueuer{}
	h := newTestHandler(store, queue)

	cases := map[string]string{
		"missing header":   "",
		"no prefix":        "deadbeef",
		"wrong secret":     sign(t, "other", validPayload()),
		"tampered body":    sign(t, testSecret, []byte(`{"workflow":{}}`)),
		"truncated digest": "sha256=abcd",
	}
	for name, sig := range cases {
		t.Run(name, func(t *testing.T) {
			headers := map[string]string{
				"X-GitHub-Event":    "workflow_run",
				"X-GitHub-Delivery": "d-badsig",
			}
			if sig != "" {
				headers["X-Hub-Signature-256"] = sig
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, post(validPayload(), headers))
			if rr.Code != http.StatusForbidden {
				t.Errorf("code = %d, want 403 (body %q)", rr.Code, rr.Body.String())
			}
			if len(store.seeds) != 0 {
				t.Errorf("store got %d seeds, want 0", len(store.seeds))
			}
		})
	}
}

func TestHandlerAcceptsValidDelivery(t *testing.T) {
	store := &fakeStore{result: ResultAccepted}
	queue := &fakeEnqueuer{}
	h := newTestHandler(store, queue)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post(validPayload(), map[string]string{
		"X-GitHub-Event":      "workflow_run",
		"X-GitHub-Delivery":   "d-ok",
		"X-Hub-Signature-256": sign(t, testSecret, validPayload()),
	}))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"status":"accepted"}` {
		t.Errorf("body = %q, want accepted", got)
	}
	if len(store.seeds) != 1 {
		t.Fatalf("store got %d seeds, want 1", len(store.seeds))
	}
	seed := store.seeds[0]
	if seed.DeliveryID != "d-ok" || seed.WorkflowRunID != 9001 || seed.RunNumber != 42 ||
		seed.TriggerSource != TriggerWebhook || !seed.SignatureValid || !seed.Enqueue {
		t.Errorf("seed mismatch: %+v", seed)
	}
	if seed.HeadSHA != "e2a1111111111111111111111111111111111111" || seed.App.Owner != "NSXBet" {
		t.Errorf("seed facts mismatch: %+v", seed)
	}
}

func TestHandlerFiltersNonDeployDeliveries(t *testing.T) {
	cases := map[string]struct {
		payload []byte
		event   string
		status  string
		seeds   int
	}{
		// Matched app + payload filter mismatch: recorded for the evidence
		// trail, never enqueued.
		"non workflow_dispatch event": {
			payload: mustJSON(map[string]any{"workflow": map[string]any{"name": "Release - Web"},
				"workflow_run": dispatchRun(9002, "[BetWeb] Deploy to staging", "push", "success")}),
			event:  "workflow_run",
			status: "recorded",
			seeds:  1,
		},
		"failed conclusion": {
			payload: mustJSON(map[string]any{"workflow": map[string]any{"name": "Release - Web"},
				"workflow_run": dispatchRun(9003, "[BetWeb] Deploy to staging", "workflow_dispatch", "failure")}),
			event:  "workflow_run",
			status: "recorded",
			seeds:  1,
		},
		// No matching app: nothing can be attributed (webhook_events.app_id is
		// NOT NULL), but a signature-valid delivery is too valuable to drop
		// silently — it lands in the admin triage queue (EDGE-01) instead.
		"unmatched display title": {
			payload: mustJSON(map[string]any{"workflow": map[string]any{"name": "Release - Web"},
				"workflow_run": dispatchRun(9004, "Release v1.2.3", "workflow_dispatch", "success")}),
			event:  "workflow_run",
			status: "triaged",
			seeds:  0,
		},
		"unregistered workflow": {
			payload: mustJSON(map[string]any{"workflow": map[string]any{"name": "Other Workflow"},
				"workflow_run": dispatchRun(9005, "[BetWeb] Deploy to staging", "workflow_dispatch", "success")}),
			event:  "workflow_run",
			status: "triaged",
			seeds:  0,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{result: ResultRecorded}
			queue := &fakeEnqueuer{}
			h := newTestHandler(store, queue)

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, post(tc.payload, map[string]string{
				"X-GitHub-Event":      tc.event,
				"X-GitHub-Delivery":   "d-filter-" + name,
				"X-Hub-Signature-256": sign(t, testSecret, tc.payload),
			}))
			if rr.Code != http.StatusAccepted || rr.Body.String() != fmt.Sprintf(`{"status":%q}`, tc.status) {
				t.Errorf("code = %d body = %q, want 202 %s", rr.Code, rr.Body.String(), tc.status)
			}
			if len(store.seeds) != tc.seeds {
				t.Fatalf("got %d seeds, want %d", len(store.seeds), tc.seeds)
			}
			for _, seed := range store.seeds {
				if seed.Enqueue {
					t.Error("filtered delivery must not enqueue")
				}
			}
		})
	}
}

func TestHandlerDuplicatesMapToAlreadyIngested(t *testing.T) {
	for name, result := range map[string]Result{
		"duplicate delivery": ResultDuplicateEvent,
		"duplicate run":      ResultDuplicateRun,
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{result: result}
			h := newTestHandler(store, &fakeEnqueuer{})

			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, post(validPayload(), map[string]string{
				"X-GitHub-Event":      "workflow_run",
				"X-GitHub-Delivery":   "d-dup",
				"X-Hub-Signature-256": sign(t, testSecret, validPayload()),
			}))
			if rr.Code != http.StatusAccepted || rr.Body.String() != `{"status":"already-ingested"}` {
				t.Errorf("code = %d body = %q, want 202 already-ingested", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandlerMalformedRequests(t *testing.T) {
	store := &fakeStore{result: ResultAccepted}
	h := newTestHandler(store, &fakeEnqueuer{})

	t.Run("missing delivery id", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, post(validPayload(), map[string]string{
			"X-GitHub-Event":      "workflow_run",
			"X-Hub-Signature-256": sign(t, testSecret, validPayload()),
		}))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})

	t.Run("unparseable body", func(t *testing.T) {
		body := []byte(`{not json`)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, post(body, map[string]string{
			"X-GitHub-Event":      "workflow_run",
			"X-GitHub-Delivery":   "d-junk",
			"X-Hub-Signature-256": sign(t, testSecret, body),
		}))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want 400", rr.Code)
		}
	})

	t.Run("store error is a 500", func(t *testing.T) {
		store := &fakeStore{err: fmt.Errorf("db down")}
		h := newTestHandler(store, &fakeEnqueuer{})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, post(validPayload(), map[string]string{
			"X-GitHub-Event":      "workflow_run",
			"X-GitHub-Delivery":   "d-500",
			"X-Hub-Signature-256": sign(t, testSecret, validPayload()),
		}))
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("code = %d, want 500", rr.Code)
		}
	})
}

func TestHandlerIgnoresNonWorkflowRunEvents(t *testing.T) {
	store := &fakeStore{result: ResultAccepted}
	h := newTestHandler(store, &fakeEnqueuer{})

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, post(validPayload(), map[string]string{
		"X-GitHub-Event":      "ping",
		"X-GitHub-Delivery":   "d-ping",
		"X-Hub-Signature-256": sign(t, testSecret, validPayload()),
	}))
	if rr.Code != http.StatusAccepted || rr.Body.String() != `{"status":"ignored"}` {
		t.Errorf("code = %d body = %q, want 202 ignored", rr.Code, rr.Body.String())
	}
	if len(store.seeds) != 0 {
		t.Errorf("ping must short-circuit before the store, got %d seeds", len(store.seeds))
	}
}

func TestHandlerMethodNotAllowed(t *testing.T) {
	h := newTestHandler(&fakeStore{}, &fakeEnqueuer{})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/github/webhook/", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d, want 405", rr.Code)
	}
}

// ---- store + sweep (scratch database) ----

// dbPool dials the E2AI_TEST_DATABASE_URL scratch database, skipping when no
// database is configured (same contract as internal/app's tests).
func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("E2AI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database-backed ingest tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("dial test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedSchema applies the app migrations so the scratch database has the
// ingest tables. Down+Migrate keeps runs idempotent across test files.
func seedSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := app.Down(context.Background(), pool); err != nil {
		t.Fatalf("down schema: %v", err)
	}
	if err := app.Migrate(context.Background(), pool, noDefaultLogger()); err != nil {
		t.Fatalf("migrate schema: %v", err)
	}
	// Registered app row backs resolveAppID's lookup.
	ctx := context.Background()
	if _, err := app.SeedApps(ctx, pool, []app.App{testApp}); err != nil {
		t.Fatalf("seed app: %v", err)
	}
}

func seedFromRemote(runID int64, displayTitle string) RunSeed {
	return sweepSeed(testApp, RemoteRun{
		ID:           runID,
		RunNumber:    7,
		HeadSHA:      "e2b1111111111111111111111111111111111111",
		HeadBranch:   "main",
		DisplayTitle: displayTitle,
		Event:        "workflow_dispatch",
		Conclusion:   "success",
		HTMLURL:      "https://github.com/NSXBet/bet-app/actions/runs/" + strconv.FormatInt(runID, 10),
	})
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func TestIngestAtomicPath(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	seed := RunSeed{
		App:            testApp,
		DeliveryID:     "d-atomic-1",
		WorkflowRunID:  9100,
		RunNumber:      1,
		HeadSHA:        "e2c1111111111111111111111111111111111111",
		HeadBranch:     "main",
		TriggerSource:  TriggerWebhook,
		SignatureValid: true,
		Payload:        validPayload(),
		Enqueue:        true,
	}
	queue := &fakeEnqueuer{}

	result, err := store.Ingest(ctx, seed, queue)
	if err != nil || result != ResultAccepted {
		t.Fatalf("first ingest = %d, %v; want accepted", result, err)
	}
	if n := countRows(t, pool, "select count(*) from webhook_events where delivery_id = $1", seed.DeliveryID); n != 1 {
		t.Errorf("webhook_events rows = %d, want 1", n)
	}
	if n := countRows(t, pool, "select count(*) from runs where workflow_run_id = $1", seed.WorkflowRunID); n != 1 {
		t.Errorf("runs rows = %d, want 1", n)
	}
	if len(queue.args) != 1 {
		t.Fatalf("enqueued jobs = %d, want 1 wait-rollout", len(queue.args))
	}
	if _, ok := queue.args[0].(WaitRolloutArgs); !ok {
		t.Errorf("enqueued kind = %s, want WaitRolloutArgs", queue.args[0].Kind())
	}

	// Duplicate delivery GUID → 202 duplicate, no second run row, no second job.
	result, err = store.Ingest(ctx, seed, queue)
	if err != nil || result != ResultDuplicateEvent {
		t.Errorf("duplicate ingest = %d, %v; want duplicate-event", result, err)
	}
	if n := countRows(t, pool, "select count(*) from runs where workflow_run_id = $1", seed.WorkflowRunID); n != 1 {
		t.Errorf("runs rows after duplicate = %d, want 1", n)
	}
	if len(queue.args) != 1 {
		t.Errorf("jobs after duplicate = %d, want still 1", len(queue.args))
	}

	// Filtered payload: recorded, no run row.
	filtered := seed
	filtered.DeliveryID = "d-atomic-2"
	filtered.Enqueue = false
	result, err = store.Ingest(ctx, filtered, queue)
	if err != nil || result != ResultRecorded {
		t.Errorf("filtered ingest = %d, %v; want recorded", result, err)
	}
	if n := countRows(t, pool, "select count(*) from runs where workflow_run_id = $1", seed.WorkflowRunID); n != 1 {
		t.Errorf("runs rows after filtered = %d, want 1", n)
	}
}

func TestIngestDuplicateRunSameDelivery(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	// Same run, fresh delivery GUID (GitHub retries new GUIDs on redelivery):
	// the run-level unique gate must report duplicate run, not double-enqueue.
	base := RunSeed{
		App:            testApp,
		WorkflowRunID:  9200,
		RunNumber:      2,
		HeadSHA:        "e2d1111111111111111111111111111111111111",
		HeadBranch:     "main",
		TriggerSource:  TriggerWebhook,
		SignatureValid: true,
		Payload:        validPayload(),
		Enqueue:        true,
	}
	queue := &fakeEnqueuer{}

	first := base
	first.DeliveryID = "d-run-1"
	if result, err := store.Ingest(ctx, first, queue); err != nil || result != ResultAccepted {
		t.Fatalf("first ingest = %d, %v; want accepted", result, err)
	}
	second := base
	second.DeliveryID = "d-run-2"
	result, err := store.Ingest(ctx, second, queue)
	if err != nil || result != ResultDuplicateRun {
		t.Errorf("second ingest = %d, %v; want duplicate-run", result, err)
	}
	if len(queue.args) != 1 {
		t.Errorf("jobs = %d, want 1 (no re-enqueue)", len(queue.args))
	}
}

func TestSweepPicksUpMissedRun(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	lister := &fakeLister{runs: []RemoteRun{
		// Missed by the webhook — should be ingested.
		{ID: 9300, RunNumber: 3, HeadSHA: "e2e1111111111111111111111111111111111111",
			HeadBranch: "main", DisplayTitle: "[BetWeb] Deploy to staging",
			Event: "workflow_dispatch", Conclusion: "success",
			HTMLURL: "https://github.com/NSXBet/bet-app/actions/runs/9300"},
		// Already ingested above — sweep's second pass must be a no-op.
	}}
	queue := &fakeEnqueuer{}
	worker := NewReconcileWorker([]app.App{testApp}, lister, store, queue, noDefaultLogger())

	if err := worker.Work(ctx, &river.Job[ReconcileArgs]{}); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if n := countRows(t, pool, "select count(*) from runs where workflow_run_id = 9300"); n != 1 {
		t.Fatalf("sweep must create the run row, got %d", n)
	}
	var trigger string
	if err := pool.QueryRow(ctx, "select trigger_source from runs where workflow_run_id = 9300").Scan(&trigger); err != nil || trigger != TriggerReconcile {
		t.Errorf("trigger_source = %q (%v), want reconcile", trigger, err)
	}
	// The sweep's evidence trail lands in webhook_events too.
	if n := countRows(t, pool, "select count(*) from webhook_events where delivery_id = 'reconcile:9300'"); n != 1 {
		t.Errorf("sweep event rows = %d, want 1", n)
	}

	// Idempotent second pass: nothing new.
	if err := worker.Work(ctx, &river.Job[ReconcileArgs]{}); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n := countRows(t, pool, "select count(*) from runs where workflow_run_id = 9300"); n != 1 {
		t.Errorf("runs rows after second sweep = %d, want 1", n)
	}
	if n := countRows(t, pool, "select count(*) from webhook_events where delivery_id = 'reconcile:9300'"); n != 1 {
		t.Errorf("event rows after second sweep = %d, want 1 (dedupe)", n)
	}
}

func TestSweepSkipsFilteredRuns(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	store := NewStore(pool)
	ctx := context.Background()

	lister := &fakeLister{runs: []RemoteRun{
		{ID: 9400, RunNumber: 4, HeadSHA: "e2f1111111111111111111111111111111111111",
			DisplayTitle: "Release v9.9", Event: "workflow_dispatch", Conclusion: "success"},
	}}
	queue := &fakeEnqueuer{}
	worker := NewReconcileWorker([]app.App{testApp}, lister, store, queue, noDefaultLogger())

	if err := worker.Work(ctx, &river.Job[ReconcileArgs]{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n := countRows(t, pool, "select count(*) from runs where workflow_run_id = 9400"); n != 0 {
		t.Errorf("filtered run must not create a run row, got %d", n)
	}
	if n := countRows(t, pool, "select count(*) from webhook_events where delivery_id = 'reconcile:9400'"); n != 0 {
		t.Errorf("filtered sweep must be skipped before ingest, got %d event rows", n)
	}
	if len(queue.args) != 0 {
		t.Errorf("filtered sweep must not enqueue, got %d jobs", len(queue.args))
	}
}
