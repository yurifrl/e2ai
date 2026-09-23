// Cockpit HTTP surface (COCK-04 API half): the arm-state read and the two
// attributed pause mutations. Wire shape is locked to web/src/lib/types.ts —
// CockpitState — and the endpoints api.ts calls (getCockpitState, pauseArm,
// unpauseArm) exist exactly as the web module expects.
//
// Auth posture mirrors the approvals surface (approvals.PlansHTTP): BFF
// identity headers gate via httpx — RunViewPermission for the read,
// RunEditPermission for mutations — and every mutation names a real
// operator: the httpx dev-fallback identity ("you", or the configured
// DevFallbackUser) is refused with 400 so a proxied-in dev shell or an
// un-proxied curl can never be recorded as the actor.
package cockpit

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
)

// reasonMaxLen is the server-side mirror of the UI-SPEC's 200-char input
// cap; overlong reasons are rejected, never silently truncated.
const reasonMaxLen = 200

// Mount registers the cockpit routes.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/cockpit/state", s.State)
	mux.HandleFunc("POST /api/cockpit/pause", s.Pause)
	mux.HandleFunc("POST /api/cockpit/unpause", s.Unpause)
}

// State serves the arm-state read model (view permission).
func (s *Service) State(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, false) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	state, err := Read(r.Context(), s.Pool)
	if err != nil {
		s.Logger.Error("cockpit state read failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "read failed")
		return
	}
	writeWire(w, http.StatusOK, stateDTO(state))
}

// Pause flips the arm state to paused with an operator reason (edit
// permission).
func (s *Service) Pause(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, true)
}

// Unpause clears the arm pause (edit permission).
func (s *Service) Unpause(w http.ResponseWriter, r *http.Request) {
	s.mutate(w, r, false)
}

// mutate is the shared pause/unpause body: gate, refuse placeholder
// identities, validate the optional reason, flip + audit in one tx.
func (s *Service) mutate(w http.ResponseWriter, r *http.Request, paused bool) {
	if !s.gate(w, r, true) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	user := resolvedUser(r)
	if user == "" || user == "you" || (s.FallbackUser != "" && user == s.FallbackUser) {
		writeErr(w, http.StatusBadRequest, "cockpit mutations require a real identity")
		return
	}

	// Reason is optional; an empty or absent body is fine (approvals notes
	// pattern).
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if len(body.Reason) > reasonMaxLen {
		writeErr(w, http.StatusBadRequest, "reason too long")
		return
	}

	if err := SetPaused(r.Context(), s.Pool, user, paused, body.Reason); err != nil {
		if errors.Is(err, ErrNoActor) {
			writeErr(w, http.StatusBadRequest, "cockpit mutations require a real identity")
			return
		}
		s.Logger.Error("cockpit mutation failed", "error", err, "user", user)
		writeErr(w, http.StatusInternalServerError, "mutation failed")
		return
	}
	// 200 + the fresh arm state: the pause card re-renders from the ack
	// (server-ack wait, no optimistic UI) — mirrors the quarantine surface's
	// mutations-return-a-body convention, never a bare 204.
	st, err := Read(r.Context(), s.Pool)
	if err != nil {
		s.Logger.Error("cockpit state read failed", "error", err)
		writeErr(w, http.StatusInternalServerError, "state read failed")
		return
	}
	writeWire(w, http.StatusOK, stateDTO(st))
}

// gate mirrors the approvals surface: view for reads, edit for mutations. A
// resolvable identity without the required permission is 403; a
// middleware-absent identity is 401.
func (s *Service) gate(w http.ResponseWriter, r *http.Request, edit bool) bool {
	need := httpx.RunViewPermission
	if edit {
		need = httpx.RunEditPermission
	}
	identity, ok := httpx.IdentityFromContext(r.Context())
	if ok && identity.HasPermission(need) {
		return true
	}
	if ok {
		writeErr(w, http.StatusForbidden, "forbidden")
	} else {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
	}
	return false
}

// resolvedUser picks the attribution identity: the middleware-resolved
// identity when present, else the bare X-User header (UserOf). Placeholder
// identities are refused by the caller before any write.
func resolvedUser(r *http.Request) string {
	if identity, ok := httpx.IdentityFromContext(r.Context()); ok && identity.User != "" {
		return identity.User
	}
	return httpx.UserOf(r)
}

// --- wire DTO (locked verbatim to web/src/lib/types.ts CockpitState) ---

type cockpitStateDTO struct {
	Paused       bool    `json:"paused"`
	PausedReason *string `json:"paused_reason"`
	PausedBy     *string `json:"paused_by"`
	PausedAt     *string `json:"paused_at"`
	UpdatedAt    string  `json:"updated_at"`
}

func stateDTO(s State) cockpitStateDTO {
	dto := cockpitStateDTO{
		Paused:    s.Paused,
		UpdatedAt: s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if s.PausedReason != "" {
		dto.PausedReason = &s.PausedReason
	}
	if s.PausedBy != "" {
		dto.PausedBy = &s.PausedBy
	}
	if s.PausedAt != nil {
		t := s.PausedAt.UTC().Format(time.RFC3339Nano)
		dto.PausedAt = &t
	}
	return dto
}

func writeWire(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeWire(w, status, map[string]string{"error": msg})
}
