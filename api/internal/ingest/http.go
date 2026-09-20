// Package ingest is the pipeline head (INGE-01/02): the GitHub webhook HTTP
// handler and the reconciliation sweep. Both funnel into the same
// transactional path in dedupe.go — the delivery record, the run row, and the
// wait-rollout job enqueue commit atomically, so redeliveries and sweep
// replays are idempotent by construction.
package ingest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

const (
	// KindWaitRollout is the first pipeline job kind (worked by this package's
	// rollout gate, Wave 3). Enqueued transactionally with the run row.
	KindWaitRollout = "e2ai:run:wait-rollout"

	// maxWebhookBody caps the buffered body at 5 MiB. workflow_run payloads
	// are orders of magnitude smaller; anything bigger is abuse.
	maxWebhookBody = 5 << 20

	// acceptedEvent is the only GitHub event the pipeline consumes.
	acceptedEvent = "workflow_run"
)

// WaitRolloutArgs is the wait-rollout job's payload: the run whose app's
// rollout health the worker polls (default queue).
type WaitRolloutArgs struct {
	RunID uuid.UUID `json:"run_id"`
}

// Kind implements river.JobArgs.
func (WaitRolloutArgs) Kind() string { return KindWaitRollout }

// waitRolloutOpts keeps the gate one-shot (MaxAttempts 1): it polls health_url
// to its own deadline and reports INFRA_ERROR on timeout (INGE-03) — River
// retries would only delay the verdict. UniqueOpts keeps a racing second
// enqueue of the same run (webhook vs sweep) from doubling the job.
func waitRolloutOpts() *river.InsertOpts {
	return &river.InsertOpts{
		MaxAttempts: 1,
		UniqueOpts: river.UniqueOpts{ByState: []rivertype.JobState{
			rivertype.JobStateScheduled,
			rivertype.JobStatePending,
			rivertype.JobStateAvailable,
			rivertype.JobStateRunning,
		}},
	}
}

// Handler serves POST /github/webhook/ (the admin-bff forwards GitHub's
// delivery): verify signature → match a registered app → filter → ingest.
type Handler struct {
	secret   string
	matchers matchers
	store    IngestStore
	queue    JobEnqueuer
	logger   *slog.Logger
}

// IngestStore is the transactional ingest path (dedupe.go). The indirection
// keeps handler plumbing testable without a database.
type IngestStore interface {
	Ingest(ctx context.Context, seed RunSeed, queue JobEnqueuer) (Result, error)
}

// NewHandler wires the webhook. secret is the rendered WEBHOOK_SECRET; an
// empty secret fails closed — every delivery gets a 503 rather than an
// unverifiable pass (SECR-01).
func NewHandler(secret string, apps []app.App, store IngestStore, queue JobEnqueuer, logger *slog.Logger) *Handler {
	return &Handler{
		secret:   secret,
		matchers: compileMatchers(apps, logger),
		store:    store,
		queue:    queue,
		logger:   logger,
	}
}

// ServeHTTP implements the GitHub webhook contract: raw-body HMAC-SHA256
// (X-Hub-Signature-256), delivery GUID dedupe, app matching, and the
// dispatch/success filter before anything touches the database.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpError(w, http.StatusMethodNotAllowed, "webhook is POST-only")
		return
	}
	if h.secret == "" || h.store == nil {
		httpError(w, http.StatusServiceUnavailable, "ingest unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		httpError(w, http.StatusBadRequest, "read body failed")
		return
	}
	if len(body) > maxWebhookBody {
		httpError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	if !verifySignature(h.secret, body, r.Header.Get("X-Hub-Signature-256")) {
		httpError(w, http.StatusForbidden, "signature mismatch")
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" {
		httpError(w, http.StatusBadRequest, "missing X-GitHub-Delivery")
		return
	}
	if r.Header.Get("X-GitHub-Event") != acceptedEvent {
		respondJSON(w, http.StatusAccepted, "ignored")
		return
	}
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		httpError(w, http.StatusBadRequest, "malformed payload")
		return
	}
	m, ok := h.matchers.match(payload.Workflow.Name, payload.WorkflowRun.DisplayTitle)
	if !ok {
		h.logger.Debug("webhook delivery ignored: no matching app",
			"workflow", payload.Workflow.Name, "display_title", payload.WorkflowRun.DisplayTitle)
		respondJSON(w, http.StatusAccepted, "ignored")
		return
	}
	seed := seedFromWebhook(m.app, delivery, body, payload)
	result, err := h.store.Ingest(r.Context(), seed, h.queue)
	if err != nil {
		h.logger.Error("webhook ingest failed", "error", err, "delivery", delivery,
			"workflow_run_id", payload.WorkflowRun.ID)
		httpError(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	switch result {
	case ResultAccepted:
		respondJSON(w, http.StatusOK, "accepted")
	default: // ResultDuplicateEvent, ResultDuplicateRun, ResultRecorded
		respondJSON(w, http.StatusAccepted, result.String())
	}
}

// verifySignature validates GitHub's X-Hub-Signature-256 ("sha256=<hex>")
// against the raw body with a constant-time compare. An empty secret or
// header always rejects.
func verifySignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	const prefix = "sha256="
	digest, ok := strings.CutPrefix(header, prefix)
	if !ok {
		return false
	}
	got, err := hex.DecodeString(digest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// webhookPayload is the narrow slice of a workflow_run delivery the ingest
// decision needs. The raw body is stored verbatim in webhook_events for the
// evidence trail.
type webhookPayload struct {
	Workflow struct {
		Name string `json:"name"`
	} `json:"workflow"`
	WorkflowRun struct {
		ID           int64  `json:"id"`
		RunNumber    int    `json:"run_number"`
		HeadSHA      string `json:"head_sha"`
		HeadBranch   string `json:"head_branch"`
		DisplayTitle string `json:"display_title"`
		HTMLURL      string `json:"html_url"`
		Conclusion   string `json:"conclusion"`
		Event        string `json:"event"`
		PullRequests []struct {
			ID     int64 `json:"id"`
			Number int   `json:"number"`
		} `json:"pull_requests"`
	} `json:"workflow_run"`
}

// accepts filters for the deploy signal: a completed, successful
// workflow_dispatch of a registered workflow (INTG-01/02). conclusion is
// empty while a run still executes — those deliveries are recorded but never
// become runs.
func accepts(p webhookPayload) bool {
	return p.WorkflowRun.Event == "workflow_dispatch" && p.WorkflowRun.Conclusion == "success"
}

// seedFromWebhook folds a verified delivery into the ingest seed; Enqueue
// follows the filter so filtered payloads are recorded without a run row.
func seedFromWebhook(a app.App, delivery string, raw []byte, payload webhookPayload) RunSeed {
	return RunSeed{
		App:            a,
		DeliveryID:     delivery,
		WorkflowRunID:  payload.WorkflowRun.ID,
		RunNumber:      payload.WorkflowRun.RunNumber,
		HeadSHA:        payload.WorkflowRun.HeadSHA,
		HeadBranch:     payload.WorkflowRun.HeadBranch,
		PRNumber:       webhookPR(payload),
		TriggerSource:  TriggerWebhook,
		SignatureValid: true,
		Payload:        raw,
		Enqueue:        accepts(payload),
	}
}

// webhookPR extracts the first PR number attached to a workflow_run
// delivery (0 when the payload carries none — the derive job's own PR
// resolution by head SHA is the fallback).
func webhookPR(p webhookPayload) int {
	for _, pr := range p.WorkflowRun.PullRequests {
		if pr.Number > 0 {
			return pr.Number
		}
	}
	return 0
}

// matcher pairs a registered app with its compiled display_title filter (the
// server-side re-validation of the webhook filter contract, APPS-01).
type matcher struct {
	app app.App
	re  *regexp.Regexp
}

// matchers is the compiled app registry for one handler.
type matchers []matcher

// compileMatchers compiles every app's pattern. An invalid pattern skips that
// app loudly (config-as-code bug) without taking ingest down for the rest.
func compileMatchers(apps []app.App, logger *slog.Logger) matchers {
	ms := make(matchers, 0, len(apps))
	for _, a := range apps {
		re, err := regexp.Compile(a.DisplayTitlePattern)
		if err != nil {
			if logger != nil {
				logger.Error("invalid display_title_pattern; skipping app",
					"app", a.WorkflowName, "environment", a.Environment, "error", err)
			}
			continue
		}
		ms = append(ms, matcher{app: a, re: re})
	}
	return ms
}

// match selects the app whose workflow name and display_title filter both
// accept the delivery. First match wins; the registry's
// (owner, repo, workflow_name, environment) uniqueness keeps overlaps
// intentional.
func (ms matchers) match(workflowName, displayTitle string) (matcher, bool) {
	for _, m := range ms {
		if m.app.WorkflowName == workflowName && m.re.MatchString(displayTitle) {
			return m, true
		}
	}
	return matcher{}, false
}

// respondJSON writes {"status": <s>} with the given code.
func respondJSON(w http.ResponseWriter, code int, status string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"status":%q}`, status)
}

// httpError writes a plain-text error with the given code.
func httpError(w http.ResponseWriter, code int, message string) {
	http.Error(w, message, code)
}
