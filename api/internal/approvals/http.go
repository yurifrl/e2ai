// Approvals REST surface (APPR-02): the pending queue (newest first) and
// the two attributed decision endpoints. Wire shapes are locked verbatim to
// web/src/lib/types.ts — ApprovalsQueue, PlanApproval — and the endpoints
// api.ts calls (getApprovalsQueue, approvePlanVersion, rejectPlanVersion)
// exist exactly as the web module expects.
//
// Auth posture mirrors the plans surface (plans.PlansHTTP): BFF identity
// headers via httpx, reads gated on RunViewPermission, writes (decisions)
// on RunEditPermission. One extra rule the plans surface does not have:
// decisions must name a human — the httpx dev-fallback identity ("you", or
// the configured DevFallbackUser) is refused with 400 so a proxied-in dev
// shell or an un-proxied curl can never be recorded as an approver.
package approvals

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ApprovalsHTTP mounts the approvals queue + decision handlers.
type ApprovalsHTTP struct {
	Pool *pgxpool.Pool
	// FallbackUser is the api binary's configured dev-fallback identity
	// (cfg.DevFallbackUser). Writes attributed to it are refused.
	FallbackUser string
}

// NewApprovalsHTTP builds the surface. Unmounted when postgres is down,
// like the plans and runs surfaces.
func NewApprovalsHTTP(pool *pgxpool.Pool, fallbackUser string) *ApprovalsHTTP {
	return &ApprovalsHTTP{Pool: pool, FallbackUser: fallbackUser}
}

// --- wire DTOs (locked verbatim to web/src/lib/types.ts) ---

type planDTO struct {
	ID              string    `json:"id"`
	App             string    `json:"app"`
	Title           string    `json:"title"`
	Surfaces        []string  `json:"surfaces"`
	Trigger         string    `json:"trigger"`
	Status          string    `json:"status"`
	SourcePath      string    `json:"source_path"`
	LatestVersionID *string   `json:"latest_version_id"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type planVersionDTO struct {
	ID          string    `json:"id"`
	PlanID      string    `json:"plan_id"`
	Version     string    `json:"version"`
	ContentHash string    `json:"content_hash"`
	Status      string    `json:"status"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

type approvalDTO struct {
	ID            string    `json:"id"`
	PlanID        string    `json:"plan_id"`
	PlanVersionID string    `json:"plan_version_id"`
	Decision      string    `json:"decision"`
	Approver      string    `json:"approver"`
	Notes         *string   `json:"notes"`
	DecidedAt     time.Time `json:"decided_at"`
}

type pendingApprovalDTO struct {
	Plan    planDTO        `json:"plan"`
	Version planVersionDTO `json:"version"`
}

type approvalsQueueDTO struct {
	Pending []pendingApprovalDTO `json:"pending"`
	Recent  []approvalDTO        `json:"recent"`
}

// --- handlers ---

// Queue serves GET /api/approvals → { pending, recent }.
func (s *ApprovalsHTTP) Queue(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	pending, err := listPending(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list pending approvals failed")
		return
	}
	recent, err := listRecent(r.Context(), s.Pool)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list recent approvals failed")
		return
	}
	writeWire(w, http.StatusOK, approvalsQueueDTO{Pending: pending, Recent: recent})
}

// Decide serves POST /api/plan-versions/{versionID}/approve|reject with an
// optional { notes } body, returning the attributed PlanApproval.
func (s *ApprovalsHTTP) Decide(decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !gate(w, r, true) {
			return
		}
		if s.Pool == nil {
			writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
			return
		}
		versionID, err := parseUUID(r.PathValue("versionID"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad version id")
			return
		}

		user := resolvedUser(r)
		if user == "" || user == "you" || (s.FallbackUser != "" && user == s.FallbackUser) {
			writeErr(w, http.StatusBadRequest, "approvals require a real identity")
			return
		}

		var body struct {
			Notes string `json:"notes"`
		}
		// notes are optional; an empty or absent body is fine.
		_ = json.NewDecoder(r.Body).Decode(&body)

		a, err := Decide(r.Context(), s.Pool, versionID, decision, user, body.Notes)
		switch {
		case errors.Is(err, ErrNotFound):
			writeErr(w, http.StatusNotFound, "unknown version")
			return
		case errors.Is(err, ErrNotPending):
			writeErr(w, http.StatusConflict, "version is not awaiting a decision")
			return
		case err != nil:
			writeErr(w, http.StatusInternalServerError, "decide failed")
			return
		}
		writeWire(w, http.StatusOK, approvalDTO{
			ID:            uuid.UUID(a.ID.Bytes).String(),
			PlanID:        a.PlanID,
			PlanVersionID: versionID.String(),
			Decision:      a.Decision,
			Approver:      a.Approver,
			Notes:         a.Notes,
			DecidedAt:     a.DecidedAt,
		})
	}
}

// Mount registers the approvals routes on the mux. Method prefixes keep
// reads GET-only and decisions POST-only; the decide endpoints pair with
// the plans surface's read endpoints on the same version path.
func (s *ApprovalsHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/approvals", s.Queue)
	mux.HandleFunc("POST /api/plan-versions/{versionID}/approve", s.Decide("approved"))
	mux.HandleFunc("POST /api/plan-versions/{versionID}/reject", s.Decide("rejected"))
}

// --- queue queries ---

// listPending loads every version awaiting a decision, each with its plan
// row (the same latest-version frontmatter join the plans list uses) so the
// UI renders plan + changed version in one response.
func listPending(ctx context.Context, pool *pgxpool.Pool) ([]pendingApprovalDTO, error) {
	rows, err := pool.Query(ctx, `
		select pv.id, pv.plan_id, pv.version, pv.content_hash, pv.status, pv.created_by, pv.created_at,
		       a.owner || '/' || a.repo,
		       f.title, f.surfaces, f.trigger,
		       p.status, p.source_path, p.latest_version_id, p.updated_at
		from plan_versions pv
		join plans p on p.id = pv.plan_id
		join registered_apps a on a.id = p.app_id
		left join plan_versions lv on lv.id = p.latest_version_id
		left join lateral (select * from jsonb_to_record(lv.frontmatter)
			as x(title text, surfaces text[], trigger text)) f on lv is not null
		where pv.status = $1
		order by pv.created_at desc`, StatusPendingApproval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pendingApprovalDTO{}
	for rows.Next() {
		var v planVersionDTO
		var p planDTO
		var title, trigger pgtype.Text
		var surfaces []string
		var surfacesNull bool
		var latestID pgtype.UUID
		if err := rows.Scan(&v.ID, &v.PlanID, &v.Version, &v.ContentHash, &v.Status,
			&v.CreatedBy, &v.CreatedAt,
			&p.App, &title, &surfaces, &surfacesNull, &trigger,
			&p.Status, &p.SourcePath, &latestID, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.ID = v.PlanID
		p.Title = nullableText(title)
		p.Trigger = nullableText(trigger)
		if surfaces != nil {
			p.Surfaces = surfaces
		}
		p.LatestVersionID = nullableUUID(latestID)
		out = append(out, pendingApprovalDTO{Plan: p, Version: v})
	}
	return out, rows.Err()
}

// listRecent loads the latest attributed decisions (newest first).
func listRecent(ctx context.Context, pool *pgxpool.Pool) ([]approvalDTO, error) {
	rows, err := pool.Query(ctx, `
		select pa.id, p.id, pa.plan_version_id, pa.decision, pa.approver, pa.notes, pa.decided_at
		from plan_approvals pa
		join plan_versions pv on pv.id = pa.plan_version_id
		join plans p on p.id = pv.plan_id
		order by pa.decided_at desc
		limit 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []approvalDTO{}
	for rows.Next() {
		var a approvalDTO
		var id, vid pgtype.UUID
		var notes *string
		if err := rows.Scan(&id, &a.PlanID, &vid, &a.Decision, &a.Approver, &notes, &a.DecidedAt); err != nil {
			return nil, err
		}
		a.ID = uuid.UUID(id.Bytes).String()
		a.PlanVersionID = uuid.UUID(vid.Bytes).String()
		a.Notes = notes
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- helpers (same style as the plans surface) ---

func writeWire(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeWire(w, status, map[string]string{"error": msg})
}

// gate enforces the identity wall: authenticated + permission. Reads use
// RunViewPermission, decisions RunEditPermission (tiering per the naive v0
// access model).
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

// resolvedUser picks the attribution identity: the middleware-resolved
// identity when present, else the bare X-User header (UserOf). Placeholder
// identities are refused by the caller before any write.
func resolvedUser(r *http.Request) string {
	if identity, ok := httpx.IdentityFromContext(r.Context()); ok && identity.User != "" {
		return identity.User
	}
	return httpx.UserOf(r)
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

func parseUUID(ref string) (pgtype.UUID, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgUUID(id), nil
}

func nullableText(t pgtype.Text) string {
	if !t.Valid {
		return ""
	}
	return t.String
}

func nullableUUID(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}
