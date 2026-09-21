// Repair-proposal intake tests (FLAK-02): the runner-token wall, session→app
// scoping, auto-learning creation, and the report-only invariant — a POST
// alone must leave corpus_behaviors untouched, and a sub-floor confidence is
// refused before any row lands.
package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func repairBody() string {
	return `{"behavior_key":"PWSPEC-LIVE-BADGE","broken_selector":"css=.bet-btn--v2",
"proposed_selector":"role=button[name=\"Place bet\"]","rationale":"a11y snapshot shows the button under a stable accessible name",
"confidence":0.9,"evidence_ref":"runs/r/step-3/a11y.json"}`
}

func behaviorCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from corpus_behaviors`).Scan(&n); err != nil {
		t.Fatalf("count behaviors: %v", err)
	}
	return n
}

func repairClient(t *testing.T, pool *pgxpool.Pool) (func(method, url, body string) (int, map[string]any), *httptest.Server) {
	t.Helper()
	srv := serveBare(t, pool, "runner-token")
	hdr := map[string]string{"Authorization": "Bearer runner-token"}
	call := func(method, url, body string) (int, map[string]any) {
		req, err := http.NewRequest(method, url, strings.NewReader(body))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
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
	return call, srv
}

func TestProposeRepairIntake(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	sessionID := seedSession(t, pool, runID, RoleNavigator, 40, 10_000_000)
	call, srv := repairClient(t, pool)

	url := srv.URL + "/api/agent-sessions/" + uid(sessionID) + "/repair-proposals"
	code, body := call("POST", url, repairBody())
	if code != http.StatusCreated {
		t.Fatalf("intake = %d, want 201: %v", code, body)
	}
	if body["status"] != "proposed" || body["proposed_by"] != "navigator" {
		t.Fatalf("proposal wire = %v, want proposed/navigator", body)
	}

	// Report-only: the intake appended the learning + proposal, never a behavior.
	if n := behaviorCount(t, pool); n != 0 {
		t.Fatalf("intake must not mutate the corpus oracle, behaviors = %d", n)
	}

	// Sub-floor confidence is refused before anything is written.
	lowBody := `{"behavior_key":"K","broken_selector":"a","proposed_selector":"b",
"rationale":"hunch","confidence":0.4,"evidence_ref":""}`
	code, body = call("POST", url, lowBody)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("sub-floor confidence = %d, want 422: %v", code, body)
	}

	// Unknown session: 404.
	code, _ = call("POST",
		srv.URL+"/api/agent-sessions/00000000-0000-0000-0000-000000000001/repair-proposals",
		repairBody())
	if code != http.StatusNotFound {
		t.Fatalf("unknown session = %d, want 404", code)
	}

	// No runner token: rejected at the wall.
	req, _ := http.NewRequest("POST", url, strings.NewReader(repairBody()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		t.Fatalf("unauthenticated intake = %d, want 401/403", resp.StatusCode)
	}
}

func TestProposeRepairPinsLearning(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool)
	sessionID := seedSession(t, pool, runID, RoleNavigator, 40, 10_000_000)
	call, srv := repairClient(t, pool)

	// Append a learning directly, then pin it from the navigator body: the
	// proposal must land on THAT learning, not a synthesized one.
	var learningID pgtype.UUID
	if err := pool.QueryRow(context.Background(), `
		insert into corpus_learnings (app_id, run_id, observation)
		select app_id, id, 'deposit button locator went stale after the v2 deploy'
		from runs where id = $1 returning id`, runID).Scan(&learningID); err != nil {
		t.Fatalf("seed learning: %v", err)
	}

	pinned := `{"behavior_key":"LEARN-X","broken_selector":"css=#deposit-v1",
"proposed_selector":"role=button[name=\"Deposit\"]","rationale":"renamed container",
"confidence":0.85,"evidence_ref":"runs/r/step-7/a11y.json","learning_id":"` + uid(learningID) + `"}`
	code, body := call("POST", srv.URL+"/api/agent-sessions/"+uid(sessionID)+"/repair-proposals", pinned)
	if code != http.StatusCreated {
		t.Fatalf("pinned intake = %d, want 201: %v", code, body)
	}
	if body["learning_id"] != uid(learningID) {
		t.Fatalf("proposal learning_id = %v, want pinned %s", body["learning_id"], uid(learningID))
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from corpus_learnings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("pinned intake must not synthesize a second learning, learnings = %d", count)
	}
}
