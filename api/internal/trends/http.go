// Trends HTTP surface (Phase 5 T1): GET-only aggregations under
// /api/trends, view-permission gated, mirroring the plans/approvals
// surface pattern (mount, identity wall, JSON errors).
package trends

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

var errNoRows = pgx.ErrNoRows

// TrendsHTTP serves the trends surface.
type TrendsHTTP struct {
	Pool *pgxpool.Pool
}

// NewTrendsHTTP builds the surface for main.go's mux.
func NewTrendsHTTP(pool *pgxpool.Pool) *TrendsHTTP {
	return &TrendsHTTP{Pool: pool}
}

// Mount registers the routes (GET-only; view permission).
func (s *TrendsHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/trends/flips", s.FlipRateHandler)
	mux.HandleFunc("GET /api/trends/cost/{runId}", s.CostHandler)
	mux.HandleFunc("GET /api/trends/history", s.HistoryHandler)
}

// FlipRateHandler serves GET /api/trends/flips?app_id=&window=.
func (s *TrendsHTTP) FlipRateHandler(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	appID, ok := appIDFromQuery(w, r)
	if !ok {
		return
	}
	fr, err := ComputeFlipRate(r.Context(), s.Pool, appID, queryInt(r, "window", MinWindow))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "flip rate failed")
		return
	}
	writeJSON(w, http.StatusOK, fr)
}

// CostHandler serves GET /api/trends/cost/{runId}.
func (s *TrendsHTTP) CostHandler(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	runID, err := pgUUIDFromString(r.PathValue("runId"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid run id")
		return
	}
	rc, err := RunCosts(r.Context(), s.Pool, runID)
	if errors.Is(err, errNoRows) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cost rollup failed")
		return
	}
	writeJSON(w, http.StatusOK, rc)
}

// HistoryHandler serves GET /api/trends/history?app_id=&window=.
func (s *TrendsHTTP) HistoryHandler(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	appID, ok := appIDFromQuery(w, r)
	if !ok {
		return
	}
	h, err := HistoryRecent(r.Context(), s.Pool, appID, queryInt(r, "window", DefaultWindow))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "history failed")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// gate enforces the identity wall: authenticated + view permission for all
// trends reads (the surfaces gate verbs; trends has no writes).
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

func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b, _ := id.Value()
	return b.(string)
}

// uuidText normalizes the textual uuid form (lowercase hyphenated).
func uuidText(s string) string { return s }

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
