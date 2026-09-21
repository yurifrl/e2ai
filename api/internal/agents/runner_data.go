// Runner-facing data endpoints (Phase 5 wave-1 scope addition): the corpus
// oracle read (CORP-03) and the approved plan-steps read — the DB-mediated
// handoff for the navigator (no env, no files). Both are runner-token gated,
// the same wall as heartbeat/append.
package agents

import (
	"errors"
	"net/http"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/plans"
	"github.com/jackc/pgx/v5"
)

// corpusBehaviorDTO is the agent-facing corpus read contract (05-01):
// {app_id, key, status, content, provenance}. Content is the falsifiable
// behavior text; provenance is the stored corpus provenance, verbatim.
type corpusBehaviorDTO struct {
	AppID      string            `json:"app_id"`
	Key        string            `json:"key"`
	Status     string            `json:"status"`
	Content    map[string]any    `json:"content"`
	Provenance corpus.Provenance `json:"provenance"`
}

// CorpusBehavior serves GET /api/agent/corpus/behaviors?app_id=&key=.
func (s *AgentsHTTP) CorpusBehavior(w http.ResponseWriter, r *http.Request) {
	if !s.runnerGate(w, r) {
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "missing key")
		return
	}
	appID, err := pgUUIDFromString(r.URL.Query().Get("app_id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid app id")
		return
	}

	b, err := corpus.GetBehavior(r.Context(), s.Pool, appID, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "corpus read failed")
		return
	}
	if b == nil {
		writeErr(w, http.StatusNotFound, "behavior not found")
		return
	}
	writeJSON(w, http.StatusOK, corpusBehaviorDTO{
		AppID:      uuidString(b.AppID),
		Key:        b.Key,
		Status:     b.Status,
		Content:    map[string]any{"title": b.Title, "description": b.Description},
		Provenance: b.Provenance,
	})
}

// planStepDTO is one navigator step on the wire (snake_case; the TS runner
// mirrors this shape).
type planStepDTO struct {
	Number    int      `json:"number"`
	Intent    string   `json:"intent"`
	Judge     string   `json:"judge"`
	Uses      []string `json:"uses"`
	OnFailure string   `json:"on_failure"`
}

// sessionPlanDTO is the GET /api/agent-sessions/{id}/plan body: the
// session”s run identity, the approved plan slug, and its steps verbatim.
type sessionPlanDTO struct {
	AppID  string        `json:"app_id"`
	PlanID string        `json:"plan_id"`
	Steps  []planStepDTO `json:"steps"`
}

// SessionPlan serves GET /api/agent-sessions/{id}/plan: resolves
// session → run → approved plan resolution → plan version content, parses
// the markdown steps, and returns them as JSON for the navigator.
func (s *AgentsHTTP) SessionPlan(w http.ResponseWriter, r *http.Request) {
	if !s.runnerGate(w, r) {
		return
	}
	id, ok := parseSessionID(w, r)
	if !ok {
		return
	}

	var appIDStr, planID, content string
	err := s.Pool.QueryRow(r.Context(), `
		select runs.app_id::text, rr.plan_id, pv.content
		from agent_sessions s
		join runs on runs.id = s.run_id
		join run_plan_resolutions rr on rr.run_id = s.run_id
		join plan_versions pv on pv.id = rr.plan_version_id
		where s.id = $1 and rr.mode = ''approved''
	`, id).Scan(&appIDStr, &planID, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "agent session not found")
		return
	}
	if err != nil {
		// A missing runs row joins to nothing too: re-check the session
		// alone so an unknown session is a 404 and a known session whose
		// run lacks a resolution is a 409.
		var exists bool
		if e := s.Pool.QueryRow(r.Context(),
			`select exists (select 1 from agent_sessions where id = $1)`, id).Scan(&exists); e != nil || !exists {
			writeErr(w, http.StatusNotFound, "agent session not found")
			return
		}
		writeErr(w, http.StatusConflict, "session''s run has no approved plan resolution")
		return
	}

	plan, err := plans.Parse([]byte(content))
	if err != nil {
		writeErr(w, http.StatusConflict, "approved plan content no longer parses")
		return
	}
	steps := make([]planStepDTO, 0, len(plan.Steps))
	for _, st := range plan.Steps {
		steps = append(steps, planStepDTO{
			Number: st.Number, Intent: st.Intent, Judge: st.Judge,
			Uses: st.Uses, OnFailure: st.OnFailure,
		})
	}
	writeJSON(w, http.StatusOK, sessionPlanDTO{AppID: appIDStr, PlanID: planID, Steps: steps})
}
