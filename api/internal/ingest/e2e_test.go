// T13 phase gate: the full deterministic loop, end to end, in one test
// against a real scratch Postgres and a fake GitHub. Webhook → HMAC verify →
// app match → filter → dedupe tx → rollout gate (health probe) → execute
// (fake container runner writing a real Playwright 1.58.2 fixture, enriched
// with one screenshot attachment) → verdict + report job → sticky PR comment
// + neutral check-run → runs/report/artifact HTTP surface. Every hop is the
// production code path; only the container (docker run) and the GitHub REST
// edge are faked.
package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	plapp "github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	plriver "github.com/NSXBet/e2ai/api/internal/platform/river"
	"github.com/NSXBet/e2ai/api/internal/report"
	"github.com/NSXBet/e2ai/api/internal/suite"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// e2eGitHub fakes the GitHub REST surface the loop touches: check-run
// create/patch, PR resolution by head SHA, and the sticky comment list/create.
// Every response advertises a healthy rate budget so the publisher's
// observational gate never trips.
type e2eGitHub struct {
	server *httptest.Server

	createBody    map[string]any // POST check-runs payload from the executor
	patchBody     map[string]any // PATCH check-runs payload from the report job
	commentIDs    []int64        // comment ids the publisher created
	commentBodies []string       // comment bodies the publisher sent

	mux *http.ServeMux
}

func newE2EGitHub(t *testing.T) *e2eGitHub {
	t.Helper()
	gh := &e2eGitHub{mux: http.NewServeMux()}

	rate := func(w http.ResponseWriter) {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "4000000000")
	}

	gh.mux.HandleFunc("POST /repos/NSXBet/bet-app/check-runs", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.createBody = body
		rate(w)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1001,"html_url":"https://github.test/check-runs/1001","status":"in_progress","conclusion":""}`))
	})
	gh.mux.HandleFunc("PATCH /repos/NSXBet/bet-app/check-runs/1001", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.patchBody = body
		rate(w)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1001,"html_url":"https://github.test/check-runs/1001","status":"completed","conclusion":"neutral"}`))
	})
	gh.mux.HandleFunc("GET /repos/NSXBet/bet-app/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		rate(w)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"number":77,"state":"open","title":"Deploy to staging","draft":false,"html_url":"https://github.test/pull/77"}]`))
	})
	gh.mux.HandleFunc("GET /repos/NSXBet/bet-app/issues/77/comments", func(w http.ResponseWriter, r *http.Request) {
		rate(w)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	gh.mux.HandleFunc("POST /repos/NSXBet/bet-app/issues/77/comments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gh.commentBodies = append(gh.commentBodies, fmt.Sprint(body["body"]))
		gh.commentIDs = append(gh.commentIDs, int64(5001+len(gh.commentIDs)-1+1))
		rate(w)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%d,"html_url":"https://github.test/pull/77#issuecomment-5001","created_at":"2026-09-20T00:00:00Z"}`, 5001)
	})

	gh.server = httptest.NewServer(gh.mux)
	t.Cleanup(gh.server.Close)
	return gh
}

// e2eCheckRuns adapts the github client onto suite.CheckRunCreator (the same
// alias the runner binary mounts; suite avoids importing the platform package).
type e2eCheckRuns struct{ gh *github.Client }

func (a e2eCheckRuns) CreateCheckRun(ctx context.Context, owner, repo string, in suite.CheckRunInput) (*suite.CheckRun, error) {
	cr, err := a.gh.CreateCheckRun(ctx, owner, repo, github.CreateCheckRunInput{
		Name:    in.Name,
		HeadSHA: in.HeadSHA,
		Status:  in.Status,
	})
	if err != nil {
		return nil, err
	}
	return &suite.CheckRun{ID: cr.ID}, nil
}

// e2eRunner stands in for the suite container: it writes the parsed-artifact
// view the executor expects into the mounted host directory — a real
// Playwright 1.58.2 fixture enriched with one screenshot attachment on the
// unexpected test's last failed attempt, plus the screenshot bytes on disk.
type e2eRunner struct {
	results func(mount string) error
}

func (r *e2eRunner) Run(ctx context.Context, spec suite.ContainerSpec) error {
	if len(spec.Mounts) != 1 {
		return fmt.Errorf("e2e runner: want 1 mount, got %d", len(spec.Mounts))
	}
	return r.results(spec.Mounts[0].Host)
}

// attachScreenshotToUnexpected walks the fixture and pins the screenshot
// attachment onto the last failed attempt of the unexpected (FAIL-class) test,
// so the comment render and artifact serving exercise a real failure's
// evidence.
func attachScreenshotToUnexpected(t *testing.T, raw []byte, rel string) []byte {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	var target map[string]any
	var walk func(n any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			if res, ok := v["results"].([]any); ok {
				if s, _ := v["status"].(string); s == "unexpected" {
					for _, r := range res {
						if rm, ok := r.(map[string]any); ok {
							if s2, _ := rm["status"].(string); s2 == "failed" {
								target = rm
							}
						}
					}
				}
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(doc)
	if target == nil {
		t.Fatal("fixture has no failed attempt under an unexpected test")
	}
	target["attachments"] = []map[string]string{{
		"name":        "screenshot",
		"contentType": "image/png",
		"path":        rel,
	}}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("remarshal fixture: %v", err)
	}
	return out
}

const e2eScreenshotRel = "screenshots/failed-home.png"

var e2ePNG, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

func TestE2EFullLoop(t *testing.T) {
	pool := dbPool(t)
	seedSchema(t, pool)
	ctx := context.Background()
	if err := plapp.MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("migrate river schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from river_job`); err != nil {
		t.Fatalf("clear river_job: %v", err)
	}

	// --- wiring: fake GitHub + report origins + local health probe ---
	gh := newE2EGitHub(t)
	secret := "e2ai-test-report-secret"
	reportBase := "https://reports.test"
	ghClient, err := github.NewWithBaseURL("pat-e2e", gh.server.URL)
	if err != nil {
		t.Fatalf("github client: %v", err)
	}
	publisher := report.NewPublisher(ghClient, reportBase, secret, noDefaultLogger())

	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer health.Close()

	e2eApp := testApp
	e2eApp.HealthURL = health.URL
	if _, err := plapp.SeedApps(ctx, pool, []plapp.App{e2eApp}); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	client, err := plriver.DefaultClient(pool, 1, plriver.WithQueues(map[string]river.QueueConfig{
		plriver.QueueDefault: {MaxWorkers: 1},
		plriver.QueueRunner:  {MaxWorkers: 1},
	}))
	if err != nil {
		t.Fatalf("river client: %v", err)
	}
	qr := &QueueRef{}
	qr.Set(client)

	// --- 1. ingest: webhook → dedupe tx → wait-rollout job ---
	handler := NewHandler(testSecret, []plapp.App{e2eApp}, NewStore(pool), qr, noDefaultLogger())
	payload := mustJSON(map[string]any{
		"workflow":     map[string]any{"name": e2eApp.WorkflowName},
		"workflow_run": dispatchRun(88001, "[BetWeb] Deploy to staging", "workflow_dispatch", "success"),
	})
	headers := map[string]string{
		"X-GitHub-Event":      "workflow_run",
		"X-GitHub-Delivery":   "d-e2e-full-loop",
		"X-Hub-Signature-256": sign(t, testSecret, payload),
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, post(payload, headers))
	if rr.Code != http.StatusOK {
		t.Fatalf("ingest: code = %d, want 200 (body %q)", rr.Code, rr.Body.String())
	}

	var runID uuid.UUID
	if err := pool.QueryRow(ctx, `select id from runs where workflow_run_id = 88001`).Scan(&runID); err != nil {
		t.Fatalf("load ingested run: %v", err)
	}
	var headSHA string
	if err := pool.QueryRow(ctx, `select head_sha from runs where id = $1`, runID).Scan(&headSHA); err != nil {
		t.Fatalf("load head sha: %v", err)
	}
	_ = headSHA

	// --- 2. rollout gate: health probe 2xx → provisioning + execute job ---
	gate := NewWaitRolloutWorker(pool, qr, noDefaultLogger())
	gate.budget, gate.pollInterval, gate.fallbackWait = 5*time.Second, 20*time.Millisecond, time.Minute
	if err := gate.Work(ctx, &river.Job[WaitRolloutArgs]{Args: WaitRolloutArgs{RunID: runID}}); err != nil {
		t.Fatalf("gate: %v", err)
	}
	if n, queue := enqueueCount(t, pool, suite.JobKindExecute); n != 1 || queue != plriver.QueueRunner {
		t.Fatalf("execute handoff = %d jobs on %q, want 1 on %q", n, queue, plriver.QueueRunner)
	}
	if s := runStatus(t, pool, runID); s != "provisioning" {
		t.Fatalf("run status after gate = %q, want provisioning", s)
	}

	// --- 3. execute: fake runner → parse → persist → report enqueue ---
	artifactsRoot := t.TempDir()
	executor := &suite.Executor{
		Pool:         pool,
		Image:        "e2ai-runner:test",
		ArtifactsDir: artifactsRoot,
		CheckRuns:    e2eCheckRuns{gh: ghClient},
		Runner: &e2eRunner{results: func(mount string) error {
			raw, err := os.ReadFile(filepath.Join("..", "suite", "testdata", "results.json"))
			if err != nil {
				return fmt.Errorf("read fixture: %w", err)
			}
			enriched := attachScreenshotToUnexpected(t, raw, e2eScreenshotRel)
			if err := os.MkdirAll(filepath.Join(mount, "screenshots"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(mount, "results.json"), enriched, 0o644); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(mount, e2eScreenshotRel), e2ePNG, 0o644)
		}},
		Queue:  client,
		Logger: noDefaultLogger(),
	}
	if err := executor.Work(ctx, &river.Job[suite.ExecuteArgs]{Args: suite.ExecuteArgs{RunID: runID.String()}}); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if crID := runCheckRunID(t, pool, runID); crID != 1001 {
		t.Fatalf("check_run_id = %d, want 1001", crID)
	}
	if n := countRows(t, pool, `select count(*) from test_results where run_id = $1`, runID); n != 4 {
		t.Fatalf("test_results rows = %d, want 4 (the fixture's four tests)", n)
	}

	// --- 4. report: verdict persist → completed → publisher handoff ---
	reportWorker := &report.Worker{Pool: pool, Logger: noDefaultLogger(), Publisher: publisher}
	if err := reportWorker.Work(ctx, &river.Job[report.ReportArgs]{Args: report.ReportArgs{RunID: runID.String()}}); err != nil {
		t.Fatalf("report: %v", err)
	}

	// Run row: completed, FAIL (the fixture carries one unexpected test),
	// counts 4/1/1/1/1, neutral check-run patched, report URL recorded.
	var status, verdict string
	var total, passed, failed, flaky, skipped int
	var checkRunID, durationMS int64
	var reportURL string
	if err := pool.QueryRow(ctx, `
		select status, verdict, coalesce(check_run_id, 0), total_tests, passed,
		       failed, flaky, skipped, coalesce(duration_ms, 0), coalesce(report_url, '')
		from runs where id = $1`, runID).
		Scan(&status, &verdict, &checkRunID, &total, &passed, &failed, &flaky, &skipped, &durationMS, &reportURL); err != nil {
		t.Fatalf("load completed run: %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
	if verdict != "fail" {
		t.Errorf("verdict = %q, want fail", verdict)
	}
	if total != 4 || passed != 1 || failed != 1 || flaky != 1 || skipped != 1 {
		t.Errorf("counts = %d/%d/%d/%d/%d, want 4/1/1/1/1", total, passed, failed, flaky, skipped)
	}
	if checkRunID != 1001 {
		t.Errorf("check_run_id = %d, want 1001", checkRunID)
	}
	wantLink := report.ReportLink(runID, reportBase, secret)
	if reportURL != wantLink {
		t.Errorf("report_url = %q, want %q", reportURL, wantLink)
	}

	// Check-run patched completed with a neutral conclusion (report-only).
	if gh.patchBody == nil {
		t.Fatal("check-run was never patched")
	}
	if c, _ := gh.patchBody["conclusion"].(string); c != "neutral" {
		t.Errorf("check-run conclusion = %v, want neutral", gh.patchBody["conclusion"])
	}

	// Sticky PR comment: marker-prefixed, carries the signed link and the
	// signed screenshot URL.
	var marker string
	if err := pool.QueryRow(ctx, `select comment_marker from registered_apps where owner = $1 and repo = $2`, e2eApp.Owner, e2eApp.Repo).Scan(&marker); err != nil {
		t.Fatalf("load comment marker: %v", err)
	}
	if len(gh.commentBodies) != 1 {
		t.Fatalf("publisher created %d comments, want 1", len(gh.commentBodies))
	}
	body := gh.commentBodies[0]
	if !strings.Contains(body, marker) {
		t.Errorf("comment body missing sticky marker %q", marker)
	}
	if !strings.Contains(body, wantLink) {
		t.Errorf("comment body missing signed report link %q", wantLink)
	}
	wantShot := fmt.Sprintf("/artifacts/%s/%s?e2ai=", runID, e2eScreenshotRel)
	if !strings.Contains(body, wantShot) {
		t.Errorf("comment body missing signed screenshot %q", wantShot)
	}

	// Job chain: exactly one execute handoff (runner queue) and one report
	// handoff (default queue) in river_job.
	if n, _ := enqueueCount(t, pool, suite.JobKindExecute); n != 1 {
		t.Errorf("execute jobs = %d, want 1", n)
	}
	if n, _ := enqueueCount(t, pool, suite.JobKindReport); n != 1 {
		t.Errorf("report jobs = %d, want 1", n)
	}

	// --- 5. runs/report/artifact HTTP surface (the wire contract) ---
	surface := report.NewRunsHTTP(pool, artifactsRoot, secret)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs", surface.ListRuns)
	mux.HandleFunc("GET /api/runs/{id}", surface.GetRun)
	mux.HandleFunc("GET /api/runs/{id}/report", surface.RunReport)
	mux.HandleFunc("GET /artifacts/{runID}/{path...}", surface.ServeArtifact)

	listToken := reportTokenFor(t, reportBase, secret, uuid.Nil)
	runToken := reportTokenFor(t, reportBase, secret, runID)

	// GET /api/runs: the run appears with its uppercase wire verdict.
	get := func(target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
	rec := get("/api/runs?limit=10&e2ai=" + listToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runs: code = %d (body %q)", rec.Code, rec.Body.String())
	}
	var page struct {
		Runs       []map[string]any `json:"runs"`
		Limit      int              `json:"limit"`
		Offset     int              `json:"offset"`
		NextOffset *int             `json:"next_offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode runs page: %v", err)
	}
	var wireRun map[string]any
	for _, r := range page.Runs {
		if r["id"] == runID.String() {
			wireRun = r
		}
	}
	if wireRun == nil {
		t.Fatalf("GET /api/runs missing run %s", runID)
	}
	if wireRun["verdict"] != "FAIL" {
		t.Errorf("wire verdict = %v, want FAIL", wireRun["verdict"])
	}
	if page.NextOffset != nil {
		t.Errorf("next_offset = %v, want nil (single run)", page.NextOffset)
	}

	// GET /api/runs/{id}: detail with 4 test results and the artifact pointer.
	rec = get("/api/runs/" + runID.String() + "?e2ai=" + runToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runs/{id}: code = %d (body %q)", rec.Code, rec.Body.String())
	}
	var detail struct {
		Run         map[string]any   `json:"run"`
		TestResults []map[string]any `json:"test_results"`
		Artifacts   []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode run detail: %v", err)
	}
	if len(detail.TestResults) != 4 {
		t.Errorf("detail test_results = %d, want 4", len(detail.TestResults))
	}
	if !artifactListed(detail.Artifacts, e2eScreenshotRel) {
		t.Errorf("detail artifacts missing %s (have %d rows)", e2eScreenshotRel, len(detail.Artifacts))
	}

	// Without a token and without identity the endpoint is gated.
	rec = get("/api/runs/" + runID.String())
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("ungated GET /api/runs/{id}: code = %d, want 401", rec.Code)
	}

	// GET /api/runs/{id}/report: failures-first page blob with the KPI counts.
	rec = get("/api/runs/" + runID.String() + "/report?e2ai=" + runToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET report: code = %d (body %q)", rec.Code, rec.Body.String())
	}
	var reportData struct {
		Run    map[string]any `json:"run"`
		Counts map[string]any `json:"counts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reportData); err != nil {
		t.Fatalf("decode report page: %v", err)
	}
	if reportData.Run["verdict"] != "FAIL" {
		t.Errorf("report verdict = %v, want FAIL", reportData.Run["verdict"])
	}
	for k, want := range map[string]float64{
		"total_tests": 4, "passed": 1, "failed": 1, "flaky": 1, "skipped": 1,
	} {
		if got, _ := reportData.Counts[k].(float64); got != want {
			t.Errorf("counts.%s = %v, want %v", k, reportData.Counts[k], want)
		}
	}

	// Artifact serving: the screenshot streams from the run's volume.
	rec = get("/artifacts/" + runID.String() + "/" + e2eScreenshotRel + "?e2ai=" + runToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET artifact: code = %d (body %q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("artifact content-type = %q, want image/png", ct)
	}
	if string(rec.Body.Bytes()) != string(e2ePNG) {
		t.Errorf("artifact body differs from the stored screenshot bytes")
	}
}

func reportTokenFor(t *testing.T, base, secret string, runID uuid.UUID) string {
	t.Helper()
	link := report.ReportLink(runID, base, secret)
	i := strings.Index(link, "?e2ai=")
	if i < 0 {
		t.Fatalf("report link %q carries no e2ai token", link)
	}
	return link[i+len("?e2ai="):]
}

func artifactListed(artifacts []map[string]any, rel string) bool {
	for _, a := range artifacts {
		if p, _ := a["path"].(string); p == rel {
			return true
		}
	}
	return false
}

// runStatus reads runs.status for one run.
func runStatus(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `select status from runs where id = $1`, runID).Scan(&s); err != nil {
		t.Fatalf("load run status: %v", err)
	}
	return s
}

// runCheckRunID reads runs.check_run_id (0 when unset).
func runCheckRunID(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `select coalesce(check_run_id, 0) from runs where id = $1`, runID).Scan(&id); err != nil {
		t.Fatalf("load check_run_id: %v", err)
	}
	return id
}
