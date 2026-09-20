// Plans REST surface (T4): the plan catalog, per-version content and diff,
// the manual sync trigger, and the corpus read/decision endpoints. Wire
// shapes are locked verbatim to web/src/lib/types.ts — plans serve them,
// T5 renders them.
//
// Auth posture mirrors the runs surface (report.RunsHTTP): BFF identity
// headers via httpx everywhere, reads gated on RunViewPermission, writes
// (sync trigger, corpus decisions) on RunEditPermission.
package plans

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/NSXBet/e2ai/api/internal/corpus"
	"github.com/NSXBet/e2ai/api/internal/platform/github"
	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pmezard/go-difflib/difflib"
)

// wireMaxLimit bounds page size; the client asks for at most this many rows.
const wireMaxLimit = 100

// PlansHTTP mounts the plan catalog + corpus REST handlers.
type PlansHTTP struct {
	Pool   *pgxpool.Pool
	Client *github.Client // nil is fine: only the sync trigger needs it
}

// NewPlansHTTP builds the surface. Unmounted when postgres is down, like runs.
func NewPlansHTTP(pool *pgxpool.Pool, client *github.Client) *PlansHTTP {
	return &PlansHTTP{Pool: pool, Client: client}
}

// --- wire DTOs (locked verbatim to web/src/lib/types.ts) ---

type planDTO struct {
	ID              string    `json:"id"`
	App             string    `json:"app"`
	Title           string    `json:"title"`
	Surfaces        []string  `json:"surfaces"`
	// Surface is the plan's target surface (Phase 4). Locked vocabulary with
	// the TS side: "web"|"android"|"ios"; pre-phase-4 rows surface as "web".
	Surface         string    `json:"surface"`
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

type plansPageDTO struct {
	Plans      []planDTO `json:"plans"`
	Limit      int       `json:"limit"`
	Offset     int       `json:"offset"`
	NextOffset *int      `json:"next_offset"`
}

type planDetailDTO struct {
	Plan     planDTO          `json:"plan"`
	Versions []planVersionDTO `json:"versions"`
}

type versionContentDTO struct {
	Content     string          `json:"content"`
	Frontmatter json.RawMessage `json:"frontmatter"`
	ContentHash string          `json:"content_hash"`
	Status      string          `json:"status"`
}

type versionDiffDTO struct {
	Diff          string  `json:"diff"`
	BaseVersionID *string `json:"base_version_id"`
}

type corpusDTO struct {
	Behaviors []corpus.Behavior `json:"behaviors"`
	Learnings []learningDTO     `json:"learnings"`
}

// learningDTO trims to the locked wire: no app_id, no created_at.
type learningDTO struct {
	ID          string  `json:"id"`
	RunID       *string `json:"run_id"`
	Observation string  `json:"observation"`
	Status      string  `json:"status"`
}

// --- helpers ---

func writeWire(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeWire(w, status, map[string]string{"error": msg})
}

// gate enforces the identity wall: authenticated + permission. Reads use
// RunViewPermission, writes RunEditPermission (tiering per the naive v0
// access model — the BFF Okta wall does coarse auth, the surface gates verbs).
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

func queryInt(r *http.Request, key string, fallback int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return v
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	// uuid.UUID is [16]byte; a direct copy is exact. pgtype.UUID.Scan does
	// not accept google/uuid values — its error was discarded, leaving a
	// NULL-bindable zero value (caught by the promote/dismiss round-trip).
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

// parseUUID maps a wire id to a pgtype.UUID.
func parseUUID(ref string) (pgtype.UUID, error) {
	id, err := uuid.Parse(ref)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgUUID(id), nil
}

// nullableString renders a pgtype.Text as string|null.
func nullableString(t pgtype.Text) *string {
	if !t.Valid {
		return nil
	}
	s := t.String
	return &s
}

// nullableUUID renders a pgtype.UUID as string|null.
func nullableUUID(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}

// wirePlan maps a store row to the locked Plan shape.
func wirePlan(r planRow) planDTO {
	dto := planDTO{
		ID:              r.ID,
		App:             r.App,
		Surfaces:        r.Surfaces,
		Status:          r.Status,
		SourcePath:      r.SrcPath,
		LatestVersionID: r.LatestID,
		UpdatedAt:       r.Updated,
	}
	if r.Surface != nil && *r.Surface != "" {
		dto.Surface = *r.Surface
	} else {
		dto.Surface = SurfaceWeb // pre-phase-4 frontmatter rows have no surface key
	}
	if r.Title != nil {
		dto.Title = *r.Title
	}
	if r.Trigger != nil {
		dto.Trigger = *r.Trigger
	}
	return dto
}

// wireVersion maps a store row to the locked PlanVersionSummary shape.
func wireVersion(r versionRow) planVersionDTO {
	return planVersionDTO{
		ID:          r.ID.String(),
		PlanID:      r.PlanID,
		Version:     r.Version,
		ContentHash: r.Hash,
		Status:      r.Status,
		CreatedBy:   r.CreatedBy,
		CreatedAt:   r.CreatedAt,
	}
}

// --- handlers ---

// ListPlans serves GET /api/plans?app=&status=&limit=&offset=.
func (s *PlansHTTP) ListPlans(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	var appID pgtype.UUID
	if ref := r.URL.Query().Get("app"); ref != "" {
		id, err := ResolveApp(r.Context(), s.Pool, ref)
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusBadRequest, "unknown app")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "resolve app failed")
			return
		}
		appID = id
	}
	limit := queryInt(r, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	if limit > wireMaxLimit {
		limit = wireMaxLimit
	}
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	rows, err := ListPlans(r.Context(), s.Pool, appID, r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list plans failed")
		return
	}
	plans := []planDTO{}
	for _, row := range rows {
		plans = append(plans, wirePlan(row))
	}
	page := plansPageDTO{Limit: limit, Offset: offset, Plans: plans}
	if len(rows) == limit {
		next := offset + limit
		page.NextOffset = &next
	}
	writeWire(w, http.StatusOK, page)
}

// GetPlan serves GET /api/plans/{planID} → plan + version history.
func (s *PlansHTTP) GetPlan(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	planID := r.PathValue("planID")
	row, err := GetPlan(r.Context(), s.Pool, planID)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "plan not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load plan failed")
		return
	}
	versions, err := ListPlanVersions(r.Context(), s.Pool, planID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load versions failed")
		return
	}
	dtos := []planVersionDTO{}
	for _, v := range versions {
		dtos = append(dtos, wireVersion(v))
	}
	writeWire(w, http.StatusOK, planDetailDTO{Plan: wirePlan(*row), Versions: dtos})
}

// GetVersionContent serves GET /api/plan-versions/{versionID}/content.
func (s *PlansHTTP) GetVersionContent(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	vid, err := parseUUID(r.PathValue("versionID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad version id")
		return
	}
	v, err := GetVersionContent(r.Context(), s.Pool, vid)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load version failed")
		return
	}
	writeWire(w, http.StatusOK, versionContentDTO{
		Content:     v.Content,
		Frontmatter: json.RawMessage(v.FM),
		ContentHash: v.Hash,
		Status:      v.Status,
	})
}

// GetVersionDiff serves GET /api/plan-versions/{versionID}/diff?base_version_id=.
// Without an explicit base the baseline is the previous version of the same
// plan; with no baseline at all the diff shows the whole file as additions
// and base_version_id is null (locked wire note).
func (s *PlansHTTP) GetVersionDiff(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, false) {
		return
	}
	vid, err := parseUUID(r.PathValue("versionID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad version id")
		return
	}
	var baseVID *pgtype.UUID
	if ref := r.URL.Query().Get("base_version_id"); ref != "" {
		u, err := parseUUID(ref)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad base_version_id")
			return
		}
		baseVID = &u
	}
	baseContent, baseID, err := DiffBase(r.Context(), s.Pool, vid, baseVID)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "version not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load diff baseline failed")
		return
	}
	current, err := GetVersionContent(r.Context(), s.Pool, vid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load version failed")
		return
	}
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(baseContent),
		B:        difflib.SplitLines(current.Content),
		FromFile: "base",
		ToFile:   "current",
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "render diff failed")
		return
	}
	writeWire(w, http.StatusOK, versionDiffDTO{Diff: text, BaseVersionID: nullableUUIDPtr(baseID)})
}

// TriggerSync serves POST /api/apps/{app}/plans-sync: the manual trigger.
// One synchronous pass against the app's declared source — the queue,
// boot, and report triggers converge on the same SyncApp logic.
func (s *PlansHTTP) TriggerSync(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, true) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	if s.Client == nil {
		writeErr(w, http.StatusServiceUnavailable, "github client unavailable")
		return
	}
	appID, err := ResolveApp(r.Context(), s.Pool, r.PathValue("app"))
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "unknown app")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve app failed")
		return
	}
	result, err := SyncAppByID(r.Context(), s.Pool, s.Client, appID)
	switch {
	case errors.Is(err, ErrNoPlansSource):
		writeErr(w, http.StatusConflict, "app has no plans_source")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "sync failed")
		return
	}
	writeWire(w, http.StatusOK, result)
}

// ListCorpus serves GET /api/corpus?app= → behaviors + learnings for one app.
func (s *PlansHTTP) ListCorpus(w http.ResponseWriter, r *http.Request) {
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
	behaviors, err := corpus.ListBehaviors(r.Context(), s.Pool, appID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list behaviors failed")
		return
	}
	learnings, err := corpus.ListLearnings(r.Context(), s.Pool, appID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list learnings failed")
		return
	}
	out := corpusDTO{Behaviors: behaviors}
	for _, l := range learnings {
		out.Learnings = append(out.Learnings, learningDTO{
			ID:          l.ID.String(),
			RunID:       nullableUUID(l.RunID),
			Observation: l.Observation,
			Status:      l.Status,
		})
	}
	writeWire(w, http.StatusOK, out)
}

// PromoteLearning serves POST /api/corpus/learnings/{learningID}/promote:
// the reviewer promotes a run observation into a corpus behavior. Returns
// the promoted behavior on the wire (the store's return value).
func (s *PlansHTTP) PromoteLearning(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, true) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	lid, err := parseUUID(r.PathValue("learningID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad learning id")
		return
	}
	b, err := corpus.PromoteLearning(r.Context(), s.Pool, lid, httpx.UserOf(r))
	switch {
	case errors.Is(err, corpus.ErrNotFound):
		writeErr(w, http.StatusNotFound, "learning not found")
		return
	case errors.Is(err, corpus.ErrAlreadyPromoted):
		writeErr(w, http.StatusConflict, "learning already promoted")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "promote failed")
		return
	}
	writeWire(w, http.StatusOK, b)
}

// DismissLearning serves POST /api/corpus/learnings/{learningID}/dismiss.
// Returns the updated learning (locked CorpusLearning shape).
func (s *PlansHTTP) DismissLearning(w http.ResponseWriter, r *http.Request) {
	if !gate(w, r, true) {
		return
	}
	if s.Pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "postgres unavailable")
		return
	}
	lid, err := parseUUID(r.PathValue("learningID"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad learning id")
		return
	}
	if err := corpus.DismissLearning(r.Context(), s.Pool, lid); err != nil {
		switch {
		case errors.Is(err, corpus.ErrNotFound):
			writeErr(w, http.StatusNotFound, "learning not found")
			return
		case errors.Is(err, corpus.ErrAlreadyDismissed):
			writeErr(w, http.StatusConflict, "learning already dismissed")
			return
		case errors.Is(err, corpus.ErrAlreadyPromoted):
			writeErr(w, http.StatusConflict, "learning already promoted")
			return
		}
		writeErr(w, http.StatusInternalServerError, "dismiss failed")
		return
	}
	// Re-read post-decision so the client gets the updated status back.
	l, err := corpus.GetLearning(r.Context(), s.Pool, lid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load learning failed")
		return
	}
	writeWire(w, http.StatusOK, learningDTO{
		ID:          l.ID.String(),
		RunID:       nullableUUID(l.RunID),
		Observation: l.Observation,
		Status:      l.Status,
	})
}

// Mount registers the plans + corpus routes on the mux. Method prefixes keep
// reads GET-only and writes POST-only.
func (s *PlansHTTP) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plans", s.ListPlans)
	mux.HandleFunc("GET /api/plans/{planID}", s.GetPlan)
	mux.HandleFunc("GET /api/plan-versions/{versionID}/content", s.GetVersionContent)
	mux.HandleFunc("GET /api/plan-versions/{versionID}/diff", s.GetVersionDiff)
	mux.HandleFunc("POST /api/apps/{app}/plans-sync", s.TriggerSync)
	mux.HandleFunc("GET /api/corpus", s.ListCorpus)
	mux.HandleFunc("POST /api/corpus/learnings/{learningID}/promote", s.PromoteLearning)
	mux.HandleFunc("POST /api/corpus/learnings/{learningID}/dismiss", s.DismissLearning)
}

// nullableUUIDPtr renders a pgtype.UUID pointer as string|null.
func nullableUUIDPtr(u *pgtype.UUID) *string {
	if u == nil || !u.Valid {
		return nil
	}
	s := uuid.UUID(u.Bytes).String()
	return &s
}
