// Manual-trigger HTTP boundary (TRIG-01/02, COCK-04 API half): POST
// /api/cockpit/triggers. Refusal matrix mirrors the domain's typed errors:
// pause/budget/active-run/unapproved-plan refusals are 409 with the specific
// reason; zero-step pins and zero ceilings are 422 (validation-style);
// request-shape violations are 422; identity-less or placeholder callers are
// refused before the transaction. Success is 202 with the created run.
package cockpit

import (
	"errors"
	"net/http"
)

// Mount entry — registered beside the pause mutations.
// POST /api/cockpit/triggers

// triggerDTO is the 202 body: the run the operator just spawned.
type triggerDTO struct {
	RunID         string `json:"run_id"`
	WorkflowRunID int64  `json:"workflow_run_id"`
}

// TriggerHTTP handles POST /api/cockpit/triggers: gate, refuse placeholder
// identities, then run the one-transaction Trigger and translate its typed
// refusals per the plan's status matrix.
func (s *Service) TriggerHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, true) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	if s.Queue == nil {
		writeErr(w, http.StatusServiceUnavailable, "runner queue unavailable")
		return
	}
	if s.Heads == nil {
		writeErr(w, http.StatusServiceUnavailable, "head resolver not wired")
		return
	}
	user := resolvedUser(r)
	if user == "" || user == "you" || (s.FallbackUser != "" && user == s.FallbackUser) {
		writeErr(w, http.StatusBadRequest, "cockpit mutations require a real identity")
		return
	}

	var req TriggerRequest
	if err := decodeCapped(r, &req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "invalid trigger request body")
		return
	}
	if len(req.Reason) > reasonMaxLen {
		writeErr(w, http.StatusBadRequest, "reason too long")
		return
	}
	// Actor is context-only (P4/AUDT-01) — never the request body.
	req.actor = user

	runID, workflowRunID, err := s.Trigger(r.Context(), req)
	if err != nil {
		writeTriggerErr(w, err)
		return
	}
	writeWire(w, http.StatusAccepted, triggerDTO{
		RunID: runID.String(), WorkflowRunID: workflowRunID,
	})
}

// writeTriggerErr maps the trigger's typed refusals to the plan's status
// matrix. Errors.Is is required — several refusals are wrapped with detail.
func writeTriggerErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errInvalidRequest):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrZeroCeiling), errors.Is(err, ErrZeroStepPlan):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, ErrUnknownApp):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrHeadResolve):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrPaused), errors.Is(err, ErrBudgetBreach),
		errors.Is(err, ErrActiveManualRun), errors.Is(err, ErrUnapprovedPlan),
		errors.Is(err, ErrZeroApprovedPlans):
		// The wrapped cause text carries the specific reason (pause reason,
		// remaining budget, superseding approved version).
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "trigger failed")
	}
}

// compile-time check: the boundary uses the same identity helpers as pause.
