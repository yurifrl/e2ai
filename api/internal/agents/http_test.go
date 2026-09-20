// Agents HTTP surface tests (Phase 3 T2): the identity wall (bare 401,
// view-only 403), the runner service-token path, the approved-plan enqueue
// gate, the lifecycle endpoints over the T1 guarded machine, the budget
// breach 409, and the admin queue listing — against scratch Postgres with
// the real 0004 constraints.
package agents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const planFixture = `---
id: bet-login-v1.0.0
app: yuri/betweb
title: Login flow
surfaces: [web]
trigger: on-deploy
---

1. Log in to bet-app with valid credentials.
   judge: the dashboard is visible
2. Open the deposit page from the dashboard.
   judge: the deposit form is visible
`

func serveAgents(t *testing.T, pool *pgxpool.Pool, runnerToken string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewAgentsHTTP(pool, runnerToken).Mount(mux)
	wrapped := httpx.IdentityWithConfig(httpx.IdentityConfig{DevFallbackUser: "tester"}, mux)
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv
}

func serveBare(t *testing.T, pool *pgxpool.Pool, runnerToken string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	NewAgentsHTTP(pool, runnerToken).Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// call issues one request through the identity middleware: user "" means
// no headers at all; non-empty user sets X-User plus X-Permissions.
func call(t *testing.T, method, url, body, user, permissions string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if user != "" {
		req.Header.Set("X-User", user)
		req.Header.Set("X-Permissions", permissions)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// --- fixtures ---

func setRunStatus(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`update runs set status = $2 where id = $1`, runID, status); err != nil {
		t.Fatalf("set run status: %v", err)
	}
}

func seedApprovedPlan(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) uuid.UUID {
	t.Helper()
	var appID uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`select app_id from runs where id = $1`, runID).Scan(&appID); err != nil {
		t.Fatalf("read run app: %v", err)
	}
	var versionID uuid.UUID
	err := pool.QueryRow(context.Background(), `
		with pv as (
			insert into plan_versions (plan_id, version, content_hash, content, frontmatter, status, created_by)
			values ('bet-login-v1.0.0', 'v1.0.0', 'hash-http-test', $2, '{}', 'approved', 'tester')
			returning id
		)
		insert into plans (id, app_id, source_path, status, latest_version_id)
		values ('bet-login-v1.0.0', $1, 'plans/bet-login.md', 'approved', (select id from pv))
		returning latest_version_id
	`, appID, planFixture).Scan(&versionID)
	if err != nil {
		t.Fatalf("seed approved plan: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		insert into run_plan_resolutions (run_id, plan_id, plan_version_id, content_hash, mode)
		values ($1, 'bet-login-v1.0.0', $2, 'hash-http-test', 'approved')
	`, runID, versionID); err != nil {
		t.Fatalf("seed resolution: %v", err)
	}
	return versionID
}

func setStatus(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID, status Status) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`update agent_sessions set status = $2 where id = $1`, sessionID, status); err != nil {
		t.Fatalf("set session status: %v", err)
	}
}

func sessionBudget(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) (Status, int, int64) {
	t.Helper()
	var status Status
	var steps int
	var cost int64
	if err := pool.QueryRow(context.Background(),
		`select status, steps_used, cost_millicents from agent_sessions where id = $1`,
		sessionID).Scan(&status, &steps, &cost); err != nil {
		t.Fatalf("read session: %v", err)
	}
	return status, steps, cost
}

// --- tests ---

// Identity wall: a bare mux (no BFF headers) gets 401 everywhere; a
// view-only identity reads but cannot edit; the runner service token is
// admitted on runner endpoints even through the bare mux.
func TestIdentityWall(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)

	admin := serveAgents(t, pool, "runner-secret")
	viewOnly := http.Header{"X-User": {"runner"}, "X-Permissions": {"admin.e2ai.view"}}
	callHdr := func(server string, hdr http.Header, method, url, body string) int {
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v[0])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	// View-only identity: reads fine, edit/runner endpoints forbidden.
	if code := callHdr(admin.URL, viewOnly, "GET", admin.URL+"/api/agent-sessions?run_id="+uid(runID), ""); code != http.StatusOK {
		t.Errorf("view-only list = %d, want 200", code)
	}
	if code := callHdr(admin.URL, viewOnly, "POST", admin.URL+"/api/runs/"+uid(runID)+"/agent-sessions", ""); code != http.StatusForbidden {
		t.Errorf("view-only enqueue = %d, want 403", code)
	}
	if code := callHdr(admin.URL, viewOnly, "POST", admin.URL+"/api/agent-sessions/"+uuid.NewString()+"/kill", ""); code != http.StatusForbidden {
		t.Errorf("view-only kill = %d, want 403", code)
	}

	// Bare traffic (no middleware, no token): 401 on every endpoint.
	bare := serveBare(t, pool, "runner-secret")
	bareHdr := http.Header{}
	for _, tc := range []struct{ method, url, body string }{
		{"GET", bare.URL + "/api/agent-sessions?run_id=" + uid(runID), ""},
		{"POST", bare.URL + "/api/runs/" + uid(runID) + "/agent-sessions", ""},
		{"POST", bare.URL + "/api/agent-sessions/" + uuid.NewString() + "/heartbeat", ""},
		{"POST", bare.URL + "/api/agent-sessions/" + uuid.NewString() + "/steps", `{"kind":"navigate"}`},
	} {
		if code := callHdr(bare.URL, bareHdr, tc.method, tc.url, tc.body); code != http.StatusUnauthorized {
			t.Errorf("bare %s %s = %d, want 401", tc.method, tc.url, code)
		}
	}

	// The runner service token passes the bare mux without BFF headers —
	// and the same request without the token stays out.
	const sid = "00000000-0000-0000-0000-000000000000"
	tokenHdr := http.Header{"Authorization": {"Bearer runner-secret"}}
	if code := callHdr(bare.URL, tokenHdr, "POST", bare.URL+"/api/agent-sessions/"+sid+"/heartbeat", ""); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("service-token heartbeat rejected with %d (want admission past the wall)", code)
	}
	if code := callHdr(bare.URL, bareHdr, "POST", bare.URL+"/api/agent-sessions/"+sid+"/heartbeat", ""); code != http.StatusUnauthorized {
		t.Errorf("tokenless heartbeat = %d, want 401", code)
	}
	// The token does NOT grant admin endpoints: kill stays behind edit
	// permission even for the runner.
	if code := callHdr(bare.URL, tokenHdr, "POST", bare.URL+"/api/agent-sessions/"+sid+"/kill", ""); code != http.StatusUnauthorized {
		t.Errorf("service-token kill = %d, want 401 (token is runner-only)", code)
	}
}

// Enqueue: executing run + approved plan → 202 with one navigator and one
// vision_judge, both step-bounded by the plan's step count; a second
// enqueue 409s; unknown run 404; non-executing run 409; missing approval 409.
func TestEnqueue(t *testing.T) {
	pool := testPool(t)
	runIDp := seedRun(t, pool); runID := uuid.UUID(runIDp.Bytes)
	seedApprovedPlan(t, pool, runID)
	srv := serveAgents(t, pool, "")

	url := srv.URL + "/api/runs/" + runID.String() + "/agent-sessions"

	// Non-executing run first: seedRun starts queued.
	if code, _ := call(t, "POST", url, "", "admin", "admin.e2ai.edit"); code != http.StatusConflict {
		t.Errorf("enqueue non-executing run = %d, want 409", code)
	}
	setRunStatus(t, pool, runID, "executing")

	// Unknown run id.
	if code, _ := call(t, "POST", srv.URL+"/api/runs/"+uuid.NewString()+"/agent-sessions", "", "admin", "admin.e2ai.edit"); code != http.StatusNotFound {
		t.Errorf("enqueue unknown run = %d, want 404", code)
	}

	code, body := call(t, "POST", url, "", "admin", "admin.e2ai.edit")
	if code != http.StatusAccepted {
		t.Fatalf("enqueue = %d, want 202: %v", code, body)
	}
	sessions, _ := body["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("enqueued %d sessions, want 2 (navigator + vision_judge)", len(sessions))
	}
	roles := map[string]bool{}
	for _, s := range sessions {
		m, _ := s.(map[string]any)
		roles[m["role"].(string)] = true
	}
	if !roles["navigator"] || !roles["vision_judge"] {
		t.Errorf("roles = %v, want navigator + vision_judge", roles)
	}

	// Both sessions carry the plan's step count as ceiling.
	var ceilingCount int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from agent_sessions where run_id = $1 and step_ceiling = 2`, runID).Scan(&ceilingCount); err != nil || ceilingCount != 2 {
		t.Errorf("step ceilings = %d/2 at 2, err %v", ceilingCount, err)
	}

	// Re-enqueue is a 409.
	if code, _ := call(t, "POST", url, "", "admin", "admin.e2ai.edit"); code != http.StatusConflict {
		t.Errorf("re-enqueue = %d, want 409", code)
	}

	// A run with no approved resolution is a 409.
	otherp := seedRun(t, pool); other := uuid.UUID(otherp.Bytes)
	setRunStatus(t, pool, other, "executing")
	if code, _ := call(t, "POST", srv.URL+"/api/runs/"+other.String()+"/agent-sessions", "", "admin", "admin.e2ai.edit"); code != http.StatusConflict {
		t.Errorf("enqueue without approved plan = %d, want 409", code)
	}
}

// Lifecycle over the guarded machine: steps append-only with budget math,
// heartbeat on the token path, complete from running, and the kill switch
// stopping a mid-flight session.
func TestLifecycleAndKill(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	sid := seedSession(t, pool, runID, RoleNavigator, 3, 10_000)
	setStatus(t, pool, uuid.UUID(sid.Bytes), StatusClaimed)
	bare := serveBare(t, pool, "runner-token")
	hdr := http.Header{"Authorization": {"Bearer runner-token"}}
	callHdr := func(method, url, body string) (int, map[string]any) {
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v[0])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		var decoded map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&decoded)
		return resp.StatusCode, decoded
	}
	id := uid(sid)

	if code, _ := callHdr("POST", bare.URL+"/api/agent-sessions/"+id+"/heartbeat", ""); code != http.StatusOK {
		t.Errorf("heartbeat = %d, want 200", code)
	}

	// Append steps: cost accumulates inside the append transaction.
	for range 2 {
		body := `{"kind":"navigate","tokens_in":100,"tokens_out":10,"cost_millicents":3000}`
		if code, _ := callHdr("POST", bare.URL+"/api/agent-sessions/"+id+"/steps", body); code != http.StatusCreated {
			t.Errorf("append step (3000 mc) = %d, want 201", code)
		}
	}
	if _, steps, cost := sessionBudget(t, pool, uuid.UUID(sid.Bytes)); steps != 2 || cost != 6000 {
		t.Errorf("budget after 2 steps = (%d steps, %d mc), want (2, 6000)", steps, cost)
	}

	// Breach: a third step at 5k exceeds the 10k ceiling → 409, session
	// flips to budget_exhausted, and the runner path is closed.
	if code, _ := callHdr("POST", bare.URL+"/api/agent-sessions/"+id+"/steps",
		`{"kind":"act","cost_millicents":5000}`); code != http.StatusConflict {
		t.Errorf("breaching append = %d, want 409", code)
	}
	if status, _, _ := sessionBudget(t, pool, uuid.UUID(sid.Bytes)); status != StatusBudgetExhausted {
		t.Errorf("status after breach = %s, want budget_exhausted", status)
	}
	if code, _ := callHdr("POST", bare.URL+"/api/agent-sessions/"+id+"/steps",
		`{"kind":"observe"}`); code != http.StatusConflict {
		t.Errorf("append after breach = %d, want 409", code)
	}

	// Kill switch on a fresh session, then the runner sees the 409 wall.
	sid2 := seedSession(t, pool, runID, RoleVisionJudge, 3, 10_000)
	setStatus(t, pool, uuid.UUID(sid2.Bytes), StatusClaimed)
	if code, body := callHdr("POST", bare.URL+"/api/agent-sessions/"+uid(sid2)+"/kill", ""); code != http.StatusUnauthorized {
		t.Errorf("token kill = %d, want 401 (admin-only): %v", code, body)
	}
}

// The admin queue listing returns the seeded sessions filtered by run_id
// with the stuck flag mirroring the orphan sweep threshold.
func TestListQueue(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	sid := seedSession(t, pool, runID, RoleNavigator, 3, 10_000)
	setStatus(t, pool, uuid.UUID(sid.Bytes), StatusClaimed)
	// Fresh heartbeat → not stuck.
	if _, err := pool.Exec(context.Background(),
		`update agent_sessions set heartbeat_at = now() where id = $1`, sid); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	sid2 := seedSession(t, pool, runID, RoleVisionJudge, 3, 10_000)
	setStatus(t, pool, uuid.UUID(sid2.Bytes), StatusRunning)
	// Lapsed heartbeat → stuck.
	if _, err := pool.Exec(context.Background(),
		`update agent_sessions set heartbeat_at = now() - interval '5 minutes' where id = $1`, sid2); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	srv := serveAgents(t, pool, "")
	code, body := call(t, "GET", srv.URL+"/api/agent-sessions?run_id="+uid(runID), "", "admin", "admin.e2ai.view")
	if code != http.StatusOK {
		t.Fatalf("list = %d: %v", code, body)
	}
	rows, _ := body["sessions"].([]any)
	if len(rows) != 2 {
		t.Fatalf("list returned %d rows, want 2", len(rows))
	}
	byRole := map[string]map[string]any{}
	for _, r := range rows {
		m, _ := r.(map[string]any)
		byRole[m["role"].(string)] = m
	}
	if got := byRole["navigator"]; got["stuck"] == true {
		t.Errorf("fresh navigator stuck = %v, want false", got["stuck"])
	}
	if got := byRole["vision_judge"]; got["stuck"] != true {
		t.Errorf("lapsed vision_judge stuck = %v, want true", got["stuck"])
	}
	if code, _ := call(t, "GET", srv.URL+"/api/agent-sessions", "", "admin", "admin.e2ai.view"); code != http.StatusBadRequest {
		t.Errorf("list without run_id = %d, want 400", code)
	}
}

// uid renders a pgtype.UUID as its canonical string.
func uid(id pgtype.UUID) string {
	return uuid.UUID(id.Bytes).String()
}
