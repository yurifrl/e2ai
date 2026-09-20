// Tests for the phase 5 agentic trajectory slice of the report blob:
// deterministic runs load to null keys, agent runs assemble the session
// trajectory in idx order with token-gated evidence URLs, the wireMaxLimit
// truncation keeps the most recent steps, and the cost summary sums every
// session. Same scratch Postgres convention as the rest of the package.
package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/runs"
)

// seedSession inserts one agent session and returns its id.
func seedSession(t *testing.T, pool *pgxpool.Pool, runID pgtype.UUID, role, status string, stepsUsed int, cost int64) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(context.Background(), `
		insert into agent_sessions (run_id, role, status, steps_used, cost_millicents)
		values ($1, $2, $3, $4, $5)
		returning id
	`, runID, role, status, stepsUsed, cost).Scan(&id); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

// seedStep inserts one append-only trajectory step.
func seedStep(t *testing.T, pool *pgxpool.Pool, sessionID pgtype.UUID, idx int, kind string, decision, action, evidence string, tokensIn, tokensOut int, cost int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into session_steps (session_id, idx, kind, decision, action, evidence, tokens_in, tokens_out, cost_millicents)
		values ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7, $8, $9)
	`, sessionID, idx, kind, decision, action, evidence, tokensIn, tokensOut, cost); err != nil {
		t.Fatalf("seed step %d: %v", idx, err)
	}
}

// TestReportBlobDeterministicRun pins the phase 5 wire contract's
// backwards-compatibility half: a run with no agent sessions keeps the
// trajectory and agent_summary keys null.
func TestReportBlobDeterministicRun(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	handler, _ := newSurface(t, pool, "")
	ident := asIdentity(handler)
	appID := seedApp(t, pool, "trajectory")
	runRow := seedRun(t, pool, appID, 11, time.Now().Add(-time.Minute), runs.StatusCompleted)
	runID := mustParseUUID(t, runRow.String())
	seedTestResult(t, pool, runID, "passes cleanly", "chromium", "expected")

	status, body := do(t, ident, "GET", "/api/runs/"+runID.String()+"/report")
	if status != http.StatusOK {
		t.Fatalf("report: %d %s", status, body)
	}
	var blob map[string]any
	if err := json.Unmarshal(body, &blob); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	for _, key := range []string{"trajectory", "agent_summary"} {
		v, ok := blob[key]
		if !ok {
			t.Errorf("%s: key missing from wire blob", key)
			continue
		}
		if v != nil {
			t.Errorf("%s: got %v want null", key, v)
		}
	}
}

// TestReportBlobTrajectory seeds a two-session agent run and verifies the
// full wire round-trip: step order by idx, evidence URL stamping with
// traversal-safe filtering, and the cost summary.
func TestReportBlobTrajectory(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	handler, _ := newSurface(t, pool, "")
	ident := asIdentity(handler)
	appID := seedApp(t, pool, "trajectory-agent")
	runRow := seedRun(t, pool, appID, 12, time.Now().Add(-time.Minute), runs.StatusExecuting)
	runID := mustParseUUID(t, runRow.String())
	seedTestResult(t, pool, runID, "fails hard", "webkit", "unexpected")

	navigator := seedSession(t, pool, runRow, "navigator", "completed", 2, 500)
	judge := seedSession(t, pool, runRow, "vision_judge", "running", 1, 250)

	seedStep(t, pool, navigator, 0, "navigate",
		`{"url":"/login"}`, `{"opened":"/login"}`,
		`[{"kind":"screenshot","path":"agents/n1/screenshots/step-0.png"}]`, 1200, 45, 100)
	seedStep(t, pool, navigator, 1, "assert",
		`{"expected":"session cookie"}`, `{"asserted":true}`,
		`[{"kind":"screenshot","path":"../escape.png"},{"kind":"a11y","path":"agents/n1/a11y/step-1.json"}]`, 800, 30, 400)
	seedStep(t, pool, judge, 0, "judge",
		`{"verdict":"fail"}`, `{"reported":true}`,
		`[{"kind":"screenshot","path":"agents/j1/screenshots/step-4.png"}]`, 600, 20, 250)

	status, body := do(t, ident, "GET", "/api/runs/"+runID.String()+"/report")
	if status != http.StatusOK {
		t.Fatalf("report: %d %s", status, body)
	}
	var blob struct {
		Trajectory []struct {
			SessionID      string `json:"session_id"`
			Role           string `json:"role"`
			Status         string `json:"status"`
			StepsUsed      int    `json:"steps_used"`
			CostMillicents int64  `json:"cost_millicents"`
			Truncated      bool   `json:"truncated"`
			Steps          []struct {
				Idx      int    `json:"idx"`
				Kind     string `json:"kind"`
				Decision any    `json:"decision"`
				Action   any    `json:"action"`
				Evidence []struct {
					Kind string  `json:"kind"`
					Path string  `json:"path"`
					URL  *string `json:"url"`
				} `json:"evidence"`
				TokensIn        int    `json:"tokens_in"`
				TokensOut       int    `json:"tokens_out"`
				CostMillicents  int64  `json:"cost_millicents"`
				CreatedAt       string `json:"created_at"`
			} `json:"steps"`
		} `json:"trajectory"`
		AgentSummary *struct {
			TotalCostMillicents int64 `json:"total_cost_millicents"`
			Sessions            []struct {
				Role           string `json:"role"`
				Status         string `json:"status"`
				CostMillicents int64  `json:"cost_millicents"`
			} `json:"sessions"`
		} `json:"agent_summary"`
	}
	if err := json.Unmarshal(body, &blob); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if blob.AgentSummary == nil {
		t.Fatal("agent_summary: got nil")
	}
	if len(blob.Trajectory) != 2 {
		t.Fatalf("trajectory: got %d sessions want 2", len(blob.Trajectory))
	}

	// Sessions oldest-first; per-session cost headers come from
	// agent_sessions, not the step sum.
	if blob.Trajectory[0].Role != "navigator" || blob.Trajectory[0].Status != "completed" {
		t.Errorf("session 0 header: %s/%s", blob.Trajectory[0].Role, blob.Trajectory[0].Status)
	}
	if blob.Trajectory[1].Role != "vision_judge" || blob.Trajectory[1].Status != "running" {
		t.Errorf("session 1 header: %s/%s", blob.Trajectory[1].Role, blob.Trajectory[1].Status)
	}
	if blob.Trajectory[0].StepsUsed != 2 || blob.Trajectory[0].CostMillicents != 500 {
		t.Errorf("session 0 budget: steps_used=%d cost=%d", blob.Trajectory[0].StepsUsed, blob.Trajectory[0].CostMillicents)
	}
	if blob.Trajectory[0].Truncated || blob.Trajectory[1].Truncated {
		t.Error("truncated flag set on a short trajectory")
	}

	// Step order is idx order within the session.
	if len(blob.Trajectory[0].Steps) != 2 {
		t.Fatalf("navigator steps: got %d want 2", len(blob.Trajectory[0].Steps))
	}
	if blob.Trajectory[0].Steps[0].Idx != 0 || blob.Trajectory[0].Steps[0].Kind != "navigate" {
		t.Errorf("step 0: idx=%d kind=%s", blob.Trajectory[0].Steps[0].Idx, blob.Trajectory[0].Steps[0].Kind)
	}
	if blob.Trajectory[0].Steps[1].Idx != 1 || blob.Trajectory[0].Steps[1].Kind != "assert" {
		t.Errorf("step 1: idx=%d kind=%s", blob.Trajectory[0].Steps[1].Idx, blob.Trajectory[0].Steps[1].Kind)
	}
	if blob.Trajectory[0].Steps[0].Decision.(map[string]any)["url"] != "/login" {
		t.Errorf("step 0 decision passthrough: %v", blob.Trajectory[0].Steps[0].Decision)
	}
	if blob.Trajectory[0].Steps[0].TokensIn != 1200 || blob.Trajectory[0].Steps[0].CostMillicents != 100 {
		t.Errorf("step 0 cost: tokens=%d/%d cost=%d", blob.Trajectory[0].Steps[0].TokensIn, blob.Trajectory[0].Steps[0].TokensOut, blob.Trajectory[0].Steps[0].CostMillicents)
	}

	// Evidence stamps token-gated artifact URLs; traversal-safe paths only.
	prefix := "/e2ai/artifacts/" + runID.String() + "/"
	nSteps := blob.Trajectory[0].Steps
	if len(nSteps[0].Evidence) != 1 {
		t.Fatalf("step 0 evidence: %v", nSteps[0].Evidence)
	}
	ev0 := nSteps[0].Evidence[0]
	if ev0.URL == nil || *ev0.URL != prefix+"agents/n1/screenshots/step-0.png" {
		t.Errorf("step 0 evidence url: %v", ev0)
	}
	ev1 := nSteps[1].Evidence
	if len(ev1) != 2 {
		t.Fatalf("step 1 evidence: %v", ev1)
	}
	if ev1[0].URL != nil {
		t.Errorf("traversal path must keep no url: %v", ev1[0])
	}
	if ev1[0].Path != "../escape.png" {
		t.Errorf("unsafe path kept verbatim as kind/path: %v", ev1[0])
	}
	if ev1[1].URL == nil || *ev1[1].URL != prefix+"agents/n1/a11y/step-1.json" {
		t.Errorf("a11y evidence url: %v", ev1[1])
	}

	// Cost summary: per-session entries in session order, total = 750.
	if blob.AgentSummary.TotalCostMillicents != 750 {
		t.Errorf("summary total: got %d want 750", blob.AgentSummary.TotalCostMillicents)
	}
	if len(blob.AgentSummary.Sessions) != 2 {
		t.Fatalf("summary sessions: %v", blob.AgentSummary.Sessions)
	}
	if blob.AgentSummary.Sessions[0].Role != "navigator" || blob.AgentSummary.Sessions[0].CostMillicents != 500 {
		t.Errorf("summary session 0: %v", blob.AgentSummary.Sessions[0])
	}
	if blob.AgentSummary.Sessions[1].Role != "vision_judge" || blob.AgentSummary.Sessions[1].CostMillicents != 250 {
		t.Errorf("summary session 1: %v", blob.AgentSummary.Sessions[1])
	}
}

// TestReportBlobTrajectoryTruncation drives a session past the wireMaxLimit
// step cap: the wire keeps the most recent wireMaxLimit steps (idx
// contiguous at the tail), flags truncated, and the session header keeps
// the authoritative steps_used.
func TestReportBlobTrajectoryTruncation(t *testing.T) {
	pool := testPool(t)
	migrate(t, pool)
	handler, _ := newSurface(t, pool, "")
	ident := asIdentity(handler)
	appID := seedApp(t, pool, "trajectory-trunc")
	runRow := seedRun(t, pool, appID, 13, time.Now().Add(-time.Minute), runs.StatusExecuting)
	runID := mustParseUUID(t, runRow.String())

	session := seedSession(t, pool, runRow, "flake_analyst", "completed", wireMaxLimit+5, 9000)
	for i := range wireMaxLimit + 5 {
		seedStep(t, pool, session, i, "observe",
			fmt.Sprintf(`{"i":%d}`, i), `{"noop":true}`, `[]`, 1, 1, 10)
	}

	status, body := do(t, ident, "GET", "/api/runs/"+runID.String()+"/report")
	if status != http.StatusOK {
		t.Fatalf("report: %d %s", status, body)
	}
	var blob struct {
		Trajectory []struct {
			StepsUsed int `json:"steps_used"`
			Truncated bool `json:"truncated"`
			Steps     []struct {
				Idx int `json:"idx"`
			} `json:"steps"`
		} `json:"trajectory"`
	}
	if err := json.Unmarshal(body, &blob); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(blob.Trajectory) != 1 {
		t.Fatalf("trajectory: got %d sessions", len(blob.Trajectory))
	}
	sess := blob.Trajectory[0]
	if !sess.Truncated {
		t.Error("truncated: got false on an over-cap trajectory")
	}
	if sess.StepsUsed != wireMaxLimit+5 {
		t.Errorf("steps_used: got %d want %d (authoritative header)", sess.StepsUsed, wireMaxLimit+5)
	}
	if len(sess.Steps) != wireMaxLimit {
		t.Fatalf("steps: got %d want %d", len(sess.Steps), wireMaxLimit)
	}
	// Most recent window: the first kept step is idx 5, the last is the
	// final appended step.
	if sess.Steps[0].Idx != 5 {
		t.Errorf("first kept idx: got %d want 5", sess.Steps[0].Idx)
	}
	if last := sess.Steps[len(sess.Steps)-1].Idx; last != wireMaxLimit+4 {
		t.Errorf("last kept idx: got %d want %d", last, wireMaxLimit+4)
	}
}
