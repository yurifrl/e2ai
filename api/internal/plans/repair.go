// Repair-proposal review surface (FLAK-02, Phase 5 T7): the admin half of the
// selector-repair flow. Proposals arrive through the agents intake (runner
// token); here the human review queue is listed per app and the one-shot
// decision is applied with attribution. Approval is the ONLY path that
// mutates the corpus oracle — it reuses the same promote transaction the
// manual flow uses — and rejection is a terminal status flip. Wire shapes
// mirror the agents intake DTO (additive-only contract with web/src/lib).
package plans

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
)

// repairDecisionRequest is the reviewer's decide body.
type repairDecisionRequest struct {
	Decision string `json:"decision"`
}

// repairDTO is the snake_case review-queue row (same shape the agents intake
// returns; one wire shape for both surfaces).
type repairDTO struct {
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
	DecidedAt        *string `json:"decided_at"`
	CreatedAt        string  `json:"created_at"`
}

func repairWire(p *corpus.RepairProposal) repairDTO {
	var decidedBy, decidedAt *string
	if p.DecidedBy.Valid {
		s := p.DecidedBy.String
		decidedBy = &s
	}
	if p.DecidedAt.Valid {
		s := p.DecidedAt.Time.UTC().Format(time.RFC3339)
		decidedAt = &s
	}
	return repairDTO{
		ID:               p.ID.String(),
		LearningID:       p.LearningID.String(),
		AppID:            p.AppID.String(),
		BehaviorKey:      p.BehaviorKey,
		BrokenSelector:   p.BrokenSelector,
		ProposedSelector: p.ProposedSelector,
		Rationale:        p.Rationale,
		Confidence:       p.Confidence,
		EvidenceRef:      p.EvidenceRef,
		Status:           p.Status,
		ProposedBy:       p.ProposedBy,
		DecidedBy:        decidedBy,
		DecidedAt:        decidedAt,
		CreatedAt:        p.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// ListRepairProposals serves GET /api/corpus/repairs?app=: the human review
// queue for one app, newest first.
func (s *PlansHTTP) ListRepairProposals(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	ref := r.URL.Query().Get("app")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "app is required")
		return
	}
	appID, err := ResolveApp(r.Context(), s.Pool, ref)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusBadRequest, "bad app ref")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve app failed")
		return
	}
	proposals, err := corpus.ListRepairProposals(r.Context(), s.Pool, appID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list repair proposals failed")
		return
	}
	out := make([]repairDTO, 0, len(proposals))
	for _, p := range proposals {
		out = append(out, repairWire(&p))
	}
	writeWire(w, http.StatusOK, out)
}

// DecideRepairProposal serves POST /api/corpus/repairs/{repairID}/decide with
// body {"decision": "approve"|"reject"}: the attributed, one-shot human
// decision. Approval promotes the attached learning into a corpus behavior.
func (s *PlansHTTP) DecideRepairProposal(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, true) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	rid, err := parseUUID(r.PathValue("repairID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad repair id")
		return
	}
	var req repairDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid decide body")
		return
	}
	p, err := corpus.DecideRepairProposal(r.Context(), s.Pool, rid, req.Decision, httpx.UserOf(r))
	switch {
	case errors.Is(err, corpus.ErrNotFound):
		writeErr(w, http.StatusNotFound, "repair proposal not found")
		return
	case errors.Is(err, corpus.ErrNotAProposal):
		writeErr(w, http.StatusConflict, "repair proposal already decided")
		return
	case errors.Is(err, corpus.ErrInvalidDecision):
		writeErr(w, http.StatusBadRequest, "decision must be approve or reject")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "repair decision failed")
		return
	}
	writeWire(w, http.StatusOK, repairWire(p))
}

// Mount registers the repair review routes (called from the existing
// Mount — kept additive).
func (s *PlansHTTP) mountRepair(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/corpus/repairs", s.ListRepairProposals)
	mux.HandleFunc("POST /api/corpus/repairs/{repairID}/decide", s.DecideRepairProposal)
}
