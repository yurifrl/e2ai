// Repair-proposal intake (FLAK-02, Phase 5 T7): the navigator's appender for
// selector-repair proposals. The runner-token wall is the same as
// heartbeat/steps; the session scopes the proposal to its run's app. The
// endpoint only ever APPENDS — a proposal row on the learning record the
// failure produced — and never mutates the corpus oracle or the app under
// test. Adoption is a human decision on the plans repair-decision endpoints.
package agents

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/jackc/pgx/v5"
)

// repairProposalRequest is the navigator's proposal body (agentrunner
// repair.ts is the producer). learning_id optionally pins the corpus learning
// the failure already generated; when empty the API appends the learning.
type repairProposalRequest struct {
	BehaviorKey      string  `json:"behavior_key"`
	BrokenSelector   string  `json:"broken_selector"`
	ProposedSelector string  `json:"proposed_selector"`
	Rationale        string  `json:"rationale"`
	Confidence       float64 `json:"confidence"`
	EvidenceRef      string  `json:"evidence_ref"`
	LearningID       string  `json:"learning_id"`
}

// ProposeRepair serves POST /api/agent-sessions/{id}/repair-proposals.
func (s *AgentsHTTP) ProposeRepair(w http.ResponseWriter, r *http.Request) {
	if !s.runnerGate(w, r) {
		return
	}
	id, ok := parseSessionID(w, r)
	if !ok {
		return
	}
	var req repairProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid repair proposal body")
		return
	}

	// Session → app/run scoping: a proposal can only ever attach to the app
	// the session is running against, and its auto-generated learning pins
	// the run that produced the failure.
	var appIDStr, runIDStr string
	err := s.Pool.QueryRow(r.Context(), `
		select runs.app_id::text, s.run_id::text
		from agent_sessions s
		join runs on runs.id = s.run_id
		where s.id = $1
	`, id).Scan(&appIDStr, &runIDStr)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "agent session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve session app failed")
		return
	}
	appID, err := pgUUIDFromString(appIDStr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "session run has malformed app id")
		return
	}
	runID, err := pgUUIDFromString(runIDStr)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "session has malformed run id")
		return
	}

	// The proposal attaches to a corpus learning record (the failure's
	// observation). When the navigator did not append the learning itself,
	// the intake creates it from the proposal's structured fields so the
	// review queue stays readable without side channels.
	learningID := req.LearningID
	if learningID == "" {
		l, err := corpus.AppendLearning(r.Context(), s.Pool, appID, runID,
			repairObservation(req))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "append learning for repair failed")
			return
		}
		learningID = uuidString(l.ID)
	}
	parsedLearning, err := pgUUIDFromString(learningID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid learning_id")
		return
	}
	p, err := corpus.AppendRepairProposal(r.Context(), s.Pool, corpus.RepairProposal{
		LearningID:       parsedLearning,
		BehaviorKey:      req.BehaviorKey,
		BrokenSelector:   req.BrokenSelector,
		ProposedSelector: req.ProposedSelector,
		Rationale:        req.Rationale,
		Confidence:       req.Confidence,
		EvidenceRef:      req.EvidenceRef,
		ProposedBy:       "navigator",
	})
	if err != nil {
		mapRepairError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, repairProposalWire(p))
}

// repairObservation condenses the proposal into the learning observation the
// review queue reads: what broke, what the navigator suggests, why.
func repairObservation(req repairProposalRequest) string {
	return "Selector repair proposed for behavior " + req.BehaviorKey +
		": locator " + req.BrokenSelector + " failed to resolve. Proposed: " +
		req.ProposedSelector + " (confidence " +
		strconv.FormatFloat(req.Confidence, 'f', 2, 64) + "). " + req.Rationale
}

// repairProposalWire is the snake_case wire shape (agentrunner repair.ts and
// the admin queue both consume it; wire contract is additive-only).
type repairProposalWireDTO struct {
	ID               string  `json:"id"`
	LearningID       string  `json:"learning_id"`
	AppID            string  `json:"app_id"`
	BehaviorKey      string  `json:"behavior_key"`
	BrokenSelector   string  `json:"broken_selector"`
	ProposedSelector string  `json:"proposed_selector"`
	Rationale        string  `json:"rationale"`
	Confidence       float64 `json:"confidence"`
	EvidenceRef      string  `json:"evidence_ref"`
	Status           string  `json:"status"`
	ProposedBy       string  `json:"proposed_by"`
	DecidedBy        *string `json:"decided_by"`
	CreatedAt        string  `json:"created_at"`
}

func repairProposalWire(p *corpus.RepairProposal) repairProposalWireDTO {
	var decidedBy *string
	if p.DecidedBy.Valid {
		s := p.DecidedBy.String
		decidedBy = &s
	}
	return repairProposalWireDTO{
		ID:               uuidString(p.ID),
		LearningID:       uuidString(p.LearningID),
		AppID:            uuidString(p.AppID),
		BehaviorKey:      p.BehaviorKey,
		BrokenSelector:   p.BrokenSelector,
		ProposedSelector: p.ProposedSelector,
		Rationale:        p.Rationale,
		Confidence:       p.Confidence,
		EvidenceRef:      p.EvidenceRef,
		Status:           p.Status,
		ProposedBy:       p.ProposedBy,
		DecidedBy:        decidedBy,
		CreatedAt:        p.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// mapRepairError maps the corpus repair store's sentinels to REST statuses.
func mapRepairError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, corpus.ErrNotFound):
		writeErr(w, http.StatusNotFound, "learning not found")
	case errors.Is(err, corpus.ErrBelowConfidenceFloor):
		writeErr(w, http.StatusUnprocessableEntity, "confidence below refuse-to-heal floor; escalate with an observation instead")
	case errors.Is(err, corpus.ErrIncompleteProposal):
		writeErr(w, http.StatusUnprocessableEntity, "repair proposal missing required fields")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "append repair proposal failed")
	}
}
