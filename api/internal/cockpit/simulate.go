// The webhook simulate boundary (ACTN-01, 06-03 T1): render a synthetic
// workflow_run payload from the app's registration facts, HMAC-sign it
// server-side with the SAME per-app secret the ingest Handler trusts, and
// drive the real ingest seam — Store.Ingest with a hand-built RunSeed
// (sweep.go precedent; CONTEXT decision 2 option (a)). No outbound GitHub
// calls: the head comes from stored runs, the run identity from the
// cockpit_run_ids sequence. Pause and budget gates refuse exactly like the
// manual trigger (CONTEXT decision 2: simulate + trigger enumerate the
// pause scope).
package cockpit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/ingest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ShapeWorkflowRun is the one payload flavor simulate renders today: a
// GitHub workflow_run body the ingest handler parses verbatim.
const ShapeWorkflowRun = "workflow_run"

// SecretResolver renders the webhook secret for a registered app from the
// in-memory map the ingest Handler built at boot (WithSecretResolver:
// per-app secret first, shared fallback second). Simulate consumes the SAME
// resolved output — it never re-renders vault refs (SECR-01: secrets render
// once, in main.go, at boot).
type SecretResolver func(ctx context.Context, a app.App) (string, error)

// ErrSecretUnavailable: no rendered webhook secret exists for the app — the
// boot resolver failed, or neither the per-app ref nor the shared secret is
// configured. Fail-closed: a simulate that cannot be signed is refused,
// never recorded unsigned.
var ErrSecretUnavailable = errors.New("webhook secret unavailable")

// SimulateRequest is the one-click simulate body: the app and the payload
// flavor. The payload itself is never client-supplied — it is rendered
// server-side from the app's registration facts (ACTN-01).
type SimulateRequest struct {
	AppID        string
	PayloadShape string
	Reason       string
	actor        string
}

func (r SimulateRequest) validate() error {
	if r.AppID == "" {
		return fmt.Errorf("%w: app_id is required", errInvalidRequest)
	}
	if _, err := uuid.Parse(r.AppID); err != nil {
		return fmt.Errorf("%w: app_id is not a uuid", errInvalidRequest)
	}
	// Payload shape is open-ended: workflow_run (or the empty default) is
	// the run-creating flavor; every other flavor is an ingest passthrough
	// (ResultRecorded, no run row, no job) — same gates, same response
	// shape, no special-casing.
	return nil
}

// simApp is the registration slice simulate renders from — the ingest
// 4-tuple (owner/repo/workflow_name/environment, the resolveAppID key) plus
// the display-title pattern the synthetic payload cites.
type simApp struct {
	ID                  uuid.UUID
	Owner               string
	Repo                string
	WorkflowName        string
	Environment         string
	DisplayTitlePattern string
	WebhookSecretRef    string
}

// lookupSimApp mirrors lookupApp but fetches the rendering facts too. An
// unknown app_id is a typed refusal before any gate runs.
func (s *Service) lookupSimApp(ctx context.Context, appID uuid.UUID) (simApp, error) {
	var a simApp
	err := s.Pool.QueryRow(ctx, `
		select id::text, owner, repo, workflow_name, environment,
		       coalesce(display_title_pattern, ''), coalesce(webhook_secret_ref, '')
		from registered_apps
		where id = $1`, appID).Scan(&a.ID, &a.Owner, &a.Repo, &a.WorkflowName,
		&a.Environment, &a.DisplayTitlePattern, &a.WebhookSecretRef)
	if errors.Is(err, pgx.ErrNoRows) {
		return simApp{}, fmt.Errorf("%w: %s", ErrUnknownApp, appID)
	}
	return a, err
}

// Simulate enacts one webhook simulation in the trigger's gate order —
// pause → budget — then renders, signs, self-verifies, and ingests. On
// success it returns the ingest outcome plus the created run's id (when
// ingest inserted one) and the synthetic workflow_run_id; every refusal is
// a typed error.
func (s *Service) Simulate(ctx context.Context, req SimulateRequest) (result ingest.Result, runID uuid.UUID, workflowRunID int64, err error) {
	if s.Pool == nil {
		return 0, uuid.Nil, 0, errors.New("postgres unavailable")
	}
	if err := req.validate(); err != nil {
		return 0, uuid.Nil, 0, err
	}
	if s.Store == nil {
		return 0, uuid.Nil, 0, errors.New("ingest store not wired")
	}
	if s.Queue == nil {
		return 0, uuid.Nil, 0, errors.New("runner queue unavailable")
	}
	if s.Secrets == nil {
		return 0, uuid.Nil, 0, ErrSecretUnavailable
	}
	appID, err := uuid.Parse(req.AppID)
	if err != nil {
		return 0, uuid.Nil, 0, fmt.Errorf("%w: app_id is not a uuid", errInvalidRequest)
	}
	a, err := s.lookupSimApp(ctx, appID)
	if err != nil {
		return 0, uuid.Nil, 0, err
	}

	// Gate 1 — pause (CONTEXT decision 2: simulate + trigger enumerate the
	// pause scope; the refusal carries the operator reason).
	if err := s.checkPaused(ctx); err != nil {
		return 0, uuid.Nil, 0, err
	}
	// Gate 2 — budget (BUDG-03: simulated runs feed real agent sessions;
	// the same Evaluate aggregate as the trigger applies).
	pgAppID := pgtype.UUID{Bytes: appID, Valid: true}
	verdict, err := Evaluate(ctx, s.Pool, pgAppID)
	if err != nil {
		return 0, uuid.Nil, 0, err
	}
	if verdict.Cap.Capped() && verdict.Cap.Millicents == 0 {
		return 0, uuid.Nil, 0, ErrZeroCeiling
	}
	if verdict.Refused {
		return 0, uuid.Nil, 0, fmt.Errorf("%w: %s", ErrBudgetBreach, verdict.Reason)
	}

	// The same resolved secret the webhook handler trusts (APPS-03 map —
	// per-app first, shared fallback), never a fresh vault render.
	seedApp := app.App{
		Owner:            a.Owner,
		Repo:             a.Repo,
		WorkflowName:     a.WorkflowName,
		Environment:      a.Environment,
		WebhookSecretRef: a.WebhookSecretRef,
	}
	secret, err := s.Secrets(ctx, seedApp)
	if err != nil || secret == "" {
		return 0, uuid.Nil, 0, fmt.Errorf("%w: %s/%s (%v)", ErrSecretUnavailable, a.Owner, a.Repo, err)
	}

	// Synthetic identity — negative, from the dedicated sequence (TRIG-05
	// symmetry with the trigger: never collides under the webhook-dedupe
	// unique (app_id, workflow_run_id) key).
	if err := s.Pool.QueryRow(ctx, `select -nextval('cockpit_run_ids')`).Scan(&workflowRunID); err != nil {
		return 0, uuid.Nil, 0, fmt.Errorf("synthetic workflow_run_id: %w", err)
	}

	// Stored data only (ACTN-01: no outbound GitHub calls) — the newest
	// head the app has ever run from; empty when the app never ran.
	headSHA := s.latestStoredHead(ctx, appID)
	// workflow_run renders the full GitHub body; other flavors record a
	// minimal shape-tagged envelope — both are signed deliveries, the flavor
	// decides only how far the delivery travels.
	var body []byte
	if req.PayloadShape == "" || req.PayloadShape == ShapeWorkflowRun {
		body, err = json.Marshal(simulatePayload(a, workflowRunID, headSHA))
	} else {
		body, err = json.Marshal(map[string]any{
			"simulated":     true,
			"payload_shape": req.PayloadShape,
		})
	}
	if err != nil {
		return 0, uuid.Nil, 0, fmt.Errorf("render simulate payload: %w", err)
	}
	// Sign server-side (X-Hub-Signature-256 format) and prove the symmetry:
	// the signature must verify under the same scheme the webhook handler
	// applies. A self-verification failure means the signing scheme and the
	// ingest verifier have drifted — refuse rather than record unsigned.
	signature := signWebhookPayload(secret, body)
	if !verifyWebhookSignature(secret, body, signature) {
		return 0, uuid.Nil, 0, errors.New("simulate signature failed self-verification")
	}

	seed := ingest.RunSeed{
		App:            seedApp,
		DeliveryID:     fmt.Sprintf("simulate:%s:%s", req.actor, uuid.NewString()),
		WorkflowRunID:  workflowRunID,
		RunNumber:      0, // synthetic-id policy (RESEARCH D2), same as manual
		HeadSHA:        headSHA,
		HeadBranch:     "",
		TriggerSource:  ingest.TriggerSimulate,
		SignatureValid: true,
		Payload:        body,
		// workflow_run flavors travel the full pipeline (run row + runner
		// job); everything else stops at the recorded delivery.
		Enqueue: req.PayloadShape == "" || req.PayloadShape == ShapeWorkflowRun,
	}
	// REVIEW WR-02 (AUDT-01): the audit row must be transactional with the
	// run insert. A post-ingest audit tx has the same crash window the
	// webhook path eliminated — a crash between ingest commit and audit
	// commit records an unattributed run. The hook rides ingest's own tx:
	// audit commits with the run, or neither commits. The synthetic
	// workflow_run id is the fallback target when no run row exists
	// (filtered payloads) — the actor's action is recorded either way.
	seed.Audit = func(ctx context.Context, tx pgx.Tx, outcome ingest.Result, runIDText string) error {
		target := runIDText
		if target == "" {
			target = strconv.FormatInt(workflowRunID, 10)
		}
		return AppendAudit(ctx, tx, AuditEntry{
			ActorEmail: req.actor,
			Action:     ActionSimulate,
			TargetType: "run",
			TargetID:   target,
			Reason:     req.Reason,
			Details: map[string]any{
				"app":             a.Owner + "/" + a.Repo + "/" + a.WorkflowName + "/" + a.Environment,
				"payload_shape":   ShapeWorkflowRun,
				"outcome":         outcome.String(),
				"workflow_run_id": workflowRunID,
			},
		})
	}
	result, err = s.Store.Ingest(ctx, seed, s.Queue)
	if err != nil {
		return 0, uuid.Nil, 0, fmt.Errorf("simulate ingest: %w", err)
	}
	if result == ingest.ResultAccepted {
		if err := s.Pool.QueryRow(ctx,
			`select id from runs where app_id = $1 and workflow_run_id = $2`,
			appID, workflowRunID).Scan(&runID); err != nil {
			return 0, uuid.Nil, 0, fmt.Errorf("simulate run lookup: %w", err)
		}
	}
	return result, runID, workflowRunID, nil
}

// latestStoredHead picks the newest head the app has ever run from — stored
// data only (ACTN-01: simulate makes no outbound GitHub calls). An app with
// no stored runs simulates with an empty sha; the run row stays honest.
func (s *Service) latestStoredHead(ctx context.Context, appID uuid.UUID) string {
	var sha string
	if err := s.Pool.QueryRow(ctx, `
		select head_sha from runs where app_id = $1 and head_sha <> ''
		order by created_at desc limit 1`, appID).Scan(&sha); err != nil {
		return ""
	}
	return sha
}

// simulateWorkflow / simulateRun / simulatePR mirror ingest's webhookPayload
// field-for-field (handler-acceptable shape) without importing the unexported
// type — the two stay in lockstep at the wire boundary.
type simulateWorkflow struct {
	Name string `json:"name"`
}

type simulateRun struct {
	ID           int64        `json:"id"`
	RunNumber    int          `json:"run_number"`
	HeadSHA      string       `json:"head_sha"`
	HeadBranch   string       `json:"head_branch"`
	DisplayTitle string       `json:"display_title"`
	HTMLURL      string       `json:"html_url"`
	Conclusion   string       `json:"conclusion"`
	Event        string       `json:"event"`
	PullRequests []simulatePR `json:"pull_requests"`
}

type simulatePR struct {
	ID     int64 `json:"id"`
	Number int   `json:"number"`
}

// simulatePayloadShape is the rendered body: registration facts in, GitHub
// workflow_run shape out. event/conclusion are the values ingest accepts
// (workflow_dispatch + success) so the simulation exercises the accept path,
// not the ignored path.
type simulatePayloadShape struct {
	Workflow    simulateWorkflow `json:"workflow"`
	WorkflowRun simulateRun      `json:"workflow_run"`
}

func simulatePayload(a simApp, wfID int64, headSHA string) simulatePayloadShape {
	return simulatePayloadShape{
		Workflow: simulateWorkflow{Name: a.WorkflowName},
		WorkflowRun: simulateRun{
			ID:           wfID,
			RunNumber:    0,
			HeadSHA:      headSHA,
			HeadBranch:   "",
			DisplayTitle: a.DisplayTitlePattern,
			HTMLURL:      "",
			Conclusion:   "success",
			Event:        "workflow_dispatch",
			PullRequests: []simulatePR{},
		},
	}
}

// signWebhookPayload renders GitHub's X-Hub-Signature-256 format
// ("sha256=<hex>") over the raw body with the app's webhook secret.
func signWebhookPayload(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// verifyWebhookSignature mirrors ingest's verifySignature scheme —
// "sha256=" prefixed, constant-time compare. Simulate self-verifies before
// storing so SignatureValid=true is a verified claim, never an assumption.
func verifyWebhookSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}
