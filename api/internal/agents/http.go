// Agents HTTP surface (Phase 3 T2): the runner-facing session lifecycle
// endpoints (heartbeat, steps, complete, fail — service token), the admin
// kill switch and session enqueue (RunEditPermission), and the admin queue
// data listing. Identity follows the established surface pattern: BFF
// headers via httpx middleware; runner endpoints additionally accept
// "Authorization: Bearer $E2AI_RUNNER_SERVICE_TOKEN".
package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/NSXBet/e2ai/api/internal/plans"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AgentsHTTP is the agent-session HTTP surface.
type AgentsHTTP struct {
	Pool *pgxpool.Pool
	// RunnerToken is E2AI_RUNNER_SERVICE_TOKEN; empty disables the
	// service-token path (runner endpoints fall back to BFF identity only).
	RunnerToken string
}

// NewAgentsHTTP builds the surface for main.go's mux.
func NewAgentsHTTP(pool *pgxpool.Pool, runnerToken string) *AgentsHTTP {
	return &AgentsHTTP{Pool: pool, RunnerToken: runnerToken}
}

// Mount registers the routes. Method prefixes keep reads GET-only and the
// lifecycle POST-only, mirroring the approvals/plans surfaces.
func (s *AgentsHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/runs/{runId}/agent-sessions", s.Enqueue)
	mux.HandleFunc("POST /api/agent-sessions/{id}/heartbeat", s.sessionAction(func(r *http.Request, id pgtype.UUID) (any, error) {
		return okDTO{OK: true}, Heartbeat(r.Context(), s.Pool, id)
	}))
	mux.HandleFunc("POST /api/agent-sessions/{id}/steps", s.AppendStep)
	mux.HandleFunc("POST /api/agent-sessions/{id}/complete", s.sessionAction(func(r *http.Request, id pgtype.UUID) (any, error) {
		return s.finish(r, id, StatusCompleted)
	}))
	mux.HandleFunc("POST /api/agent-sessions/{id}/fail", s.sessionAction(func(r *http.Request, id pgtype.UUID) (any, error) {
		return s.finish(r, id, StatusFailed)
	}))
	mux.HandleFunc("POST /api/agent-sessions/{id}/kill", s.Kill)
	mux.HandleFunc("GET /api/agent-sessions", s.List)
}

type okDTO struct {
	OK     bool   `json:"ok"`
	Status Status `json:"status,omitempty"`
}

// --- identity ---

// requirePermission writes the 401/403 wall and reports whether the caller
// holds the permission (false = response already written).
func requirePermission(w http.ResponseWriter, r *http.Request, p httpx.Permission) bool {
	identity, ok := httpx.IdentityFromContext(r.Context())
	switch {
	case !ok:
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	case !identity.HasPermission(p):
		writeErr(w, http.StatusForbidden, "forbidden")
	default:
		return true
	}
	return false
}

// serviceIdentity reports whether the request carries the runner service
// token. The static token (not a BFF JWT) cannot pass through the identity
// middleware, so it is checked directly against the Authorization header.
func (s *AgentsHTTP) serviceIdentity(r *http.Request) bool {
	if s.RunnerToken == "" {
		return false
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return strings.HasPrefix(auth, prefix) && auth[len(prefix):] == s.RunnerToken
}

// runnerGate admits either the runner service token or an admin (edit)
// identity; anything else gets the 401/403 wall.
func (s *AgentsHTTP) runnerGate(w http.ResponseWriter, r *http.Request) bool {
	if s.serviceIdentity(r) {
		return true
	}
	return requirePermission(w, r, httpx.RunEditPermission)
}

// --- error mapping ---

// mapDomainError writes the surface error for the T1 domain sentinels:
// unknown id → 404, terminal/transition rejections and budget breaches →
// 409 (the kill-switch contract), everything else → 500.
func mapDomainError(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "agent session not found")
	case errors.Is(err, ErrInvalidTransition): // includes TransitionError (lost race or forbidden)
		writeErr(w, http.StatusConflict, "transition not allowed for this session state")
	case errors.Is(err, ErrBudgetExhausted), errors.Is(err, ErrTerminal):
		writeErr(w, http.StatusConflict, "session is terminal or no longer active")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, fallback)
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

// --- enqueue: POST /api/runs/{runId}/agent-sessions ---

// Enqueue creates the session set for the run's approved plan: the plan is
// the step-batch — one navigator to execute it and one independent
// vision_judge to verify it (RESEARCH §Roster roles, run kind). Both
// sessions carry the plan's step count as their step ceiling so the budget
// engine bounds them to the plan. Re-enqueueing a run that already has
// sessions is a 409.
func (s *AgentsHTTP) Enqueue(w http.ResponseWriter, r *http.Request) {
	if !requirePermission(w, r, httpx.RunEditPermission) {
		return
	}
	runID, err := pgUUIDFromString(r.PathValue("runId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid run id")
		return
	}
	ctx := r.Context()
	var runStatus string
	err = s.Pool.QueryRow(ctx, `select status from runs where id = $1`, runID).Scan(&runStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read run failed")
		return
	}
	if runStatus != "executing" {
		writeErr(w, http.StatusConflict, "run is not in executing state")
		return
	}

	// Approved-plan gate: the run's resolution must select an approved
	// plan_version (mode='approved' + version status='approved').
	var versionID pgtype.UUID
	var content []byte
	err = s.Pool.QueryRow(ctx, `
		select pv.id, pv.content
		from run_plan_resolutions r
		join plan_versions pv on pv.id = r.plan_version_id
		where r.run_id = $1 and r.mode = 'approved' and pv.status = 'approved'
	`, runID).Scan(&versionID, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusConflict, "run has no approved plan resolution")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read plan resolution failed")
		return
	}

	var existing int
	if err := s.Pool.QueryRow(ctx,
		`select count(*) from agent_sessions where run_id = $1`, runID).Scan(&existing); err != nil {
		writeErr(w, http.StatusInternalServerError, "check existing sessions failed")
		return
	}
	if existing > 0 {
		writeErr(w, http.StatusConflict, "agent sessions already enqueued for this run")
		return
	}

	plan, err := plans.Parse(content)
	if err != nil {
		writeErr(w, http.StatusConflict, "approved plan content no longer parses")
		return
	}
	ceiling := len(plan.Steps) // 0 = unlimited per the budget engine

	inserted := []map[string]string{}
	for _, role := range []Role{RoleNavigator, RoleVisionJudge} {
		var id pgtype.UUID
		if err := s.Pool.QueryRow(ctx, `
			insert into agent_sessions (run_id, role, step_ceiling)
			values ($1, $2, $3) returning id
		`, runID, role, ceiling).Scan(&id); err != nil {
			writeErr(w, http.StatusInternalServerError, "insert agent session failed")
			return
		}
		inserted = append(inserted, map[string]string{"id": uuidString(id), "role": string(role)})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"enqueued": true, "sessions": inserted})
}

// --- runner lifecycle endpoints ---

// stepRequest is the runner's step-append body (T3 loop.ts is the producer).
type stepRequest struct {
	Kind           string          `json:"kind"`
	Decision       json.RawMessage `json:"decision"`
	Action         json.RawMessage `json:"action"`
	Evidence       json.RawMessage `json:"evidence"`
	TokensIn       int             `json:"tokens_in"`
	TokensOut      int             `json:"tokens_out"`
	CostMillicents int64           `json:"cost_millicents"`
}

// AppendStep serves POST /api/agent-sessions/{id}/steps.
func (s *AgentsHTTP) AppendStep(w http.ResponseWriter, r *http.Request) {
	if !s.runnerGate(w, r) {
		return
	}
	id, ok := parseSessionID(w, r)
	if !ok {
		return
	}
	var req stepRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid step body")
		return
	}
	if req.Kind == "" {
		writeErr(w, http.StatusBadRequest, "step kind is required")
		return
	}
	if err := AppendStep(r.Context(), s.Pool, id, Step{
		Kind:           req.Kind,
		Decision:       req.Decision,
		Action:         req.Action,
		Evidence:       req.Evidence,
		TokensIn:       req.TokensIn,
		TokensOut:      req.TokensOut,
		CostMillicents: req.CostMillicents,
	}); err != nil {
		mapDomainError(w, err, "append step failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"appended": true})
}

// finish drives complete/fail through the guarded transition table.
func (s *AgentsHTTP) finish(r *http.Request, id pgtype.UUID, to Status) (any, error) {
	if err := Transition(r.Context(), s.Pool, id, to); err != nil {
		return nil, err
	}
	return okDTO{OK: true, Status: to}, nil
}

// sessionAction adapts a session-scoped runner action to the shared
// gate + id parsing + error mapping envelope.
func (s *AgentsHTTP) sessionAction(fn func(r *http.Request, id pgtype.UUID) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.runnerGate(w, r) {
			return
		}
		id, ok := parseSessionID(w, r)
		if !ok {
			return
		}
		body, err := fn(r, id)
		if err != nil {
			mapDomainError(w, err, "session action failed")
			return
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// HeartbeatHandler serves POST /api/agent-sessions/{id}/heartbeat.
func (s *AgentsHTTP) HeartbeatHandler(w http.ResponseWriter, r *http.Request) {
	if !s.runnerGate(w, r) {
		return
	}
	id, ok := parseSessionID(w, r)
	if !ok {
		return
	}
	if err := Heartbeat(r.Context(), s.Pool, id); err != nil {
		mapDomainError(w, err, "heartbeat failed")
		return
	}
	writeJSON(w, http.StatusOK, okDTO{OK: true})
}

// Kill serves POST /api/agent-sessions/{id}/kill: the kill switch
// (BUDG-02). RunEditPermission forces status=killed; the runner's next
// heartbeat or step append gets a 409 and exits. No deploy involved.
func (s *AgentsHTTP) Kill(w http.ResponseWriter, r *http.Request) {
	if !requirePermission(w, r, httpx.RunEditPermission) {
		return
	}
	id, ok := parseSessionID(w, r)
	if !ok {
		return
	}
	if err := Transition(r.Context(), s.Pool, id, StatusKilled); err != nil {
		mapDomainError(w, err, "kill failed")
		return
	}
	writeJSON(w, http.StatusOK, okDTO{OK: true, Status: StatusKilled})
}

// --- admin queue data: GET /api/agent-sessions?run_id= ---

// sessionDTO is one admin-queue row. Stuck mirrors the orphan sweep
// threshold: a claimed/running session whose heartbeat lapsed past 90s.
type sessionDTO struct {
	ID     string `json:"id"`
	RunID  string `json:"run_id"`
	Role   string `json:"role"`
	Status string `json:"status"`
	Stuck  bool   `json:"stuck"`
}

// List serves GET /api/agent-sessions?run_id= for the admin queue page.
func (s *AgentsHTTP) List(w http.ResponseWriter, r *http.Request) {
	if !requirePermission(w, r, httpx.RunViewPermission) {
		return
	}
	runID, err := pgUUIDFromString(r.URL.Query().Get("run_id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "run_id query parameter is required")
		return
	}
	rows, err := s.Pool.Query(r.Context(), `
		select id, run_id, role, status,
		       status in ('claimed', 'running')
		         and coalesce(heartbeat_at, created_at) < now() - interval '90 seconds' as stuck
		from agent_sessions where run_id = $1
		order by created_at`, runID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list agent sessions failed")
		return
	}
	defer rows.Close()
	out := []sessionDTO{}
	for rows.Next() {
		var dto sessionDTO
		var id, rid pgtype.UUID
		var role string
		var status Status
		if err := rows.Scan(&id, &rid, &role, &status, &dto.Stuck); err != nil {
			writeErr(w, http.StatusInternalServerError, "scan agent session failed")
			return
		}
		dto.ID, dto.RunID, dto.Role, dto.Status = uuidString(id), uuidString(rid), role, string(status)
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// --- helpers ---

func parseSessionID(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	id, err := pgUUIDFromString(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return pgtype.UUID{}, false
	}
	return id, true
}

func pgUUIDFromString(raw string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse uuid %q: %w", raw, err)
	}
	return id, nil
}

func uuidString(id pgtype.UUID) string {
	b, _ := id.Value()
	return b.(string)
}
