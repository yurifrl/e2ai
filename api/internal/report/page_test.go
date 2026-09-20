// Tests for T11's report page assembly and runs HTTP surface: pure enum
// mapping, the locked wire envelope, and DB-backed endpoint round-trips
// (list pagination, detail, report blob, gate) against the scratch Postgres.
// Database tests skip unless E2AI_TEST_DATABASE_URL is set (same convention
// as internal/runs, whose Migrate applies the schema here).
package report

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/NSXBet/e2ai/api/internal/runs"
)

// wireEnvelope is the exact locked runs-page envelope shape; typed decoding
// proves the keys exist with the right names.
type wireEnvelope struct {
	Runs       []map[string]json.RawMessage `json:"runs"`
	Limit      int                          `json:"limit"`
	Offset     int                          `json:"offset"`
	NextOffset *int                         `json:"next_offset"`
}

// seedApp inserts a registered app; names vary so unique constraints hold.
func seedApp(t *testing.T, pool *pgxpool.Pool, name string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker)
		values ('NSXBet', $1, $2, '.github/workflows/deploy.yaml',
		        'staging', '[BetWeb] Deploy to staging', 'op://vault/secret', $3)
		returning id
	`, "repo-"+name, name, "<!-- e2ai:"+name+" -->").Scan(&id); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return id
}

// seedRun inserts one runs row with an explicit created_at (ListRuns orders
// newest first, created_at desc tie-broken by id desc).
func seedRun(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, workflowRunID int64, createdAt time.Time, status runs.Status) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, head_sha,
		                  head_branch, environment, trigger_source, status, created_at)
		values ($1, $2, 1, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
		        'main', 'staging', 'webhook', $3, $4)
		returning id
	`, appID, workflowRunID, status, createdAt).Scan(&id); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

// seedTestResult inserts one test_results row for the run with two
// attachments (a screenshot and a trace).
func seedTestResult(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, title, project, outcome string) {
	t.Helper()
	// Attachment paths must be sanitizer-safe ([a-zA-Z0-9._-] segments) —
	// slug the title or shotPaths filters the screenshot out.
	slug := strings.Map(func(r rune) rune {
		ok := r == '-' || r == '.' || r == '_' ||
			r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if ok {
			return r
		}
		return '-'
	}, title)
	atts := fmt.Sprintf(`[
		{"kind":"screenshot","path":"screenshots/%s.png"},
		{"kind":"trace","path":"traces/%s.zip"}
	]`, slug, slug)
	if _, err := pool.Exec(context.Background(), `
		insert into test_results (run_id, suite, title, file, line, project,
		                          outcome, status, attempts, duration_ms,
		                          error_message, classification, attachments)
		values ($1, 'checkout', $2, 'tests/checkout.spec.ts', 12, $3,
		        $4, 'passed', 1, 1200, null, '', $5)
	`, runID, title, project, outcome, atts); err != nil {
		t.Fatalf("seed test result: %v", err)
	}
}

// seedArtifact inserts one artifacts row for the run.
func seedArtifact(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, kind, path string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into artifacts (run_id, kind, path, size_bytes) values ($1, $2, $3, 100)
	`, runID, kind, path); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
}

// newSurface builds the HTTP surface over the pool and a mux registering the
// locked patterns exactly as main.go does.
func newSurface(t *testing.T, pool *pgxpool.Pool, secret string) (http.Handler, *RunsHTTP) {
	t.Helper()
	surface := NewRunsHTTP(pool, t.TempDir(), secret)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", surface.ListRuns)
	mux.HandleFunc("GET /api/runs/{id}", surface.GetRun)
	mux.HandleFunc("GET /api/runs/{id}/report", surface.RunReport)
	mux.HandleFunc("GET /artifacts/{runID}/{path...}", surface.ServeArtifact)
	return mux, surface
}

// asIdentity wraps the handler the way main.go does (BFF identity middleware)
// and injects the run-view identity headers, simulating a proxied admin call.
func asIdentity(next http.Handler) http.Handler {
	return httpx.IdentityWithConfig(httpx.IdentityConfig{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(httpx.UserHeader, "yuri")
		r.Header.Set(httpx.PermissionsHeader, string(httpx.RunViewPermission))
		next.ServeHTTP(w, r)
	}))
}

// do issues a request through the handler and returns status + body.
func do(t *testing.T, handler http.Handler, method, target string) (int, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec.Code, rec.Body.Bytes()
}

// randomUUID returns a random valid uuid (noise ids for 404 cases).
func randomUUID() uuid.UUID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return uuid.UUID(b)
}

// mustParseUUID parses a seeded id string or fails the test.
func mustParseUUID(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}

// TestWireMapping pins the DB → wire enum conversions.
func TestWireMapping(t *testing.T) {
	if got := wireStatus(runs.StatusWaitingRollout); got != "WAITING_ROLLOUT" {
		t.Errorf("status waiting-rollout: got %s", got)
	}
	if got := wireStatus(runs.StatusCompleted); got != "COMPLETED" {
		t.Errorf("status completed: got %s", got)
	}
	for db, want := range map[string]string{
		"expected":   "PASS",
		"unexpected": "FAIL",
		"flaky":      "FLAKY",
		"skipped":    "SKIPPED",
	} {
		if got := wireOutcome(db); got != want {
			t.Errorf("outcome %s: got %s want %s", db, got, want)
		}
	}
	for db, want := range map[string]string{
		"results_json": "results",
		"screenshot":   "screenshot",
		"trace":        "trace",
		"video":        "video",
		"log":          "results",
	} {
		if got := wireArtifactKind(db); got != want {
			t.Errorf("kind %s: got %s want %s", db, got, want)
		}
	}
	if got := wireVerdict(pgtype.Text{String: "fail", Valid: true}); got == nil || *got != "FAIL" {
		t.Errorf("verdict fail: got %v", got)
	}
	if got := wireVerdict(pgtype.Text{}); got != nil {
		t.Errorf("null verdict: got %v", got)
	}
}

// TestReportEnvelopeIsWireContract proves the HTTP envelope carries exactly
// the locked {"runs", limit, offset, next_offset} keys.
func TestReportEnvelopeIsWireContract(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	handler, _ := newSurface(t, pool, "")
	status, body := do(t, asIdentity(handler), "GET", "/api/runs?limit=5&offset=0")
	if status != http.StatusOK {
		t.Fatalf("list: status %d body %s", status, body)
	}
	var page map[string]json.RawMessage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page) != 4 {
		t.Fatalf("envelope keys: got %d (%v), want exactly runs/limit/offset/next_offset", len(page), page)
	}
	for _, key := range []string{"runs", "limit", "offset", "next_offset"} {
		if _, ok := page[key]; !ok {
			t.Errorf("missing envelope key %s", key)
		}
	}
	var env wireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode typed envelope: %v", err)
	}
	if env.Limit != 5 || env.Offset != 0 || env.Runs == nil || env.NextOffset != nil {
		t.Errorf("envelope values: %+v", env)
	}
}

// TestListRunsPaginationAndClamp checks newest-first ordering, offset paging,
// next_offset, and the 100 limit clamp.
func TestListRunsPaginationAndClamp(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	handler, _ := newSurface(t, pool, "")
	ident := asIdentity(handler)
	appID := seedApp(t, pool, "page")
	base := time.Now().Add(-time.Hour)
	for i := range 3 {
		seedRun(t, pool, appID, int64(100+i), base.Add(time.Duration(i)*time.Minute), runs.StatusCompleted)
	}
	seedRun(t, pool, appID, 200, base.Add(-2*time.Minute), runs.StatusFailed)

	// Newest first: the three i-runs in reverse, then run 200.
	status, body := do(t, ident, "GET", "/api/runs?limit=4&offset=0")
	if status != http.StatusOK {
		t.Fatalf("list: %d %s", status, body)
	}
	var env wireEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Runs) != 4 {
		t.Fatalf("runs: got %d want 4", len(env.Runs))
	}
	if env.NextOffset == nil || *env.NextOffset != 4 {
		t.Errorf("next_offset: %+v", env.NextOffset)
	}
	status, body = do(t, ident, "GET", "/api/runs?limit=4&offset=4")
	if status != http.StatusOK {
		t.Fatalf("page 2: %d %s", status, body)
	}
	env = wireEnvelope{}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if len(env.Runs) != 0 || env.NextOffset != nil {
		t.Errorf("page 2 beyond end: %+v next %v", env.Runs, env.NextOffset)
	}

	// Clamp: limit=500 serves 100.
	status, body = do(t, ident, "GET", "/api/runs?limit=500&offset=0")
	if status != http.StatusOK {
		t.Fatalf("clamped list: %d %s", status, body)
	}
	env = wireEnvelope{}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode clamped: %v", err)
	}
	if env.Limit != 100 {
		t.Errorf("clamp: limit=%d want 100", env.Limit)
	}
}

// TestRunDetailAndNotFound checks the detail endpoint round-trip and 404 on
// an unknown run id.
func TestRunDetailAndNotFound(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	handler, _ := newSurface(t, pool, "")
	ident := asIdentity(handler)
	appID := seedApp(t, pool, "detail")
	runRow := seedRun(t, pool, appID, 42, time.Now().Add(-time.Minute), runs.StatusCompleted)
	runID := mustParseUUID(t, runRow.String())
	seedTestResult(t, pool, runID, "checkout works", "chromium", "expected")
	seedArtifact(t, pool, runID, "results_json", "results.json")

	status, body := do(t, ident, "GET", "/api/runs/"+runID.String())
	if status != http.StatusOK {
		t.Fatalf("detail: %d %s", status, body)
	}
	var detail struct {
		Run         map[string]any   `json:"run"`
		TestResults []map[string]any `json:"test_results"`
		Artifacts   []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Run["id"] != runID.String() || detail.Run["status"] != "COMPLETED" {
		t.Errorf("run: %v", detail.Run)
	}
	if len(detail.TestResults) != 1 || detail.TestResults[0]["outcome"] != "PASS" {
		t.Errorf("test results: %v", detail.TestResults)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0]["kind"] != "results" {
		t.Errorf("artifacts: %v", detail.Artifacts)
	}

	status, _ = do(t, ident, "GET", "/api/runs/"+randomUUID().String())
	if status != http.StatusNotFound {
		t.Errorf("unknown run: status %d want 404", status)
	}
	status, _ = do(t, ident, "GET", "/api/runs/not-a-uuid")
	if status != http.StatusNotFound {
		t.Errorf("bad uuid: status %d want 404", status)
	}
}

// TestReportBlob verifies the report page assembly on a seeded run: KPI
// counts, failures first on the filmstrip, projects, and screenshot artifact
// URLs.
func TestReportBlob(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	handler, _ := newSurface(t, pool, "")
	ident := asIdentity(handler)
	appID := seedApp(t, pool, "report")
	runRow := seedRun(t, pool, appID, 7, time.Now().Add(-time.Minute), runs.StatusCompleted)
	runID := mustParseUUID(t, runRow.String())

	seedTestResult(t, pool, runID, "passes cleanly", "chromium", "expected")
	seedTestResult(t, pool, runID, "flakes sometimes", "chromium", "flaky")
	seedTestResult(t, pool, runID, "fails hard", "webkit", "unexpected")

	status, body := do(t, ident, "GET", "/api/runs/"+runID.String()+"/report")
	if status != http.StatusOK {
		t.Fatalf("report: %d %s", status, body)
	}
	var blob struct {
		Run      map[string]any   `json:"run"`
		Counts   map[string]int   `json:"counts"`
		Projects []string         `json:"projects"`
		Failures []map[string]any `json:"failures"`
		Flaky    []map[string]any `json:"flaky"`
		Steps    []struct {
			TestID      string   `json:"test_id"`
			Title       string   `json:"title"`
			Outcome     string   `json:"outcome"`
			Screenshots []string `json:"screenshots"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(body, &blob); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if blob.Counts["total_tests"] != 3 || blob.Counts["passed"] != 1 ||
		blob.Counts["failed"] != 1 || blob.Counts["flaky"] != 1 {
		t.Errorf("counts: %v", blob.Counts)
	}
	if len(blob.Projects) != 2 {
		t.Errorf("projects: %v", blob.Projects)
	}
	if len(blob.Failures) != 1 || blob.Failures[0]["title"] != "fails hard" {
		t.Errorf("failures: %v", blob.Failures)
	}
	if len(blob.Flaky) != 1 || blob.Flaky[0]["title"] != "flakes sometimes" {
		t.Errorf("flaky: %v", blob.Flaky)
	}
	// Filmstrip: failures first (FAIL, FLAKY, PASS), each step carrying the
	// deterministic screenshots as /e2ai/artifacts/{runID}/... URLs.
	if len(blob.Steps) != 3 {
		t.Fatalf("steps: %v", blob.Steps)
	}
	if blob.Steps[0].Outcome != "FAIL" || blob.Steps[1].Outcome != "FLAKY" || blob.Steps[2].Outcome != "PASS" {
		t.Errorf("filmstrip order: %s, %s, %s", blob.Steps[0].Outcome, blob.Steps[1].Outcome, blob.Steps[2].Outcome)
	}
	for i, want := range []string{"fails hard", "flakes sometimes", "passes cleanly"} {
		if blob.Steps[i].Title != want {
			t.Errorf("step %d: got %s want %s", i, blob.Steps[i].Title, want)
		}
	}
	prefix := "/e2ai/artifacts/" + runID.String() + "/screenshots/"
	if len(blob.Steps[0].Screenshots) != 1 {
		t.Fatalf("failed step screenshots: %v", blob.Steps[0].Screenshots)
	}
	if got := blob.Steps[0].Screenshots[0]; got != prefix+"fails-hard.png" {
		t.Errorf("screenshot url: got %s want %s", got, prefix+"fails-hard.png")
	}
}

// TestReportGate is the token posture: a valid signed ?e2ai= token reaches
// the report endpoint without any identity; a bad token without identity is
// 401; an identity lacking the view permission is 403.
func TestReportGate(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	secret := "gate-test-secret"
	handler, _ := newSurface(t, pool, secret)
	appID := seedApp(t, pool, "gate")
	runRow := seedRun(t, pool, appID, 9, time.Now().Add(-time.Minute), runs.StatusCompleted)
	runID := mustParseUUID(t, runRow.String())
	seedTestResult(t, pool, runID, "fails", "chromium", "unexpected")

	// Token without identity: the PR-comment link path.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/runs/"+runID.String()+"/report?e2ai="+reportToken(runID, secret), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("token access: %d %s", rec.Code, rec.Body.String())
	}

	// Bad token, no identity → 401.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/runs/"+runID.String()+"/report?e2ai=deadbeef", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad token: %d want 401", rec.Code)
	}

	// Identity without the view permission → 403.
	limited := httpx.IdentityWithConfig(httpx.IdentityConfig{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(httpx.UserHeader, "rando")
		handler.ServeHTTP(w, r)
	}))
	rec = httptest.NewRecorder()
	limited.ServeHTTP(rec, httptest.NewRequest("GET", "/api/runs/"+runID.String()+"/report", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("permission-less identity: %d want 403", rec.Code)
	}
}
