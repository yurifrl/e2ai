// Tests for the T5 MCP surface: the protocol round-trips (initialize, ping,
// tools/list, notifications) and DB-backed tool behavior against the scratch
// Postgres. Database tests skip unless E2AI_TEST_DATABASE_URL is set (same
// convention as internal/runs and internal/agents).
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/flakes"
	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/NSXBet/e2ai/api/internal/report"
	"github.com/NSXBet/e2ai/api/internal/trends"
)

const (
	testRunnerToken = "test-runner-token"
	testLinkSecret  = "test-link-secret"
	testReportBase  = "https://admin.example.com"
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
		t.Fatalf("open scratch db: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedApp inserts a registered app with a unique identity and returns its id.
func seedApp(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker)
		values ($1, $2, $3, 'w.yaml', 'staging', '[X] Deploy', 'op://x', 'marker')
		returning id
	`, "acme", "repo-"+fmt.Sprint(time.Now().UnixNano()), "Release - X").Scan(&id)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return id
}

// seedRun inserts a run with controlled verdict/counts and a staggered
// created_at so list ordering is deterministic.
func seedRun(t *testing.T, pool *pgxpool.Pool, appID pgtype.UUID, n int, status, verdict string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, head_sha, environment,
		                  trigger_source, status, verdict, total_tests, passed, failed, flaky, skipped,
		                  created_at)
		values ($1, $2, $3, 'cafe1234', 'staging', 'webhook', $4, $5,
		        2, 1, 1, 0, 0,
		        now() - ((4 - $6) * interval '1 minute'))
		returning id
	`, appID, 9000+n, n, status, verdict, n).Scan(&id)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func seedTest(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, title, outcome, classification, errMsg string) {
	t.Helper()
	status := "failed"
	if outcome == "expected" || outcome == "flaky" {
		status = "passed" // flaky ends passed on a retry (Playwright semantics)
	}
	_, err := pool.Exec(context.Background(), `
		insert into test_results (run_id, title, file, project, outcome, status, attempts,
		                          error_message, classification)
		values ($1, $2, 'tests/x.spec.ts', 'chromium', $3, $6, 1, $4, $5)
	`, runID, title, outcome, errMsg, classification, status)
	if err != nil {
		t.Fatalf("seed test: %v", err)
	}
}

func seedArtifact(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, kind, path string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		insert into artifacts (run_id, kind, path, size_bytes)
		values ($1, $2, $3, 42)
	`, runID, kind, path)
	if err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
}

// --- harness ---

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newSurface(pool *pgxpool.Pool) *Surface {
	return New(pool, testRunnerToken, testReportBase, testLinkSecret)
}

func mcpMux(pool *pgxpool.Pool, runnerToken string) *http.ServeMux {
	mux := http.NewServeMux()
	New(pool, runnerToken, testReportBase, testLinkSecret).Mount(mux)
	return mux
}

func rpcBody(id any, method string, params any) string {
	parts := []string{`"jsonrpc":"2.0"`}
	if id != nil {
		parts = append(parts, fmt.Sprintf(`"id":%s`, mustJSON(id)))
	}
	if method != "" {
		parts = append(parts, fmt.Sprintf(`"method":%s`, mustJSON(method)))
	}
	if params != nil {
		parts = append(parts, fmt.Sprintf(`"params":%s`, mustJSON(params)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func doRPC(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, h, http.MethodPost, body, nil)
}

// doRequest posts one request to the surface as the runner service token
// unless a tokenless/identity variant is requested via opts.
func doRequest(t *testing.T, h http.Handler, method, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if headers == nil {
		req.Header.Set("Authorization", "Bearer "+testRunnerToken)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) rpcEnvelope {
	t.Helper()
	var env rpcEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode rpc response: %v (%s)", err, rec.Body.String())
	}
	return env
}

// callTool invokes a tool via the harness and returns the parsed
// structuredContent of the result (nil when the result carried an error).
func callTool(t *testing.T, h http.Handler, name string, args any) (json.RawMessage, rpcEnvelope) {
	t.Helper()
	body := rpcBody(1, "tools/call", map[string]any{"name": name, "arguments": args})
	rec := doRPC(t, h, body)
	env := decodeEnvelope(t, rec)
	if env.Error != nil {
		return nil, env
	}
	var result struct {
		IsError            bool            `json:"isError"`
		StructuredContent  json.RawMessage `json:"structuredContent"`
		Content            []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(env.Result, &result); err != nil {
		t.Fatalf("decode tool result: %v (%s)", err, env.Result)
	}
	if result.IsError {
		return nil, env
	}
	return result.StructuredContent, env
}

// --- protocol round-trips ---

func TestInitializeAndToolsList(t *testing.T) {
	h := mcpMux(nil, testRunnerToken)

	rec := doRPC(t, h, rpcBody(1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "clientInfo": map[string]string{"name": "t"},
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize status = %d", rec.Code)
	}
	env := decodeEnvelope(t, rec)
	if env.Error != nil {
		t.Fatalf("initialize error: %+v", env.Error)
	}
	var init initializeResult
	if err := json.Unmarshal(env.Result, &init); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if init.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion = %q", init.ProtocolVersion)
	}
	if _, ok := init.Capabilities["tools"]; !ok {
		t.Errorf("capabilities missing tools: %v", init.Capabilities)
	}
	if init.ServerInfo["name"] != "e2ai-mcp" {
		t.Errorf("serverInfo name = %v", init.ServerInfo["name"])
	}
	if !strings.Contains(init.Instructions, "read-only") {
		t.Errorf("instructions = %q, want read-only posture", init.Instructions)
	}

	rec = doRPC(t, h, rpcBody(2, "tools/list", nil))
	env = decodeEnvelope(t, rec)
	var list struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(env.Result, &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	want := map[string]bool{
		"list_runs": true, "get_run": true, "get_failure_evidence": true,
		"get_flake_rates": true, "get_quarantine": true,
	}
	if len(list.Tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(list.Tools), len(want))
	}
	for _, tool := range list.Tools {
		name, _ := tool["name"].(string)
		if !want[name] {
			t.Errorf("unexpected tool %q", name)
		}
		if tool["inputSchema"] == nil {
			t.Errorf("tool %q missing inputSchema", name)
		}
	}

	rec = doRPC(t, h, rpcBody(3, "ping", nil))
	env = decodeEnvelope(t, rec)
	if env.Error != nil || rec.Code != http.StatusOK {
		t.Fatalf("ping: status=%d err=%+v", rec.Code, env.Error)
	}

	// Notification: accepted with 202, empty body.
	rec = doRPC(t, h, rpcBody(nil, "notifications/initialized", nil))
	if rec.Code != http.StatusAccepted {
		t.Errorf("notification status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("notification body = %q, want empty", rec.Body.String())
	}

	// GET (the spec's SSE stream) is unsupported: 405.
	rec = doRequest(t, h, http.MethodGet, "", map[string]string{
		"Authorization": "Bearer " + testRunnerToken,
	})
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp status = %d, want 405", rec.Code)
	}
}

func TestProtocolErrors(t *testing.T) {
	h := mcpMux(nil, testRunnerToken)

	cases := []struct {
		name, body string
		wantCode   int
	}{
		{"malformed json", "{not json", -32700},
		{"not 2.0", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, -32600},
		{"unknown method", rpcBody(1, "no-such-method", nil), -32601},
		{"unknown tool", rpcBody(1, "tools/call", map[string]any{"name": "no-such-tool"}), -32602},
		{"missing tool name", rpcBody(1, "tools/call", map[string]any{}), -32602},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRPC(t, h, tt.body)
			env := decodeEnvelope(t, rec)
			if env.Error == nil {
				t.Fatalf("no error for %s (status %d)", tt.name, rec.Code)
			}
			if env.Error.Code != tt.wantCode {
				t.Errorf("code = %d, want %d (%s)", env.Error.Code, tt.wantCode, env.Error.Message)
			}
		})
	}
}

func TestAuthGate(t *testing.T) {
	h := mcpMux(nil, testRunnerToken)

	// No token and no identity: 401 pre-JSON-RPC.
	rec := doRequest(t, h, http.MethodPost, rpcBody(1, "ping", nil), map[string]string{"Authorization": ""})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no auth status = %d, want 401", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error == nil {
		t.Error("no auth response lacks rpc error")
	}

	// Wrong token: 401.
	rec = doRequest(t, h, http.MethodPost, rpcBody(1, "ping", nil), map[string]string{"Authorization": "Bearer wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token status = %d, want 401", rec.Code)
	}

	// BFF identity without the view permission: 403.
	deny := httpx.IdentityWithConfig(httpx.IdentityConfig{}, mcpMux(nil, ""))
	rec = doRequest(t, deny, http.MethodPost, rpcBody(1, "ping", nil), map[string]string{
		httpx.UserHeader: "yuri", httpx.PermissionsHeader: "admin.e2ai.edit",
	})
	if rec.Code != http.StatusForbidden {
		t.Errorf("permissionless identity status = %d, want 403", rec.Code)
	}

	// BFF identity with the view permission: admitted.
	allow := httpx.IdentityWithConfig(httpx.IdentityConfig{}, mcpMux(nil, ""))
	rec = doRequest(t, allow, http.MethodPost, rpcBody(1, "ping", nil), map[string]string{
		httpx.UserHeader: "yuri", httpx.PermissionsHeader: string(httpx.RunViewPermission),
	})
	if rec.Code != http.StatusOK {
		t.Errorf("run-view identity status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// --- DB-backed tool behavior ---

func TestListRuns(t *testing.T) {
	pool := testPool(t)
	appID := seedApp(t, pool)
	failRun := seedRun(t, pool, appID, 1, "completed", "fail")
	seedRun(t, pool, appID, 2, "completed", "pass")
	execRun := seedRun(t, pool, appID, 3, "executing", "")

	h := mcpMux(pool, testRunnerToken)

	raw, env := callTool(t, h, "list_runs", map[string]any{"app_id": uuidString(appID)})
	if raw == nil {
		t.Fatalf("list_runs errored: %+v", env.Result)
	}
	var got struct {
		AppID string `json:"app_id"`
		Runs  []struct {
			RunID   string  `json:"run_id"`
			App     string  `json:"app"`
			Status  string  `json:"status"`
			Verdict *string `json:"verdict"`
			Counts  *struct {
				Failed int `json:"failed"`
			} `json:"counts"`
			Evidence struct {
				ReportURL string `json:"report_url"`
			} `json:"evidence"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode list_runs: %v (%s)", err, raw)
	}
	if got.AppID != uuidString(appID) {
		t.Errorf("app_id = %q", got.AppID)
	}
	if len(got.Runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(got.Runs))
	}
	// Newest first (created_at stagger).
	if got.Runs[0].RunID != uuidString(execRun) {
		t.Errorf("order: first = %q, want executing run", got.Runs[0].RunID)
	}
	// Every run carries the evidence link (the report page serves any run
	// state); token verifies even before the run has been reported.
	if got.Runs[0].Evidence.ReportURL == "" {
		t.Error("unreported run missing report evidence link")
	}
	if !strings.HasPrefix(got.Runs[2].App, "acme/") {
		t.Errorf("app label = %q", got.Runs[2].App)
	}
	if got.Runs[2].Counts == nil || got.Runs[2].Counts.Failed != 1 {
		t.Errorf("fail run counts = %+v, want failed=1", got.Runs[2].Counts)
	}

	// The evidence link is token-gated and verifies like the report gate.
	link := got.Runs[2].Evidence.ReportURL
	base := testReportBase + "/e2ai/runs/" + uuidString(failRun)
	if !strings.HasPrefix(link, base+"?e2ai=") {
		t.Fatalf("report link = %q, want %q + token", link, base)
	}
	token := strings.TrimPrefix(link, base+"?e2ai=")
	if !report.VerifyReportToken(pgID(t, failRun), token, testLinkSecret) {
		t.Errorf("report link token does not verify: %q", link)
	}

	// verdict filter narrows to the failing run.
	raw, _ = callTool(t, h, "list_runs", map[string]any{"app_id": uuidString(appID), "verdict": "fail"})
	var filtered struct {
		Runs []struct {
			RunID string `json:"run_id"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &filtered); err != nil {
		t.Fatalf("decode filtered list: %v", err)
	}
	if len(filtered.Runs) != 1 || filtered.Runs[0].RunID != uuidString(failRun) {
		t.Errorf("verdict=fail runs = %+v, want only %s", filtered.Runs, uuidString(failRun))
	}
}

func TestGetRunClassificationReason(t *testing.T) {
	pool := testPool(t)
	appID := seedApp(t, pool)
	runID := seedRun(t, pool, appID, 1, "completed", "fail")
	seedTest(t, pool, runID, "checkout", "unexpected", "app_bug", "expect(page).toContainText failed")
	seedTest(t, pool, runID, "login", "expected", "", "")
	seedArtifact(t, pool, runID, "screenshot", "shots/checkout.png")

	h := mcpMux(pool, testRunnerToken)
	raw, env := callTool(t, h, "get_run", map[string]any{"run_id": uuidString(runID)})
	if raw == nil {
		t.Fatalf("get_run errored: %+v", env.Result)
	}
	var got struct {
		Tests []struct {
			Title                string   `json:"title"`
			Classification       *string  `json:"classification"`
			ClassificationReason *string  `json:"classification_reason"`
			ClassificationNote   *string  `json:"classification_note"`
			ErrorMessage         *string  `json:"error_message"`
			Attachments          []struct {
				URL string `json:"url"`
			} `json:"attachments"`
		} `json:"tests"`
		Artifacts []struct {
			URL string `json:"url"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode get_run: %v (%s)", err, raw)
	}
	if len(got.Tests) != 2 || len(got.Artifacts) != 1 {
		t.Fatalf("tests=%d artifacts=%d, want 2/1", len(got.Tests), len(got.Artifacts))
	}

	byTitle := map[string]int{}
	for i, tr := range got.Tests {
		byTitle[tr.Title] = i
	}

	// REPT-02: the classified test carries a plain-language reason derived
	// from the class and the stored error message.
	co := got.Tests[byTitle["checkout"]]
	if co.Classification == nil || *co.Classification != "app_bug" {
		t.Errorf("checkout classification = %v, want app_bug", co.Classification)
	}
	if co.ClassificationReason == nil {
		t.Fatal("checkout missing classification_reason (REPT-02)")
	}
	if !strings.Contains(*co.ClassificationReason, "app_bug") ||
		!strings.Contains(*co.ClassificationReason, "toContainText failed") {
		t.Errorf("reason = %q, want class + error message", *co.ClassificationReason)
	}
	if co.ClassificationNote != nil {
		t.Errorf("classified test should not carry a note: %q", *co.ClassificationNote)
	}
	if len(co.Attachments) != 0 {
		t.Errorf("checkout attachments = %+v, want none", co.Attachments)
	}

	// The unclassified test: null reason plus the explicit none note.
	lo := got.Tests[byTitle["login"]]
	if lo.ClassificationReason != nil {
		t.Errorf("unclassified reason = %q, want null", *lo.ClassificationReason)
	}
	if lo.ClassificationNote == nil || *lo.ClassificationNote != "classification: none" {
		t.Errorf("note = %v, want classification: none", lo.ClassificationNote)
	}

	// Artifact evidence links are token-gated and verify.
	link := got.Artifacts[0].URL
	base := testReportBase + "/e2ai/artifacts/" + uuidString(runID) + "/shots/checkout.png"
	if !strings.HasPrefix(link, base+"?e2ai=") {
		t.Fatalf("artifact link = %q, want %q + token", link, base)
	}
	if !report.VerifyReportToken(pgID(t, runID), strings.TrimPrefix(link, base+"?e2ai="), testLinkSecret) {
		t.Errorf("artifact link token does not verify: %q", link)
	}
}

func TestGetFailureEvidence(t *testing.T) {
	pool := testPool(t)
	appID := seedApp(t, pool)
	runID := seedRun(t, pool, appID, 1, "completed", "fail")
	seedTest(t, pool, runID, "checkout", "unexpected", "app_bug", "boom")
	seedTest(t, pool, runID, "cart", "flaky", "agent_flake", "timed out")
	seedTest(t, pool, runID, "login", "expected", "", "")

	h := mcpMux(pool, testRunnerToken)

	raw, env := callTool(t, h, "get_failure_evidence", map[string]any{"run_id": uuidString(runID)})
	if raw == nil {
		t.Fatalf("get_failure_evidence errored: %+v", env.Result)
	}
	var got struct {
		RunID    string `json:"run_id"`
		Verdict  *string `json:"verdict"`
		Evidence struct {
			ReportURL string `json:"report_url"`
		} `json:"evidence"`
		Failures []struct {
			Title string `json:"title"`
			Outcome string `json:"outcome"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if got.RunID != uuidString(runID) {
		t.Errorf("run_id = %q", got.RunID)
	}
	if got.Verdict == nil || *got.Verdict != "fail" {
		t.Errorf("verdict = %v, want fail", got.Verdict)
	}
	if len(got.Failures) != 2 {
		t.Fatalf("failures = %d, want 2 (unexpected + flaky, not the pass)", len(got.Failures))
	}
	for _, f := range got.Failures {
		if f.Title == "login" {
			t.Errorf("passed test leaked into failures: %+v", f)
		}
	}
	if got.Evidence.ReportURL == "" {
		t.Error("missing report evidence link")
	}

	// Exact-title filter narrows to one.
	raw, _ = callTool(t, h, "get_failure_evidence", map[string]any{"run_id": uuidString(runID), "title": "cart"})
	var filtered struct {
		Failures []struct {
			Title string `json:"title"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(raw, &filtered); err != nil {
		t.Fatalf("decode filtered: %v", err)
	}
	if len(filtered.Failures) != 1 || filtered.Failures[0].Title != "cart" {
		t.Errorf("title filter = %+v, want cart", filtered.Failures)
	}

	// Unknown run: an isError tool result (domain failure, not a protocol error).
	raw, env = callTool(t, h, "get_failure_evidence", map[string]any{"run_id": uuid.UUID{}.String()})
	if raw != nil || env.Error != nil {
		t.Fatalf("unknown run: raw=%s err=%+v, want isError result", raw, env.Error)
	}
	var res struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(env.Result, &res); err != nil || !res.IsError {
		t.Errorf("unknown run result = %s, want isError", env.Result)
	}
}

func TestGetFlakeRates(t *testing.T) {
	pool := testPool(t)
	appID := seedApp(t, pool)
	runA := seedRun(t, pool, appID, 1, "completed", "pass")
	runB := seedRun(t, pool, appID, 2, "completed", "fail")
	seedTest(t, pool, runA, "checkout", "flaky", "agent_flake", "")
	seedTest(t, pool, runB, "checkout", "unexpected", "app_bug", "")
	seedTest(t, pool, runB, "stable", "expected", "", "")
	seedRun(t, pool, appID, 3, "executing", "") // not verdict-bearing: outside the window

	h := mcpMux(pool, testRunnerToken)
	raw, env := callTool(t, h, "get_flake_rates", map[string]any{"app_id": uuidString(appID)})
	if raw == nil {
		t.Fatalf("get_flake_rates errored: %+v", env.Result)
	}
	var got struct {
		Window int `json:"window"`
		Rates  []struct {
			Title      string  `json:"title"`
			WindowRuns int     `json:"window_runs"`
			FlakyRuns  int     `json:"flaky_runs"`
			FailRuns   int     `json:"fail_runs"`
			PassRuns   int     `json:"pass_runs"`
			Rate       float64 `json:"rate"`
			Unstable   bool    `json:"unstable"`
		} `json:"rates"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	// The response echoes the requested window (default); the window bounds
	// the query, and only verdict-bearing runs land in the rates.
	if got.Window != trends.DefaultWindow {
		t.Errorf("window = %d, want default %d", got.Window, trends.DefaultWindow)
	}
	if len(got.Rates) != 2 {
		t.Fatalf("rates = %d, want 2", len(got.Rates))
	}
	byTitle := map[string]int{}
	for i, r := range got.Rates {
		byTitle[r.Title] = i
	}
	co := got.Rates[byTitle["checkout"]]
	if co.WindowRuns != 2 || co.FlakyRuns != 1 || co.FailRuns != 1 {
		t.Errorf("checkout counts = %+v", co)
	}
	if co.Rate != 0.5 || !co.Unstable {
		t.Errorf("checkout rate = %v unstable=%v, want 0.5/true", co.Rate, co.Unstable)
	}
	st := got.Rates[byTitle["stable"]]
	if st.FailRuns != 0 || st.Unstable {
		t.Errorf("stable counts = %+v", st)
	}
}

func TestGetQuarantine(t *testing.T) {
	pool := testPool(t)
	appID := seedApp(t, pool)
	ctx := context.Background()
	if _, err := flakes.Activate(ctx, pool, appID, "tests/x.spec.ts", "checkout",
		"flaky 3 of 5 runs", "runs 12,14 flaky", flakes.ByHuman, nil); err != nil {
		t.Fatalf("seed quarantine: %v", err)
	}

	h := mcpMux(pool, testRunnerToken)
	raw, env := callTool(t, h, "get_quarantine", map[string]any{"app_id": uuidString(appID)})
	if raw == nil {
		t.Fatalf("get_quarantine errored: %+v", env.Result)
	}
	var got struct {
		Entries []struct {
			AppID    string `json:"app_id"`
			Title    string `json:"title"`
			Reason   string `json:"reason"`
			Status   string `json:"status"`
			By       string `json:"quarantined_by"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if len(got.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(got.Entries))
	}
	e := got.Entries[0]
	if e.Title != "checkout" || e.Reason != "flaky 3 of 5 runs" || e.Status != "active" {
		t.Errorf("entry = %+v", e)
	}
	if e.By != flakes.ByHuman {
		t.Errorf("quarantined_by = %q", e.By)
	}
	if e.AppID != uuidString(appID) {
		t.Errorf("app_id = %q", e.AppID)
	}

	// status filter round-trips and rejects unknown statuses (-32602).
	raw, _ = callTool(t, h, "get_quarantine", map[string]any{"app_id": uuidString(appID), "status": "active"})
	var filtered struct {
		Entries []struct {
			Status string `json:"status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &filtered); err != nil {
		t.Fatalf("decode filtered: %v", err)
	}
	if len(filtered.Entries) != 1 || filtered.Entries[0].Status != "active" {
		t.Errorf("status=active entries = %+v", filtered.Entries)
	}

	_, env = callTool(t, h, "get_quarantine", map[string]any{"app_id": uuidString(appID), "status": "bogus"})
	if env.Error == nil || env.Error.Code != -32602 {
		t.Errorf("bogus status: err = %+v, want -32602", env)
	}
}

// pgID converts a pgtype.UUID for the report token verifier.
func pgID(t *testing.T, id pgtype.UUID) uuid.UUID {
	t.Helper()
	out, err := uuid.Parse(uuidString(id))
	if err != nil {
		t.Fatalf("parse uuid: %v", err)
	}
	return out
}
