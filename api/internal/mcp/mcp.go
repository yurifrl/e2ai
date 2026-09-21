package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/NSXBet/e2ai/api/internal/flakes"
	"github.com/NSXBet/e2ai/api/internal/report"
	"github.com/NSXBet/e2ai/api/internal/runs"
	"github.com/NSXBet/e2ai/api/internal/trends"
)

// --- JSON-RPC envelope ---

const (
	protocolVersion = "2025-06-18"

	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// --- tool-call plumbing ---

// argError is a caller mistake in a tool's arguments — answered as a
// JSON-RPC protocol error (-32602), per the MCP spec.
type argError struct{ msg string }

func (e argError) Error() string { return e.msg }

// toolError is a domain failure inside an executed tool (unknown run, etc.)
// — answered as an MCP result with isError:true, not a protocol error.
type toolError struct{ msg string }

func (e toolError) Error() string { return e.msg }

// content is the MCP result content block; the text block carries the same
// payload as structuredContent so text-only clients see everything.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content           []content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// toolDef is one read-only tool: descriptor + handler.
type toolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Handler takes the parsed arguments and returns the payload that
	// becomes both the text block and structuredContent.
	Handler func(s *Surface, ctx context.Context, args map[string]any) (any, error)
}

// toolCatalog is the complete read-only surface (MCP-01: no mutation tools).
var toolCatalog = []toolDef{
	{
		Name: "list_runs",
		Description: "List a registered app's e2ai runs, newest first, with status/verdict filters and " +
			"a bounded window (counted in runs, not days). Each row cites the run id, the pipeline " +
			"counts and token-gated evidence links (report page, artifact base).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"app_id":  map[string]any{"type": "string", "description": "registered app uuid (required)"},
				"status":  map[string]any{"type": "string", "description": "filter: queued|waiting-rollout|provisioning|executing|reporting|completed|failed"},
				"verdict": map[string]any{"type": "string", "description": "filter: pass|fail|flaky|infra_error|infra_flake|inconclusive"},
				"window":  map[string]any{"type": "integer", "description": "max runs returned (default 20, cap 200)"},
			},
			"required": []string{"app_id"},
		},
		Handler: (*Surface).listRuns,
	},
	{
		Name: "get_run",
		Description: "Full detail for one run: pipeline state, counts, per-test evidence (outcome, " +
			"attempts, error message, REPT-02 classification + plain-language classification_reason) " +
			"and the artifact index, each attachment carrying a token-gated URL.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id": map[string]any{"type": "string", "description": "run uuid (required)"},
			},
			"required": []string{"run_id"},
		},
		Handler: (*Surface).getRun,
	},
	{
		Name: "get_failure_evidence",
		Description: "Per-test failure evidence for one run: failed/flaky tests with error messages, " +
			"REPT-02 classification + reason, and token-gated screenshot/trace/video artifact URLs. " +
			"Optionally narrow to one test by exact title.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id": map[string]any{"type": "string", "description": "run uuid (required)"},
				"title":  map[string]any{"type": "string", "description": "optional exact test title filter"},
			},
			"required": []string{"run_id"},
		},
		Handler: (*Surface).getFailureEvidence,
	},
	{
		Name: "get_flake_rates",
		Description: "Per-test flake rates for one app over a bounded window of executed runs " +
			"(FLAK-01): flaky/fail/pass run counts, the flake rate and an unstable flag.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"app_id": map[string]any{"type": "string", "description": "registered app uuid (required)"},
				"window": map[string]any{"type": "integer", "description": "runs in the analysis window (default per TREN-01, cap 200)"},
			},
			"required": []string{"app_id"},
		},
		Handler: (*Surface).getFlakeRates,
	},
	{
		Name: "get_quarantine",
		Description: "The test quarantine store for an app (FLAK-02): active or lifted quarantine " +
			"episodes with reasons, evidence and who quarantined the test. Report-only — quarantine " +
			"never mutates the app under test.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"app_id": map[string]any{"type": "string", "description": "registered app uuid (optional; omit for all apps)"},
				"status": map[string]any{"type": "string", "description": "filter: active|lifted (optional; omit for both)"},
			},
		},
		Handler: (*Surface).getQuarantine,
	},
}

// initializeResult is the spec's InitializeResult.
type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      map[string]any `json:"serverInfo"`
	Instructions    string         `json:"instructions"`
}

// serveRPC parses the JSON-RPC envelope and dispatches. Notifications (no
// id) are accepted with 202 and no body; requests always get exactly one
// JSON-RPC response.
func (s *Surface) serveRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeParse, "mcp: unreadable request body")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, codeParse, "mcp: invalid JSON")
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, codeInvalidRequest, "mcp: not a JSON-RPC 2.0 request")
		return
	}
	if idNull(req.ID) {
		// Notification: never answered (Streamable HTTP uses 202).
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "initialize":
		writeRPC(w, http.StatusOK, req.ID, initializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    map[string]any{"tools": map[string]any{}},
			ServerInfo:      map[string]any{"name": "e2ai-mcp", "version": "0.1.0"},
			Instructions: "Read-only access to e2ai run history, failure evidence, flake rates and the " +
				"test quarantine. Every tool is read-only; evidence links are token-gated and cite run ids.",
		}, nil)
	case "ping":
		writeRPC(w, http.StatusOK, req.ID, map[string]any{}, nil)
	case "tools/list":
		tools := make([]map[string]any, 0, len(toolCatalog))
		for i := range toolCatalog {
			t := toolCatalog[i]
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
			})
		}
		writeRPC(w, http.StatusOK, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		s.callTool(w, r, req)
	default:
		writeRPCError(w, http.StatusOK, req.ID, codeMethodNotFound, "mcp: unknown method "+req.Method)
	}
}

// callTool dispatches tools/call: unknown tool or malformed arguments are
// protocol errors (-32602); domain failures surface as isError results.
func (s *Surface) callTool(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "mcp: tools/call requires a tool name")
		return
	}
	def := findTool(params.Name)
	if def == nil {
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "mcp: unknown tool "+params.Name)
		return
	}
	args := map[string]any{}
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, "mcp: tool arguments must be an object")
			return
		}
	}
	payload, err := def.Handler(s, r.Context(), args)
	switch {
	case err == nil:
		raw, _ := json.Marshal(payload)
		writeRPC(w, http.StatusOK, req.ID, toolResult{
			Content:           []content{{Type: "text", Text: string(raw)}},
			StructuredContent: payload,
		}, nil)
	case errors.As(err, new(argError)):
		writeRPCError(w, http.StatusOK, req.ID, codeInvalidParams, err.Error())
	case errors.As(err, new(toolError)):
		writeRPC(w, http.StatusOK, req.ID, toolResult{
			Content: []content{{Type: "text", Text: err.Error()}},
			IsError: true,
		}, nil)
	default:
		// Internal query failure: no internals leak, just a tool error.
		writeRPC(w, http.StatusOK, req.ID, toolResult{
			Content: []content{{Type: "text", Text: "mcp: tool failed"}},
			IsError: true,
		}, nil)
	}
}

func findTool(name string) *toolDef {
	for i := range toolCatalog {
		if toolCatalog[i].Name == name {
			return &toolCatalog[i]
		}
	}
	return nil
}

func idNull(id json.RawMessage) bool {
	return len(id) == 0 || string(id) == "null"
}

func writeRPC(w http.ResponseWriter, status int, id json.RawMessage, result any, rpcErr *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	writeRPC(w, status, id, nil, &rpcError{Code: code, Message: msg})
}

// --- argument parsing ---

func argString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

// argInt reads an optional bounded integer; out-of-range values clamp to
// [1, max] (a bad window is caller noise, not an error worth rejecting).
func argInt(args map[string]any, key string, fallback, max int) int {
	f, ok := args[key].(float64)
	if !ok || f <= 0 {
		return fallback
	}
	n := int(f)
	if n > max {
		return max
	}
	return n
}

// argUUID parses a required uuid argument or fails with -32602.
func argUUID(args map[string]any, key string) (pgtype.UUID, error) {
	raw := argString(args, key)
	if raw == "" {
		return pgtype.UUID{}, argError{msg: "mcp: " + key + " is required"}
	}
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return pgtype.UUID{}, argError{msg: "mcp: " + key + " must be a uuid"}
	}
	return id, nil
}

// argOptionalUUID parses an optional uuid argument; "" means absent.
func argOptionalUUID(args map[string]any, key string) (pgtype.UUID, bool, error) {
	raw := argString(args, key)
	if raw == "" {
		return pgtype.UUID{}, false, nil
	}
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return pgtype.UUID{}, false, argError{msg: "mcp: " + key + " must be a uuid"}
	}
	return id, true, nil
}

// --- shared payload types ---

// evidence carries the machine-checkable links every response cites.
type evidence struct {
	ReportURL    string `json:"report_url,omitempty"`
	ArtifactsURL string `json:"artifacts_base_url,omitempty"`
}

// countsDTO mirrors the pipeline fold stored on the run (null until the run
// has been reported).
type countsDTO struct {
	TotalTests int `json:"total_tests"`
	Passed     int `json:"passed"`
	Failed     int `json:"failed"`
	Flaky      int `json:"flaky"`
	Skipped    int `json:"skipped"`
}

// runDTO is one run as the wire sees it: pipeline state plus evidence links.
type runDTO struct {
	RunID         string     `json:"run_id"`
	AppID         string     `json:"app_id"`
	App           string     `json:"app"` // owner/repo
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
	Counts        *countsDTO `json:"counts"`
	DurationMS    *int32     `json:"duration_ms"`
	ErrorNote     *string    `json:"error_note"`
	StartedAt     *string    `json:"started_at"`
	FinishedAt    *string    `json:"finished_at"`
	CreatedAt     string     `json:"created_at"`
	Evidence      evidence   `json:"evidence"`
}

// attachmentDTO is one test attachment; URL is the token-gated artifact link
// (the same gate the report surface serves through).
type attachmentDTO struct {
	Kind string `json:"kind"` // screenshot|trace|video|log
	Path string `json:"path"`
	URL  string `json:"url,omitempty"`
}

// testDTO is one test_results row with the REPT-02 classification fields.
// classification_reason is mandatory: when a classification exists the
// reason is derived from the stored error evidence; when classification is
// null the reason is null and classification_note says "classification: none".
type testDTO struct {
	ID                   string          `json:"id"`
	Suite                *string         `json:"suite"`
	Title                string          `json:"title"`
	File                 *string         `json:"file"`
	Line                 *int32          `json:"line"`
	Project              *string         `json:"project"`
	Outcome              string          `json:"outcome"`
	Status               string          `json:"status"`
	Attempts             int32           `json:"attempts"`
	DurationMS           *int32          `json:"duration_ms"`
	ErrorMessage         *string         `json:"error_message"`
	Classification       *string         `json:"classification"`
	ClassificationReason *string         `json:"classification_reason"`
	ClassificationNote   *string         `json:"classification_note,omitempty"`
	Attachments          []attachmentDTO `json:"attachments"`
}

// artifactDTO is the run's artifact index entry with its token-gated URL.
type artifactDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	SizeBytes *int64 `json:"size_bytes"`
	CreatedAt string `json:"created_at"`
	URL       string `json:"url,omitempty"`
}

// --- conversion helpers ---

func uuidString(id pgtype.UUID) string {
	b, _ := id.Value()
	return b.(string)
}

func textPtr(t pgtype.Text) *string {
	if !t.Valid || t.String == "" {
		return nil
	}
	return &t.String
}

func intPtr(t pgtype.Int4) *int32 {
	if !t.Valid {
		return nil
	}
	return &t.Int32
}

func tsPtr(t pgtype.Timestamptz) *string {
	if !t.Valid {
		return nil
	}
	out := t.Time.UTC().Format(time.RFC3339)
	return &out
}

func tsString(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

// --- evidence links ---

// artifactToken recomputes the report surface's per-run token: the artifact
// gate verifies "e2ai={hex(hmac-sha256(runID, secret))}", so links built here
// verify through report.VerifyReportToken (the report package's builder is
// unexported, so the algorithm lives here and is round-trip tested against
// the real verifier).
func (s *Surface) artifactToken(runID pgtype.UUID) string {
	if s.LinkSecret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(s.LinkSecret))
	mac.Write([]byte(uuidString(runID)))
	return hex.EncodeToString(mac.Sum(nil))
}

// evidenceLinks builds the run-level link block: the signed report-page URL
// (report.ReportLink, the sticky comment's format) and the artifact base the
// per-attachment URLs extend.
func (s *Surface) evidenceLinks(runID pgtype.UUID) evidence {
	var ev evidence
	if s.ReportBase == "" {
		return ev
	}
	base := strings.TrimRight(s.ReportBase, "/")
	ev.ReportURL = report.ReportLink(uuid.UUID(runID.Bytes), base, s.LinkSecret)
	ev.ArtifactsURL = base + "/e2ai/artifacts/" + uuidString(runID) + "/"
	return ev
}

// artifactURL attaches the token to an artifact path.
func (s *Surface) artifactURL(runID pgtype.UUID, path string) string {
	if s.ReportBase == "" {
		return ""
	}
	url := strings.TrimRight(s.ReportBase, "/") + "/e2ai/artifacts/" + uuidString(runID) + "/" + strings.TrimLeft(path, "/")
	if token := s.artifactToken(runID); token != "" {
		url += "?e2ai=" + token
	}
	return url
}

// classificationReason is the REPT-02 mandate: every get_run test carries a
// plain-language classification reason. The DB stores the machine class
// (test_results.classification) and the raw error message; the reason here is
// the deterministic composition of both — no inference, no model. An absent
// classification yields a nil reason and the explicit "classification: none"
// note so downstream agents can tell "not yet classified" apart from "empty
// reason".
func classificationReason(classification, errorMessage pgtype.Text) (*string, *string) {
	if !classification.Valid || classification.String == "" {
		note := "classification: none"
		return nil, &note
	}
	reason := "classified " + classification.String
	if errorMessage.Valid && errorMessage.String != "" {
		reason += ": " + errorMessage.String
	}
	return &reason, nil
}

// --- tool handlers ---

// listRuns implements list_runs: app-scoped, newest first, with optional
// status/verdict filters and a bounded window.
func (s *Surface) listRuns(ctx context.Context, args map[string]any) (any, error) {
	appID, err := argUUID(args, "app_id")
	if err != nil {
		return nil, err
	}
	window := argInt(args, "window", 20, 200)
	status := argString(args, "status")
	verdict := argString(args, "verdict")

	rows, err := s.Pool.Query(ctx, `
		select r.id, r.app_id, a.owner || '/' || a.repo, r.workflow_run_id, r.run_number,
		       r.head_sha, r.head_branch, r.environment, r.trigger_source, r.status, r.verdict,
		       r.pr_number, r.pr_state, r.total_tests, r.passed, r.failed, r.flaky, r.skipped,
		       r.duration_ms, r.error_note, r.started_at, r.finished_at, r.created_at
		from runs r
		join registered_apps a on a.id = r.app_id
		where r.app_id = $1
		  and ($2::text = '' or r.status = $2)
		  and ($3::text = '' or r.verdict = $3)
		order by r.created_at desc
		limit $4
	`, appID, status, verdict, window)
	if err != nil {
		return nil, fmt.Errorf("mcp: list runs: %w", err)
	}
	defer rows.Close()

	out := make([]runDTO, 0)
	for rows.Next() {
		run, err := s.scanRunRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcp: iterate runs: %w", err)
	}
	return map[string]any{"app_id": uuidString(appID), "window": window, "runs": out}, nil
}

// scanRunRow scans one joined runs row into a runDTO.
func (s *Surface) scanRunRow(row pgx.Row) (*runDTO, error) {
	var (
		r       runDTO
		appID   pgtype.UUID
		id      pgtype.UUID
		branch  pgtype.Text
		verdict pgtype.Text
		prNum   pgtype.Int4
		prState pgtype.Text
		total   pgtype.Int4
		passed  pgtype.Int4
		failed  pgtype.Int4
		flaky   pgtype.Int4
		skipped pgtype.Int4
		durMS   pgtype.Int4
		note    pgtype.Text
		started pgtype.Timestamptz
		fin     pgtype.Timestamptz
		created pgtype.Timestamptz
	)
	if err := row.Scan(&id, &appID, &r.App, &r.WorkflowRunID, &r.RunNumber,
		&r.HeadSHA, &branch, &r.Environment, &r.TriggerSource, &r.Status, &verdict,
		&prNum, &prState, &total, &passed, &failed, &flaky, &skipped,
		&durMS, &note, &started, &fin, &created); err != nil {
		return nil, fmt.Errorf("mcp: scan run: %w", err)
	}
	r.RunID = uuidString(id)
	r.AppID = uuidString(appID)
	r.HeadBranch = textPtr(branch)
	r.Verdict = textPtr(verdict)
	r.PRNumber = intPtr(prNum)
	r.PRState = textPtr(prState)
	if total.Valid {
		r.Counts = &countsDTO{
			TotalTests: int(total.Int32),
			Passed:     int(passed.Int32),
			Failed:     int(failed.Int32),
			Flaky:      int(flaky.Int32),
			Skipped:    int(skipped.Int32),
		}
	}
	r.DurationMS = intPtr(durMS)
	r.ErrorNote = textPtr(note)
	r.StartedAt = tsPtr(started)
	r.FinishedAt = tsPtr(fin)
	r.CreatedAt = tsString(created)
	r.Evidence = s.evidenceLinks(id)
	return &r, nil
}

// appLabel fetches the owner/repo label for a run's app.
func (s *Surface) appLabel(ctx context.Context, appID pgtype.UUID) (string, error) {
	var label string
	err := s.Pool.QueryRow(ctx,
		`select owner || '/' || repo from registered_apps where id = $1`, appID).Scan(&label)
	if err != nil {
		return "", fmt.Errorf("mcp: app label: %w", err)
	}
	return label, nil
}

// getRun implements get_run: runs.GetRunDetail plus the REPT-02
// classification reason on every test and token-gated artifact links.
func (s *Surface) getRun(ctx context.Context, args map[string]any) (any, error) {
	runID, err := argUUID(args, "run_id")
	if err != nil {
		return nil, err
	}
	detail, err := runs.GetRunDetail(ctx, s.Pool, runID)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return nil, toolError{msg: "mcp: run not found: " + uuidString(runID)}
	}
	label, err := s.appLabel(ctx, detail.Run.AppID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"run":        s.runPayload(detail.Run, label),
		"tests":      s.testPayloads(ctx, runID, detail.TestResults),
		"artifacts":  s.artifactPayloads(runID, detail.Artifacts),
	}, nil
}

// getFailureEvidence implements get_failure_evidence: only failed/flaky
// tests, each with error message, classification + reason, and token-gated
// attachment URLs. Optional exact-title filter narrows to one test.
func (s *Surface) getFailureEvidence(ctx context.Context, args map[string]any) (any, error) {
	runID, err := argUUID(args, "run_id")
	if err != nil {
		return nil, err
	}
	title := argString(args, "title")

	detail, err := runs.GetRunDetail(ctx, s.Pool, runID)
	if err != nil {
		return nil, err
	}
	if detail == nil {
		return nil, toolError{msg: "mcp: run not found: " + uuidString(runID)}
	}
	failures := make([]testDTO, 0)
	for _, tr := range detail.TestResults {
		if !isFailure(tr) {
			continue
		}
		if title != "" && tr.Title != title {
			continue
		}
		failures = append(failures, s.testPayload(ctx, runID, tr))
	}
	return map[string]any{
		"run_id": uuidString(runID),
		"verdict": textPtr(detail.Run.Verdict),
		"error_note": textPtr(detail.Run.ErrorNote),
		"failures": failures,
		"evidence": s.evidenceLinks(runID),
	}, nil
}

// isFailure selects the tests whose outcome means "did not pass cleanly":
// unexpected (failed) and flaky (passed only on retry).
func isFailure(tr runs.TestResult) bool {
	return tr.Outcome == "unexpected" || tr.Outcome == "flaky"
}

// getFlakeRates implements get_flake_rates over the flakes package query.
func (s *Surface) getFlakeRates(ctx context.Context, args map[string]any) (any, error) {
	appID, err := argUUID(args, "app_id")
	if err != nil {
		return nil, err
	}
	window := argInt(args, "window", trends.DefaultWindow, 200)
	rates, err := flakes.FlakeRates(ctx, s.Pool, appID, window)
	if err != nil {
		return nil, err
	}
	if rates == nil {
		rates = []flakes.Rate{}
	}
	return map[string]any{
		"app_id": uuidString(appID),
		"window": window,
		"note":   "flake rates computed from test_results over the last " + fmt.Sprint(window) + " executed runs",
		"rates":  rates,
	}, nil
}

// getQuarantine implements get_quarantine over the flakes quarantine store.
func (s *Surface) getQuarantine(ctx context.Context, args map[string]any) (any, error) {
	appID, hasApp, err := argOptionalUUID(args, "app_id")
	if err != nil {
		return nil, err
	}
	status := argString(args, "status")
	entries, err := flakes.List(ctx, s.Pool, appID, status)
	if errors.Is(err, flakes.ErrInvalidInput) {
		return nil, argError{msg: "mcp: status must be active or lifted"}
	}
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []flakes.Entry{}
	}
	if !hasApp {
		appID = pgtype.UUID{}
	}
	return map[string]any{"app_id": uuidString(appID), "status": status, "entries": entries}, nil
}

// --- payload builders ---

func (s *Surface) runPayload(r *runs.Run, label string) runDTO {
	out := runDTO{
		RunID:         uuidString(r.ID),
		AppID:         uuidString(r.AppID),
		App:           label,
		WorkflowRunID: r.WorkflowRunID,
		RunNumber:     r.RunNumber,
		HeadSHA:       r.HeadSHA,
		HeadBranch:    textPtr(r.HeadBranch),
		Environment:   r.Environment,
		TriggerSource: r.TriggerSource,
		Status:        string(r.Status),
		Verdict:       textPtr(r.Verdict),
		PRNumber:      intPtr(r.PRNumber),
		PRState:       textPtr(r.PRState),
		DurationMS:    intPtr(r.DurationMS),
		ErrorNote:     textPtr(r.ErrorNote),
		StartedAt:     tsPtr(r.StartedAt),
		FinishedAt:    tsPtr(r.FinishedAt),
		CreatedAt:     tsString(r.CreatedAt),
		Evidence:      s.evidenceLinks(r.ID),
	}
	if r.TotalTests.Valid {
		out.Counts = &countsDTO{
			TotalTests: int(r.TotalTests.Int32),
			Passed:     int(r.Passed.Int32),
			Failed:     int(r.Failed.Int32),
			Flaky:      int(r.Flaky.Int32),
			Skipped:    int(r.Skipped.Int32),
		}
	}
	return out
}

// testPayloads converts every test result; attachments get token-gated URLs.
func (s *Surface) testPayloads(ctx context.Context, runID pgtype.UUID, results []runs.TestResult) []testDTO {
	out := make([]testDTO, 0, len(results))
	for _, tr := range results {
		out = append(out, s.testPayload(ctx, runID, tr))
	}
	return out
}

func (s *Surface) testPayload(ctx context.Context, runID pgtype.UUID, tr runs.TestResult) testDTO {
	reason, note := classificationReason(tr.Classification, tr.ErrorMessage)
	return testDTO{
		ID:                   uuidString(tr.ID),
		Suite:                textPtr(tr.Suite),
		Title:                tr.Title,
		File:                 textPtr(tr.File),
		Line:                 intPtr(tr.Line),
		Project:              textPtr(tr.Project),
		Outcome:              tr.Outcome,
		Status:               tr.Status,
		Attempts:             tr.Attempts,
		DurationMS:           intPtr(tr.DurationMS),
		ErrorMessage:         textPtr(tr.ErrorMessage),
		Classification:       textPtr(tr.Classification),
		ClassificationReason: reason,
		ClassificationNote:   note,
		Attachments:          s.attachmentPayloads(runID, tr.Attachments),
	}
}

// attachmentPayloads decodes the raw attachments jsonb
// ([{kind, path}]) and stamps each with its token-gated URL.
func (s *Surface) attachmentPayloads(runID pgtype.UUID, raw []byte) []attachmentDTO {
	if len(raw) == 0 {
		return []attachmentDTO{}
	}
	var parsed []struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Malformed evidence metadata stays off the wire rather than
		// failing the whole tool call.
		return []attachmentDTO{}
	}
	out := make([]attachmentDTO, 0, len(parsed))
	for _, a := range parsed {
		out = append(out, attachmentDTO{Kind: a.Kind, Path: a.Path, URL: s.artifactURL(runID, a.Path)})
	}
	return out
}

// artifactPayloads converts the artifact index rows.
func (s *Surface) artifactPayloads(runID pgtype.UUID, artifacts []runs.Artifact) []artifactDTO {
	out := make([]artifactDTO, 0, len(artifacts))
	for _, a := range artifacts {
		one := artifactDTO{
			ID:        uuidString(a.ID),
			Kind:      a.Kind,
			Path:      a.Path,
			CreatedAt: tsString(a.CreatedAt),
			URL:       s.artifactURL(runID, a.Path),
		}
		if a.SizeBytes.Valid {
			size := a.SizeBytes.Int64
			one.SizeBytes = &size
		}
		out = append(out, one)
	}
	return out
}
