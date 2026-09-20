// T11: the report page data assembly plus the runs HTTP surface. The wire
// shape is locked against web/src/lib/{api.ts,types.ts} — every DTO here
// mirrors a types.ts interface exactly (field names, nullability, casing).
// Report-only: this surface reads the pipeline state and serves evidence; it
// never writes runs or gates the pipeline.
//
// Plain-language explanations are the deterministic template text T9 already
// persists (verdict.Note → runs.error_note; e.g. "execute: no parseable
// results.json") — carried through run.error_note. AI-written notes are
// Phase 3 and slot in behind the same key; no LLM calls live here.
package report

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NSXBet/e2ai/api/internal/platform/httpx"
	"github.com/NSXBet/e2ai/api/internal/runs"
)

// --- wire DTOs (locked to web/src/lib/types.ts) ---

// countsDTO is the wire TestCounts: the T9 fold (infra_error + inconclusive
// into skipped) already lives in the runs columns.
type countsDTO struct {
	TotalTests int `json:"total_tests"`
	Passed     int `json:"passed"`
	Failed     int `json:"failed"`
	Flaky      int `json:"flaky"`
	Skipped    int `json:"skipped"`
}

// runDTO is the wire Run. pr_title/commit_message have no runs columns yet —
// the locked contract keeps the keys and the values stay null.
type runDTO struct {
	ID            string     `json:"id"`
	AppID         string     `json:"app_id"`
	WorkflowRunID int64      `json:"workflow_run_id"`
	RunNumber     int32      `json:"run_number"`
	HeadSHA       string     `json:"head_sha"`
	HeadBranch    *string    `json:"head_branch"`
	Environment   string     `json:"environment"`
	TriggerSource string     `json:"trigger_source"`
	Status        string     `json:"status"`
	Verdict       *string    `json:"verdict"`
	PRNumber      *int32     `json:"pr_number"`
	PRState       *string    `json:"pr_state"`
	PRTitle       *string    `json:"pr_title"`
	CommitMessage *string    `json:"commit_message"`
	DurationMS    *int32     `json:"duration_ms"`
	ErrorNote     *string    `json:"error_note"`
	ReportURL     *string    `json:"report_url"`
	CommentURL    *string    `json:"comment_url"`
	CheckRunID    *int64     `json:"check_run_id"`
	StartedAt     *string    `json:"started_at"`
	FinishedAt    *string    `json:"finished_at"`
	CreatedAt     string     `json:"created_at"`
	Counts        *countsDTO `json:"counts"`
}

// testResultDTO is the wire TestResult: evidence the report viewer renders
// (the DB-only columns — status/attempts/classification/attachments raw —
// stay off the wire).
type testResultDTO struct {
	ID           string  `json:"id"`
	RunID        string  `json:"run_id"`
	Suite        *string `json:"suite"`
	Title        string  `json:"title"`
	File         *string `json:"file"`
	Line         *int32  `json:"line"`
	Project      string  `json:"project"`
	Outcome      string  `json:"outcome"`
	DurationMS   *int32  `json:"duration_ms"`
	ErrorMessage *string `json:"error_message"`
}

// artifactDTO is the wire Artifact; kind folds to the four wire kinds.
type artifactDTO struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	SizeBytes *int64 `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
}

// runsPageDTO is the wire RunsPage envelope: {"runs", limit, offset,
// next_offset}.
type runsPageDTO struct {
	Runs       []runDTO `json:"runs"`
	Limit      int      `json:"limit"`
	Offset     int      `json:"offset"`
	NextOffset *int     `json:"next_offset"`
}

// runDetailDTO is the wire RunDetail.
type runDetailDTO struct {
	Run         runDTO          `json:"run"`
	TestResults []testResultDTO `json:"test_results"`
	Artifacts   []artifactDTO   `json:"artifacts"`
}

// stepDTO is one filmstrip entry: a per-test step timeline.
type stepDTO struct {
	TestID      string   `json:"test_id"`
	Title       string   `json:"title"`
	Outcome     string   `json:"outcome"`
	DurationMS  *int32   `json:"duration_ms"`
	Screenshots []string `json:"screenshots"`
}

// reportDataDTO is the wire ReportData: one blob the report viewer renders
// directly — KPI counts, failures first, then the filmstrip.
type reportDataDTO struct {
	Run      runDTO          `json:"run"`
	Counts   countsDTO       `json:"counts"`
	Projects []string        `json:"projects"`
	Failures []testResultDTO `json:"failures"`
	Flaky    []testResultDTO `json:"flaky"`
	Steps    []stepDTO       `json:"steps"`
}

// RunsHTTP mounts the runs surface: paginated run list, run detail, the
// report page blob, and artifact file serving. Auth posture: BFF identity
// headers (httpx) everywhere, plus the signed ?e2ai= token on the per-run
// endpoints so the PR comment link and its embedded screenshots work
// outside shell auth.
type RunsHTTP struct {
	Pool *pgxpool.Pool // runs queries
	Root string        // artifact volume root (E2AI_ARTIFACTS_DIR)
	// Secret is E2AI_REPORT_LINK_SECRET; empty means dev mode where unsigned
	// links verify (same semantics as T10's VerifyReportToken).
	Secret string
}

// wireMaxLimit caps the runs page limit on this surface (the wire contract's
// clamp; runs.Page's internal cap of 200 stays available to other callers).
const wireMaxLimit = 100

// NewRunsHTTP builds the runs/artifacts HTTP surface for main.go's mux.
func NewRunsHTTP(pool *pgxpool.Pool, artifactsRoot, linkSecret string) *RunsHTTP {
	return &RunsHTTP{Pool: pool, Root: artifactsRoot, Secret: linkSecret}
}

// --- enum mapping: DB lowercase → wire uppercase ---

func wireStatus(s runs.Status) string {
	if s == runs.StatusWaitingRollout {
		return "WAITING_ROLLOUT"
	}
	return strings.ToUpper(string(s))
}

func wireVerdict(v pgtype.Text) *string {
	if !v.Valid || v.String == "" {
		return nil
	}
	out := strings.ToUpper(v.String)
	return &out
}

func wireTriggerSource(s string) string { return strings.ToUpper(s) }

// wireOutcome maps the test_results outcome (Playwright top-level status
// enum as stored by T8) to the wire TestOutcome.
func wireOutcome(s string) string {
	switch s {
	case "expected":
		return "PASS"
	case "unexpected":
		return "FAIL"
	case "flaky":
		return "FLAKY"
	case "skipped":
		return "SKIPPED"
	default:
		return strings.ToUpper(s)
	}
}

// wireArtifactKind folds the artifacts.kind values onto the four wire kinds.
// html_report/blob/log are closest to "results" on the wire.
func wireArtifactKind(s string) string {
	switch s {
	case "screenshot", "trace", "video":
		return s
	case "results_json":
		return "results"
	default:
		return "results"
	}
}

// --- row → wire mapping ---

func textPtr(t pgtype.Text) *string {
	if !t.Valid || t.String == "" {
		return nil
	}
	return &t.String
}

func tsPtr(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	out := t.Time.UTC().Format(time.RFC3339Nano)
	return &out
}

// wireRun maps a runs row onto the wire Run shape.
func wireRun(r *runs.Run) runDTO {
	dto := runDTO{
		ID:            r.ID.String(),
		AppID:         r.AppID.String(),
		WorkflowRunID: r.WorkflowRunID,
		RunNumber:     r.RunNumber,
		HeadSHA:       r.HeadSHA,
		Environment:   r.Environment,
		TriggerSource: wireTriggerSource(r.TriggerSource),
		Status:        wireStatus(r.Status),
		Verdict:       wireVerdict(r.Verdict),
		PRState:       wireUpperPtr(r.PRState),
		ReportURL:     textPtr(r.ReportURL),
		CommentURL:    textPtr(r.CommentURL),
		ErrorNote:     textPtr(r.ErrorNote),
		HeadBranch:    textPtr(r.HeadBranch),
		CreatedAt:     r.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
	}
	if r.PRNumber.Valid {
		v := r.PRNumber.Int32
		dto.PRNumber = &v
	}
	if r.CheckRunID.Valid {
		v := r.CheckRunID.Int64
		dto.CheckRunID = &v
	}
	if r.DurationMS.Valid {
		v := r.DurationMS.Int32
		dto.DurationMS = &v
	}
	dto.StartedAt = tsPtr(r.StartedAt)
	dto.FinishedAt = tsPtr(r.FinishedAt)
	if r.TotalTests.Valid {
		dto.Counts = &countsDTO{
			TotalTests: int(r.TotalTests.Int32),
			Passed:     int(r.Passed.Int32),
			Failed:     int(r.Failed.Int32),
			Flaky:      int(r.Flaky.Int32),
			Skipped:    int(r.Skipped.Int32),
		}
	}
	return dto
}

func wireUpperPtr(t pgtype.Text) *string {
	if !t.Valid || t.String == "" {
		return nil
	}
	out := strings.ToUpper(t.String)
	return &out
}

func wireTestResult(tr runs.TestResult) testResultDTO {
	dto := testResultDTO{
		ID:         tr.ID.String(),
		RunID:      tr.RunID.String(),
		Title:      tr.Title,
		Project:    tr.Project.String,
		Outcome:    wireOutcome(tr.Outcome),
		Suite:      textPtr(tr.Suite),
		File:       textPtr(tr.File),
		Line:       int32Ptr(tr.Line),
		DurationMS: int32Ptr(tr.DurationMS),
	}
	dto.ErrorMessage = textPtr(tr.ErrorMessage)
	return dto
}

func int32Ptr(v pgtype.Int4) *int32 {
	if !v.Valid {
		return nil
	}
	out := v.Int32
	return &out
}

func wireArtifact(a runs.Artifact) artifactDTO {
	dto := artifactDTO{
		ID:    a.ID.String(),
		RunID: a.RunID.String(),
		Kind:  wireArtifactKind(a.Kind),
		Path:  a.Path,
	}
	if a.SizeBytes.Valid {
		v := a.SizeBytes.Int64
		dto.SizeBytes = &v
	}
	dto.CreatedAt = a.CreatedAt.Time.UTC().Format(time.RFC3339Nano)
	return dto
}

// artifactPageURL is the browser-facing URL for one run-relative artifact
// file: the BFF keeps the /e2ai prefix and this surface serves /artifacts
// behind it (same convention as the T10 comment images).
func artifactPageURL(runID pgtype.UUID, rel string) string {
	return "/e2ai/artifacts/" + runID.String() + "/" + rel
}

// --- assembly ---

// BuildRunDetail maps a full run surface onto the wire RunDetail.
func BuildRunDetail(d *runs.RunDetail) runDetailDTO {
	dto := runDetailDTO{Run: wireRun(d.Run), TestResults: []testResultDTO{}, Artifacts: []artifactDTO{}}
	for _, tr := range d.TestResults {
		dto.TestResults = append(dto.TestResults, wireTestResult(tr))
	}
	for _, a := range d.Artifacts {
		dto.Artifacts = append(dto.Artifacts, wireArtifact(a))
	}
	return dto
}

// reportOutcomeRank orders the filmstrip failures-first: FAIL, FLAKY, then
// the passing tail.
var reportOutcomeRank = map[string]int{"FAIL": 0, "FLAKY": 1, "PASS": 2, "SKIPPED": 3}

// BuildReportData assembles the report page blob: KPI counts, failures first,
// projects, and the per-test filmstrip. Deterministic — no LLM calls.
func BuildReportData(d *runs.RunDetail) reportDataDTO {
	dto := reportDataDTO{
		Run:      wireRun(d.Run),
		Counts:   reportCounts(d),
		Projects: []string{},
		Failures: []testResultDTO{},
		Flaky:    []testResultDTO{},
		Steps:    []stepDTO{},
	}

	seen := map[string]bool{}
	for _, tr := range d.TestResults {
		wire := wireTestResult(tr)
		if wire.Outcome == "FAIL" {
			dto.Failures = append(dto.Failures, wire)
		}
		if wire.Outcome == "FLAKY" {
			dto.Flaky = append(dto.Flaky, wire)
		}
		if tr.Project.String != "" && !seen[tr.Project.String] {
			seen[tr.Project.String] = true
			dto.Projects = append(dto.Projects, tr.Project.String)
		}

		shots := []string{}
		for _, shot := range shotPaths(tr.Attachments) {
			shots = append(shots, artifactPageURL(tr.RunID, shot))
		}
		dto.Steps = append(dto.Steps, stepDTO{
			TestID:      wire.ID,
			Title:       wire.Title,
			Outcome:     wire.Outcome,
			DurationMS:  wire.DurationMS,
			Screenshots: shots,
		})
	}
	// Failures first (deterministic stable order inside each group).
	sort.SliceStable(dto.Steps, func(i, j int) bool {
		return reportOutcomeRank[dto.Steps[i].Outcome] < reportOutcomeRank[dto.Steps[j].Outcome]
	})
	return dto
}

// reportCounts is the report-page KPI block: the run's persisted counts when
// the report job has run, else a fold derived from the test evidence (same
// T9 fold: infra_error + inconclusive → skipped).
func reportCounts(d *runs.RunDetail) countsDTO {
	if d.Run.TotalTests.Valid {
		return countsDTO{
			TotalTests: int(d.Run.TotalTests.Int32),
			Passed:     int(d.Run.Passed.Int32),
			Failed:     int(d.Run.Failed.Int32),
			Flaky:      int(d.Run.Flaky.Int32),
			Skipped:    int(d.Run.Skipped.Int32),
		}
	}
	var c countsDTO
	for _, tr := range d.TestResults {
		switch wireOutcome(tr.Outcome) {
		case "PASS":
			c.Passed++
		case "FAIL":
			c.Failed++
		case "FLAKY":
			c.Flaky++
		default:
			c.Skipped++
		}
	}
	c.TotalTests = len(d.TestResults)
	return c
}

// --- HTTP handlers ---

func writeWire(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeWire(w, status, map[string]string{"error": msg})
}

// gate enforces the surface's auth posture: a valid signed ?e2ai= token
// (per-run endpoints; empty secret is dev mode where unsigned links verify)
// or a BFF identity holding the run-view permission. Returns false when the
// response is already written.
func (s *RunsHTTP) gate(w http.ResponseWriter, r *http.Request, runID uuid.UUID) bool {
	if VerifyReportToken(runID, r.URL.Query().Get("e2ai"), s.Secret) {
		return true
	}
	identity, ok := httpx.IdentityFromContext(r.Context())
	if ok && identity.HasPermission(httpx.RunViewPermission) {
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
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

// ListRuns serves GET /api/runs?limit=&offset= — the locked paginated
// envelope, newest first. Limit clamps to 100 on this surface.
func (s *RunsHTTP) ListRuns(w http.ResponseWriter, r *http.Request) {
	if !s.gate(w, r, uuid.Nil) {
		return
	}
	limit := queryInt(r, "limit", 50)
	if limit < 0 {
		limit = 50
	}
	if limit > wireMaxLimit {
		limit = wireMaxLimit
	}
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	page, err := runs.ListRuns(r.Context(), s.Pool, pgtype.UUID{}, runs.Page{Limit: limit, Offset: offset})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list runs failed")
		return
	}
	out := runsPageDTO{Runs: make([]runDTO, 0, len(page)), Limit: limit, Offset: offset}
	for i := range page {
		out.Runs = append(out.Runs, wireRun(&page[i]))
	}
	if len(page) == limit {
		next := offset + limit
		out.NextOffset = &next
	}
	writeWire(w, http.StatusOK, out)
}

// GetRun serves GET /api/runs/{id} — detail including test_results and the
// artifact index. Unknown ids are 404.
func (s *RunsHTTP) GetRun(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if !s.gate(w, r, runID) {
		return
	}
	detail, err := runs.GetRunDetail(r.Context(), s.Pool, pgUUID(runID))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load run failed")
		return
	}
	if detail == nil || detail.Run == nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	writeWire(w, http.StatusOK, BuildRunDetail(detail))
}

// RunReport serves GET /api/runs/{id}/report — the report page blob.
func (s *RunsHTTP) RunReport(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if !s.gate(w, r, runID) {
		return
	}
	detail, err := runs.GetRunDetail(r.Context(), s.Pool, pgUUID(runID))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load run failed")
		return
	}
	if detail == nil || detail.Run == nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	writeWire(w, http.StatusOK, BuildReportData(detail))
}
