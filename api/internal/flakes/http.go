// Flake governance HTTP surface (Phase 5 T1): the flake-rate read, the
// quarantine queue, and the guarded quarantine mutations (activate/lift),
// mirroring the agents surface pattern (mount, identity wall, JSON errors).
// Quarantining never touches the app under test — the report stage reads
// active rows as the skip signal (report-only, FLAK-01/02).
package flakes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QuarantineHTTP serves the flake-governance surface.
type QuarantineHTTP struct {
	Pool *pgxpool.Pool
}

// NewQuarantineHTTP builds the surface for main.go's mux.
func NewQuarantineHTTP(pool *pgxpool.Pool) *QuarantineHTTP {
	return &QuarantineHTTP{Pool: pool}
}

// Mount registers the routes: reads are GET + view permission, mutations
// are POST + edit permission.
func (s *QuarantineHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/flakes", s.FlakeRatesHandler)
	mux.HandleFunc("GET /api/quarantine", s.ListQuarantine)
	mux.HandleFunc("POST /api/quarantine", s.ActivateQuarantine)
	mux.HandleFunc("POST /api/quarantine/{id}/lift", s.LiftQuarantine)
}

// FlakeRatesHandler serves GET /api/flakes?app_id=&window=.
func (s *QuarantineHTTP) FlakeRatesHandler(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	appID, ok := appIDFromQuery(w, r)
	if !ok {
		return
	}
	rates, err := FlakeRates(r.Context(), s.Pool, appID, queryInt(r, "window", DefaultWindow))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "flake rates failed")
		return
	}
	writeJSON(w, http.StatusOK, rates)
}

// ListQuarantine serves GET /api/quarantine?app_id=&status=.
func (s *QuarantineHTTP) ListQuarantine(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	appID, ok := appIDFromQuery(w, r)
	if !ok {
		return
	}
	entries, err := List(r.Context(), s.Pool, appID, r.URL.Query().Get("status"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "quarantine list failed")
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ActivateQuarantine serves POST /api/quarantine (edit permission): the
// human-gated quarantine activation. An active duplicate is a typed 409.
func (s *QuarantineHTTP) ActivateQuarantine(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, true) {
		return
	}
	var req struct {
		AppID            string `json:"app_id"`
		File             string `json:"file"`
		Title            string `json:"title"`
		Reason           string `json:"reason"`
		EvidenceArtifact string `json:"evidence_artifact"`
		ExpiresAt        string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed body")
		return
	}
	var appID pgtype.UUID
	if err := appID.Scan(req.AppID); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid app_id")
		return
	}
	if req.Title == "" {
		writeErr(w, http.StatusBadRequest, "title is required")
		return
	}
	var expiresAtTime *time.Time
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid expires_at (RFC3339)")
			return
		}
		expiresAtTime = &t
	}
	entry, err := Activate(r.Context(), s.Pool, appID, req.File, req.Title,
		req.Reason, req.EvidenceArtifact, ByHuman, expiresAtTime)
	switch {
	case errors.Is(err, ErrAlreadyQuarantined):
		writeErr(w, http.StatusConflict, "test is already quarantined; lift it first")
		return
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "quarantine activation failed")
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

// LiftQuarantine serves POST /api/quarantine/{id}/lift (edit permission).
func (s *QuarantineHTTP) LiftQuarantine(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, true) {
		return
	}
	id, err := pgUUIDFromString(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid quarantine id")
		return
	}
	by := httpx.UserOf(r)
	entry, err := Lift(r.Context(), s.Pool, id, by)
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "quarantine entry not found")
		return
	case errors.Is(err, ErrNotQuarantined):
		writeErr(w, http.StatusConflict, "test is not quarantined")
		return
	case errors.Is(err, ErrInvalidInput):
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "quarantine lift failed")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// gate enforces the identity wall: authenticated + permission, reads
// view, mutations edit (tiering per the naive v0 access model).
func gate(w http.ResponseWriter, r *http.Request, edit bool) bool {
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

func appIDFromQuery(w http.ResponseWriter, r *http.Request) (pgtype.UUID, bool) {
	var appID pgtype.UUID
	if err := appID.Scan(r.URL.Query().Get("app_id")); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid app_id")
		return appID, false
	}
	return appID, true
}

func pgUUIDFromString(raw string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse uuid %q: %w", raw, err)
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string, fallback int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return fallback
	}
	return v
}

