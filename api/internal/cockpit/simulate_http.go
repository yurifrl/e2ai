// Webhook-simulate HTTP boundary (ACTN-01, COCK-04 API half): POST
// /api/cockpit/simulate. Gate order mirrors the trigger — identity →
// pause → budget → secret → render → ingest. Refusal matrix: pause/budget
// 409 with the specific reason, zero ceiling and request-shape 422, unknown
// app 400, unavailable secret 503. Success is 202 with the ingest outcome —
// never the secret or signature material.
package cockpit

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
)

// simulateDTO is the 202 body: the ingest outcome plus the run link when a
// run row was created.
type simulateDTO struct {
	Outcome       string `json:"outcome"`
	RunID         string `json:"run_id,omitempty"`
	WorkflowRunID int64  `json:"workflow_run_id"`
}

// SimulateHTTP handles POST /api/cockpit/simulate (edit permission — it
// creates a run and spends budget).
func (s *Service) SimulateHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, true) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "ingest store unavailable")
		return
	}
	if s.Queue == nil {
		writeErr(w, http.StatusServiceUnavailable, "runner queue unavailable")
		return
	}
	user := resolvedUser(r)
	if user == "" || user == "you" || (s.FallbackUser != "" && user == s.FallbackUser) {
		writeErr(w, http.StatusBadRequest, "cockpit mutations require a real identity")
		return
	}
	var req SimulateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid simulate request body")
		return
	}
	if len(req.Reason) > reasonMaxLen {
		writeErr(w, http.StatusBadRequest, "reason too long")
		return
	}
	req.actor = user
	result, runID, wfID, err := s.Simulate(r.Context(), req)
	if err != nil {
		writeSimulateErr(w, err)
		return
	}
	runIDOut := ""
	if runID != uuid.Nil {
		runIDOut = runID.String()
	}
	writeWire(w, http.StatusAccepted, simulateDTO{
		Outcome:       result.String(),
		RunID:         runIDOut,
		WorkflowRunID: wfID,
	})
}

// writeSimulateErr maps the simulate's typed refusals to the same status
// matrix the trigger uses. Errors.Is is required — several refusals are
// wrapped with detail.
func writeSimulateErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidRequest), errors.Is(err, ErrZeroCeiling):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrUnknownApp):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrPaused), errors.Is(err, ErrBudgetBreach):
		// The wrapped cause text carries the specific reason (pause reason,
		// remaining budget).
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrSecretUnavailable):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "simulate failed")
	}
}
