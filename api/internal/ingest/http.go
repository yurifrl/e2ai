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
	secret     string            // shared fallback (unresolved app refs, legacy wiring)
	appSecrets map[string]string // per-app rendered webhook secret, keyed by app identity (APPS-03)
	matchers   matchers
	store      IngestStore
	queue      JobEnqueuer
	triage     TriageStore // optional; nil disables unknown-delivery recording
	logger     *slog.Logger
}

// appSecretKey identifies an app's secret row: the registry upsert keys on
// (owner, repo, workflow_name, environment), so that tuple names the app
// uniquely across re-seeding.
func appSecretKey(a app.App) string {
	return a.Owner + "/" + a.Repo + "/" + a.WorkflowName + "/" + a.Environment
}

// IngestStore is the transactional ingest path (dedupe.go). The indirection
// keeps handler plumbing testable without a database.
type IngestStore interface {
	Ingest(ctx context.Context, seed RunSeed, queue JobEnqueuer) (Result, error)
}

// WithTriage attaches the unknown-delivery store (EDGE-01): signature-valid
// deliveries no app claims are recorded for admin triage instead of being
// silently ignored. Wire before serving; not safe to mutate concurrently
// with requests.
func (h *Handler) WithTriage(store TriageStore) *Handler {
	h.triage = store
	return h
}

// SecretResolver renders an op:// webhook_secret_ref to plaintext (SECR-01:
// the vault CLI renders secrets; Go only carries the rendered result).
type SecretResolver func(ctx context.Context, ref string) (string, error)

// NewHandler wires the webhook. secret is the rendered shared WEBHOOK_SECRET
// (fallback for apps whose webhook_secret_ref is missing or unresolved);
// per-app secrets attach later via WithSecretResolver (APPS-03: an op://
// ref resolved at boot — `op inject`/`op read` render secrets, never Go).
// No secret anywhere fails closed — every delivery gets a 503 rather than
// an unverifiable pass (SECR-01).
func NewHandler(secret string, apps []app.App, store IngestStore, queue JobEnqueuer, logger *slog.Logger) *Handler {
	return &Handler{
		secret:     secret,
		appSecrets: map[string]string{},
		matchers:   compileMatchers(apps, logger),
		store:      store,
		queue:      queue,
		logger:     logger,
	}
}

// WithSecretResolver resolves every registered app's webhook_secret_ref at
// wiring time (boot) into the per-app secret map. Resolution failures
// degrade THAT app to the shared secret with a warning — never a panic or a
// boot failure: an app with an unreachable vault entry must not take the
// whole ingest surface down.
func (h *Handler) WithSecretResolver(ctx context.Context, resolver SecretResolver, apps []app.App) *Handler {
	for _, a := range apps {
		if a.WebhookSecretRef == "" || resolver == nil {
			continue
		}
		secret, err := resolver(ctx, a.WebhookSecretRef)
		if err != nil || secret == "" {
			h.logger.Warn("webhook secret resolution failed; falling back to shared secret",
				"app", appSecretKey(a), "ref", a.WebhookSecretRef, "error", err)
			continue
		}
		h.appSecrets[appSecretKey(a)] = secret
	}
	return h
}

// verifyAppSignature checks the signature against the candidate secrets of
// ONE matched app: its per-app secret first, the shared secret as fallback
// when the per-app secret is absent. Per-app isolation means another app's
// secret never accepts this app's delivery. Returns the matched app.
func (h *Handler) verifyAppSignature(payload webhookPayload, body []byte, header string) (matcher, bool) {
	m, ok := h.matchers.match(payload.Workflow.Name, payload.WorkflowRun.DisplayTitle)
	if !ok {
		return matcher{}, false
	}
	candidates := []string{}
	if perApp := h.appSecrets[appSecretKey(m.app)]; perApp != "" {
		candidates = append(candidates, perApp)
	}
	candidates = append(candidates, h.secret)
	for _, secret := range candidates {
		if verifySignature(secret, body, header) {
			return m, true
		}
	}
	return matcher{}, false
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
	if h.store == nil || !h.secretsConfigured() {
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
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" {
		httpError(w, http.StatusBadRequest, "missing X-GitHub-Delivery")
		return
	}
	if r.Header.Get("X-GitHub-Event") != acceptedEvent {
		respondJSON(w, http.StatusAccepted, "ignored")
		return
	}
	// Per-app secrets (APPS-03) force match-before-verify: the payload names
	// the app whose secret applies. Parsing untrusted JSON is safe; the HMAC
	// below is what gates trust.
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		httpError(w, http.StatusBadRequest, "malformed payload")
		return
	}
	m, ok := h.verifyAppSignature(payload, body, r.Header.Get("X-Hub-Signature-256"))
	if !ok {
		// Signature-valid but app-unknown deliveries are the EDGE-01 triage
		// queue's input; everything else fails closed with 403. Without a
		// wired triage store the legacy "ignored" answer stands — the
		// delivery was never actionable either way.
		if h.verifiedAgainstAny(body, r.Header.Get("X-Hub-Signature-256")) {
			h.recordTriage(delivery, payload, body)
			respondJSON(w, http.StatusAccepted, "triaged")
		} else {
			httpError(w, http.StatusForbidden, "signature mismatch")
		}
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

// secretsConfigured reports whether any verify-capable secret exists. With
// none, every delivery would 403 forever — a 503 for missing boot config is
// the honest signal (SECR-01 fail closed).
func (h *Handler) secretsConfigured() bool {
	if h.secret != "" {
		return true
	}
	for _, secret := range h.appSecrets {
		if secret != "" {
			return true
		}
	}
	return false
}

// verifiedAgainstAny reports whether the signature verifies against ANY
// configured secret. Only used for app-unknown deliveries, where per-app
// isolation cannot apply because no app is named by the registry match.
func (h *Handler) verifiedAgainstAny(body []byte, header string) bool {
	for _, secret := range h.appSecrets {
		if secret != "" && verifySignature(secret, body, header) {
			return true
		}
	}
	return verifySignature(h.secret, body, header)
}

// recordTriage files an unknown-delivery row for admin triage (EDGE-01).
// Triage failures never fail the request — the delivery is still 202'd so
// GitHub does not retry-storm an acknowledged event; the log carries the
// loss instead.
func (h *Handler) recordTriage(delivery string, payload webhookPayload, body []byte) {
	if h.triage == nil {
		h.logger.Debug("webhook delivery ignored: no matching app",
			"workflow", payload.Workflow.Name, "display_title", payload.WorkflowRun.DisplayTitle)
		return
	}
	err := h.triage.RecordUnknownDelivery(context.Background(), UnknownDelivery{
		DeliveryID:    delivery,
		WorkflowName:  payload.Workflow.Name,
		DisplayTitle:  payload.WorkflowRun.DisplayTitle,
		WorkflowRunID: payload.WorkflowRun.ID,
		Payload:       body,
	})
	if err != nil {
		h.logger.Error("webhook triage record failed", "error", err, "delivery", delivery)
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
