// REST surface tests: identity wall (401/403), wire shapes locked to
// web/src/lib/types.ts, and the corpus decision endpoints. Against scratch
// Postgres; the seeded rows come from the sync test fixtures.
package plans

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// serve wires PlansHTTP behind the identity middleware with a dev fallback
// user (all permissions), exactly like the api binary does behind the BFF.
func serve(t *testing.T, s *PlansHTTP) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	s.Mount(mux)
	wrapped := httpx.IdentityWithConfig(httpx.IdentityConfig{DevFallbackUser: "tester"}, mux)
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	return resp.StatusCode, decoded
}

func postJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	return resp.StatusCode, decoded
}

// Seed one synced draft plan through the real pipeline so every read
// endpoint has consistent rows (plans + plan_versions + behavior).
func seedPlanSurface(t *testing.T, pool *pgxpool.Pool) (appID pgtype.UUID) {
	t.Helper()
	appID = plansTestApp(t, pool)
	seedVerifiedBehavior(t, pool, appID)
	client := fakePlansRepo(t, map[string]string{"checkout.md": validPlan})
	if _, err := SyncAppByID(context.Background(), pool, client, appID); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	return appID
}

// ListPlans returns the locked envelope {plans, limit, offset} with the
// synced plan present.
func TestPlansHTTPList(t *testing.T) {
	pool := plansTestPool(t)
	seedPlanSurface(t, pool)
	srv := serve(t, NewPlansHTTP(pool, nil))

	status, body := getJSON(t, srv.URL+"/api/plans")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	plans, ok := body["plans"].([]any)
	if !ok || len(plans) != 1 {
		t.Fatalf("plans = %v, want exactly the seeded plan", body["plans"])
	}
	plan := plans[0].(map[string]any)
	for _, key := range []string{"id", "app", "title", "surfaces", "trigger", "status", "source_path", "latest_version_id", "updated_at"} {
		if _, has := plan[key]; !has {
			t.Errorf("plan missing %q (web/src/lib/types.ts contract)", key)
		}
	}
	if plan["id"] != "checkout-flow-v1.0.0" {
		t.Errorf("id = %v, want checkout-flow-v1.0.0", plan["id"])
	}
	if _, has := body["limit"]; !has {
		t.Error("envelope missing limit")
	}
}

// GetPlan returns plan + version history; GetVersionContent returns raw
// content + frontmatter for the returned version.
func TestPlansHTTPDetailAndContent(t *testing.T) {
	pool := plansTestPool(t)
	seedPlanSurface(t, pool)
	srv := serve(t, NewPlansHTTP(pool, nil))

	status, body := getJSON(t, srv.URL+"/api/plans/checkout-flow-v1.0.0")
	if status != http.StatusOK {
		t.Fatalf("detail status = %d", status)
	}
	versions, ok := body["versions"].([]any)
	if !ok || len(versions) != 1 {
		t.Fatalf("versions = %v, want one", body["versions"])
	}
	version := versions[0].(map[string]any)
	if version["status"] != "draft" {
		t.Errorf("status = %v, want draft", version["status"])
	}
	vid, ok := version["id"].(string)
	if !ok || vid == "" {
		t.Fatalf("version id = %v, want uuid string", version["id"])
	}

	status, content := getJSON(t, srv.URL+"/api/plan-versions/"+vid+"/content")
	if status != http.StatusOK {
		t.Fatalf("content status = %d", status)
	}
	if c, _ := content["content"].(string); !strings.Contains(c, "balance widget is visible") {
		t.Errorf("content missing step judge text: %v", content["content"])
	}
}

func TestPlansHTTPNotFound(t *testing.T) {
	pool := plansTestPool(t)
	seedPlanSurface(t, pool)
	srv := serve(t, NewPlansHTTP(pool, nil))

	if status, _ := getJSON(t, srv.URL+"/api/plans/does-not-exist-v9.9.9"); status != http.StatusNotFound {
		t.Errorf("unknown plan status = %d, want 404", status)
	}
	if status, _ := getJSON(t, srv.URL + "/api/plan-versions/00000000-0000-0000-0000-000000000000/content"); status != http.StatusNotFound {
		t.Errorf("unknown version status = %d, want 404", status)
	}
}

// ListCorpus: behaviors + learnings for the app ref; a bad app ref is a 400,
// not a 500 (ResolveApp already validated).
func TestPlansHTTPCorpus(t *testing.T) {
	pool := plansTestPool(t)
	seedPlanSurface(t, pool)
	srv := serve(t, NewPlansHTTP(pool, nil))

	status, body := getJSON(t, srv.URL+"/api/corpus?app=NSXBet/bet-app")
	if status != http.StatusOK {
		t.Fatalf("corpus status = %d, body = %v", status, body)
	}
	behaviors, _ := body["behaviors"].([]any)
	if len(behaviors) != 1 {
		t.Errorf("behaviors = %v, want the seeded verified behavior", body["behaviors"])
	}
	if _, has := body["learnings"]; !has {
		t.Error("envelope missing learnings")
	}

	if status, _ := getJSON(t, srv.URL+"/api/corpus?app=NSXBet/unknown-app"); status != http.StatusBadRequest {
		t.Errorf("unknown app status = %d, want 400", status)
	}
	if status, _ := getJSON(t, srv.URL+"/api/corpus"); status != http.StatusBadRequest {
		t.Errorf("missing app param status = %d, want 400", status)
	}
}

// A learning round-trips promote/dismiss through the corpus decision
// endpoints — status moves, provenance preserved.
func TestPlansHTTPCorpusDecisions(t *testing.T) {
	pool := plansTestPool(t)
	appID := seedPlanSurface(t, pool)
	srv := serve(t, NewPlansHTTP(pool, nil))

	var runID pgtype.UUID
	if err := pool.QueryRow(context.Background(), `
		insert into runs (app_id, workflow_run_id, run_number, environment, head_sha)
		values ($1, 4242, 1, 'staging', 'deadbeef') returning id`, appID).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	learning, err := corpus.AppendLearning(context.Background(), pool, appID, runID, "flaky badge on /live page")
	if err != nil {
		t.Fatalf("append learning: %v", err)
	}
	learningID := uuid.UUID(learning.ID.Bytes).String()
	t.Logf("DEBUG learning id=%s valid=%v", learningID, learning.ID.Valid)

	status, body := postJSON(t, srv.URL+"/api/corpus/learnings/"+learningID+"/promote")
	if status != http.StatusOK {
		t.Fatalf("promote status = %d, body = %v", status, body)
	}
	var promotedID string
	if err := pool.QueryRow(context.Background(),
		`select id from corpus_behaviors where provenance->>'learning_id' = $1`, learningID).Scan(&promotedID); err != nil {
		t.Fatalf("promoted behavior missing: %v", err)
	}

	// A promoted learning is terminal; dismiss targets a second learning.
	status, _ = postJSON(t, srv.URL+"/api/corpus/learnings/"+learningID+"/dismiss")
	if status != http.StatusConflict {
		t.Errorf("dismiss on promoted learning status = %d, want 409", status)
	}
	learning2, err := corpus.AppendLearning(context.Background(), pool, appID, runID, "second observation")
	if err != nil {
		t.Fatalf("append learning 2: %v", err)
	}
	learning2ID := uuid.UUID(learning2.ID.Bytes).String()
	status, _ = postJSON(t, srv.URL+"/api/corpus/learnings/"+learning2ID+"/dismiss")
	if status != http.StatusOK {
		t.Errorf("dismiss status = %d", status)
	}
	var statusText string
	if err := pool.QueryRow(context.Background(),
		`select status from corpus_learnings where id::text = $1`, learning2ID).Scan(&statusText); err != nil {
		t.Fatalf("learning row: %v", err)
	}
	if statusText != "dismissed" {
		t.Errorf("learning status = %q, want dismissed", statusText)
	}
}

// The identity wall: no BFF headers and no dev fallback -> 401; dev fallback
// without the needed permission tier -> 403.
func TestPlansHTTPIdentityWall(t *testing.T) {
	pool := plansTestPool(t)
	seedPlanSurface(t, pool)

	s := NewPlansHTTP(pool, (*github.Client)(nil))
	mux := http.NewServeMux()
	s.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if status, _ := getJSON(t, srv.URL+"/api/plans"); status != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", status)
	}
}
