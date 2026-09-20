// REST surface tests (APPR-02): identity wall (401/403), fallback-identity
// refusal on decisions, the locked queue shape, and the decide round trip.
// Against scratch Postgres; fixtures seed rows directly (the sync pipeline
// is the plans package's test domain, not ours).
package approvals

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
)

func serve(t *testing.T, s *ApprovalsHTTP) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	s.Mount(mux)
	wrapped := httpx.IdentityWithConfig(httpx.IdentityConfig{DevFallbackUser: "tester"}, mux)
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv
}

// call issues one request through the identity middleware: user "" means
// no headers at all (unauthenticated), non-empty user sets X-User plus the
// given permission header.
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

const viewEdit = "admin.e2ai.view,admin.e2ai.edit"

func TestApprovalsHTTPIdentityWall(t *testing.T) {
	pool := testPool(t)
	srv := serve(t, NewApprovalsHTTP(pool, "tester"))

	// The dev-fallback middleware fabricates an identity for un-proxied
	// traffic (that behavior is TestApprovalsFallbackIdentityRefused's
	// subject), so 401 here needs a mux WITHOUT the identity middleware —
	// exactly what the api binary serves if wired without it.
	bare := httptest.NewServer(func() http.Handler {
		mux := http.NewServeMux()
		NewApprovalsHTTP(pool, "tester").Mount(mux)
		return mux
	}())
	t.Cleanup(bare.Close)

	if status, _ := call(t, "GET", bare.URL+"/api/approvals", "", "", ""); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated queue = %d, want 401", status)
	}
	if status, _ := call(t, "POST", bare.URL+"/api/plan-versions/"+uuid.NewString()+"/approve", "{}", "", ""); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated decide = %d, want 401", status)
	}
	// View-only identity: reads fine, decisions forbidden.
	if status, _ := call(t, "GET", srv.URL+"/api/approvals", "", "viewer", "admin.e2ai.view"); status != http.StatusOK {
		t.Errorf("view-only queue = %d, want 200", status)
	}
	if status, _ := call(t, "POST", srv.URL+"/api/plan-versions/"+uuid.NewString()+"/approve", "{}", "viewer", "admin.e2ai.view"); status != http.StatusForbidden {
		t.Errorf("view-only decide = %d, want 403", status)
	}
}

// TestApprovalsFallbackIdentityRefused pins the anti-fake-approver rule:
// the dev-fallback identity ("tester", and the UserOf "you" placeholder)
// must never be recorded as an approver, even with edit permissions.
func TestApprovalsFallbackIdentityRefused(t *testing.T) {
	pool := testPool(t)
	srv := serve(t, NewApprovalsHTTP(pool, "tester"))
	appID := seedTestApp(t, pool)
	seedPlan(t, pool, appID, "checkout-flow")
	vid := seedVersion(t, pool, "checkout-flow", "1.0.0", StatusPendingApproval)

	// No X-User at all: middleware resolves to the dev fallback.
	if status, _ := call(t, "POST", srv.URL+"/api/plan-versions/"+uuid.UUID(vid.Bytes).String()+"/approve", "{}", "tester", viewEdit); status != http.StatusBadRequest {
		t.Errorf("fallback-user decide = %d, want 400", status)
	}
	// X-User equal to the fallback value, forwarded by a proxied dev shell.
	if status, _ := call(t, "POST", srv.URL+"/api/plan-versions/"+uuid.UUID(vid.Bytes).String()+"/reject", "{}", "tester", viewEdit); status != http.StatusBadRequest {
		t.Errorf("explicit fallback decide = %d, want 400", status)
	}
	if got := versionStatus(t, pool, vid); got != StatusPendingApproval {
		t.Errorf("version status = %q, still pending wanted", got)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from plan_approvals`).Scan(&n); err != nil || n != 0 {
		t.Errorf("plan_approvals rows = %d (err %v), want 0", n, err)
	}
}

func TestApprovalsQueueShape(t *testing.T) {
	pool := testPool(t)
	srv := serve(t, NewApprovalsHTTP(pool, "tester"))
	appID := seedTestApp(t, pool)
	seedPlan(t, pool, appID, "checkout-flow")
	seedVersion(t, pool, "checkout-flow", "1.0.0", StatusApproved)
	seedVersion(t, pool, "checkout-flow", "1.1.0", StatusPendingApproval)
	seedPlan(t, pool, appID, "nightly-suite")
	seedVersion(t, pool, "nightly-suite", "1.0.0", StatusDraft)

	status, body := call(t, "GET", srv.URL+"/api/approvals", "", "alice", "admin.e2ai.view")
	if status != http.StatusOK {
		t.Fatalf("queue status = %d", status)
	}
	pending, ok := body["pending"].([]any)
	if !ok || len(pending) != 1 {
		t.Fatalf("pending = %v, want one entry", body["pending"])
	}
	entry := pending[0].(map[string]any)
	plan := entry["plan"].(map[string]any)
	if plan["id"] != "checkout-flow" || plan["app"] != "NSXBet/bet-app" {
		t.Errorf("plan = %v, want checkout-flow from NSXBet/bet-app", plan)
	}
	version := entry["version"].(map[string]any)
	if version["status"] != StatusPendingApproval {
		t.Errorf("version = %v, want pending_approval", version)
	}
	if version["id"] == "" || version["content_hash"] == "" {
		t.Errorf("version fields = %v, want id + content_hash", version)
	}
	recent, ok := body["recent"].([]any)
	if !ok || len(recent) != 0 {
		t.Errorf("recent = %v, want empty array", body["recent"])
	}
}

func TestApprovalsDecideRoundTrip(t *testing.T) {
	pool := testPool(t)
	srv := serve(t, NewApprovalsHTTP(pool, "tester"))
	appID := seedTestApp(t, pool)
	seedPlan(t, pool, appID, "checkout-flow")
	seedVersion(t, pool, "checkout-flow", "1.0.0", StatusApproved)
	pendingVID := seedVersion(t, pool, "checkout-flow", "1.1.0", StatusPendingApproval)
	otherVID := seedVersion(t, pool, "checkout-flow", "2.0.0", StatusPendingApproval)

	url := srv.URL + "/api/plan-versions/" + uuid.UUID(pendingVID.Bytes).String() + "/approve"
	status, body := call(t, "POST", url, `{"notes":"ships"}`, "alice", viewEdit)
	if status != http.StatusOK {
		t.Fatalf("approve status = %d body %v", status, body)
	}
	if body["decision"] != "approved" || body["approver"] != "alice" || body["notes"] != "ships" {
		t.Errorf("approval = %v", body)
	}
	if body["plan_id"] != "checkout-flow" {
		t.Errorf("plan_id = %v", body)
	}
	if body["plan_version_id"] != uuid.UUID(pendingVID.Bytes).String() {
		t.Errorf("plan_version_id = %v, want %s", body["plan_version_id"], uuid.UUID(pendingVID.Bytes).String())
	}

	// Wire side-effects landed: queue reflects the decision.
	status, body = call(t, "GET", srv.URL+"/api/approvals", "", "alice", "admin.e2ai.view")
	if status != http.StatusOK {
		t.Fatalf("queue status = %d", status)
	}
	recent := body["recent"].([]any)
	if len(recent) != 1 {
		t.Fatalf("recent = %v, want one", recent)
	}
	if got := recent[0].(map[string]any)["approver"]; got != "alice" {
		t.Errorf("recent approver = %v", got)
	}
	pending := body["pending"].([]any)
	if len(pending) != 1 || pending[0].(map[string]any)["version"].(map[string]any)["id"] != uuid.UUID(otherVID.Bytes).String() {
		t.Errorf("pending = %v, want only the other pending version", pending)
	}

	// Conflicts + misses map to their statuses.
	if status, _ := call(t, "POST", url, "{}", "alice", viewEdit); status != http.StatusConflict {
		t.Errorf("re-approve = %d, want 409", status)
	}
	if status, _ := call(t, "POST", srv.URL+"/api/plan-versions/"+uuid.UUID(otherVID.Bytes).String()+"/reject", "{}", "bob", viewEdit); status != http.StatusOK {
		t.Errorf("reject other = %d, want 200", status)
	}
	if status, _ := call(t, "POST", srv.URL+"/api/plan-versions/"+uuid.NewString()+"/approve", "{}", "alice", viewEdit); status != http.StatusNotFound {
		t.Errorf("unknown version = %d, want 404", status)
	}
	if status, _ := call(t, "POST", srv.URL+"/api/plan-versions/not-a-uuid/approve", "{}", "alice", viewEdit); status != http.StatusBadRequest {
		t.Errorf("bad uuid = %d, want 400", status)
	}

	// DB state: plan approved, decided versions rejected/approved.
	if got := planStatus(t, pool, "checkout-flow"); got != "approved" {
		t.Errorf("plan status = %q, want approved", got)
	}
	var approvals int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from plan_approvals`).Scan(&approvals); err != nil || approvals != 2 {
		t.Errorf("plan_approvals rows = %d (err %v), want 2", approvals, err)
	}
	var v2Status, otherStatus string
	if err := pool.QueryRow(context.Background(), `
		select status from plan_versions where id = $1`, pendingVID).Scan(&v2Status); err != nil {
		t.Fatal(err)
	}
	if v2Status != "approved" {
		t.Errorf("pendingVID status = %q", v2Status)
	}
	if err := pool.QueryRow(context.Background(), `
		select status from plan_versions where id = $1`, otherVID).Scan(&otherStatus); err != nil {
		t.Fatal(err)
	}
	if otherStatus != "rejected" {
		t.Errorf("otherVID status = %q", otherStatus)
	}
	if got := versionStatus(t, pool, otherVID); got != StatusRejected {
		t.Errorf("rejected version status = %q", got)
	}
}
