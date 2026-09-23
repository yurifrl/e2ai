// Simulate tests (06-03 T1). The signature scheme is pure unit coverage:
// cockpit's signer must match the ingest verifier's exact HMAC-SHA256
// scheme, computed independently here with the standard library so the two
// halves agree by construction, not by shared code. Everything else — the
// gate pair, the synthetic seed's transactional record, and the passthrough
// outcome — runs against a real Postgres (E2AI_TEST_DATABASE_URL harness,
// same as trigger/budget/state; self-skips without it).
package cockpit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/NSXBet/e2ai/api/internal/ingest"
	platriver "github.com/NSXBet/e2ai/api/internal/platform/river"
	"github.com/NSXBet/e2ai/api/internal/suite"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
)

// --- request-shape validation (pure, no DB) ---

func TestSimulateRequestValidation(t *testing.T) {
	cases := []struct {
		name    string
		req     SimulateRequest
		wantErr bool
	}{
		{"ok default shape", SimulateRequest{AppID: testAppUUID}, false},
		{"ok explicit workflow_run shape", SimulateRequest{AppID: testAppUUID, PayloadShape: ShapeWorkflowRun}, false},
		// Shapes are open-ended: an unrecognized flavor is an ingest
		// passthrough by design, never a validation error.
		{"ok unknown passthrough shape", SimulateRequest{AppID: testAppUUID, PayloadShape: "ping"}, false},
		{"empty app_id", SimulateRequest{}, true},
		{"non-uuid app_id", SimulateRequest{AppID: "not-a-uuid"}, true},
	}
	for _, tc := range cases {
		err := tc.req.validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

// --- signature scheme (pure, no DB) ---

// TestSimulateSignatureSymmetry pins the scheme with standard-library
// independent HMACs: the signer output must equal the canonical
// "sha256=<hex>" form, and the verifier must accept the honest delivery and
// refuse the tampered body, the foreign scheme, and an empty secret.
func TestSimulateSignatureSymmetry(t *testing.T) {
	const secret = "whsec_e2ai_simulate"
	body := []byte(`{"action":"requested","workflow_run":{"id":42}}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := signWebhookPayload(secret, body); got != want {
		t.Fatalf("signWebhookPayload = %q, want %q", got, want)
	}

	if !verifyWebhookSignature(secret, body, want) {
		t.Error("honest delivery failed verification")
	}
	if verifyWebhookSignature(secret, append(body, ' '), want) {
		t.Error("tampered body passed verification")
	}
	if verifyWebhookSignature(secret, body, "md5=0000") {
		t.Error("foreign scheme passed verification")
	}
	if verifyWebhookSignature("", body, want) {
		t.Error("empty secret passed verification")
	}
}

// --- gate pair + ingest passthrough (real Postgres) ---

// noopWaitRolloutWorker satisfies River's registration requirement for the
// kind Store.Ingest enqueues; the simulate path only inserts the job, never
// runs it (same discipline as noopExecuteWorker below).
type noopWaitRolloutWorker struct {
	river.WorkerDefaults[ingest.WaitRolloutArgs]
}

func (noopWaitRolloutWorker) Work(context.Context, *river.Job[ingest.WaitRolloutArgs]) error {
	return nil
}

// simulateService wires the full NewService with a fixed-secret resolver:
// Simulate consumes the resolver's rendered value, so tests never exercise
// vault rendering itself (main.go's boot job, SECR-01).
func simulateService(t *testing.T) *Service {
	t.Helper()
	pool := testPool(t)
	// The ingest path enqueues onto river_job, so the River schema must
	// exist (MigrateRiver is idempotent, safe across files sharing the
	// scratch database) — same setup convention as ingest's gateWorker.
	if err := app.MigrateRiver(context.Background(), pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}
	workers := river.NewWorkers()
	river.AddWorker[suite.ExecuteArgs](workers, noopExecuteWorker{})
	river.AddWorker[ingest.WaitRolloutArgs](workers, noopWaitRolloutWorker{})
	client, err := platriver.RunnerClient(pool, 1, platriver.WithWorkers(workers))
	if err != nil {
		t.Fatalf("river test client: %v", err)
	}
	return NewService(pool, "", nil, client, nil, ingest.NewStore(pool), func(context.Context, app.App) (string, error) {
		return "whsec_e2ai_simulate", nil
	})
}

func TestSimulateUnknownAppRefuses(t *testing.T) {
	s := simulateService(t)
	_, _, _, err := s.Simulate(context.Background(), SimulateRequest{
		AppID:        uuid.NewString(),
		PayloadShape: ShapeWorkflowRun,
	})
	if !errors.Is(err, ErrUnknownApp) {
		t.Fatalf("unknown app must refuse with ErrUnknownApp, got %v", err)
	}
}

func TestSimulateSecretUnavailableRefuses(t *testing.T) {
	s := simulateService(t)
	// Fail-closed rendering: a resolver error is ErrSecretUnavailable and
	// nothing is recorded — a simulate that cannot be signed is never
	// stored unsigned.
	s.Secrets = func(context.Context, app.App) (string, error) {
		return "", errors.New("vault down")
	}
	appID := budgetApp(t, s.Pool, nil)
	ctx := context.Background()
	_, _, _, err := s.Simulate(ctx, SimulateRequest{
		AppID:        uuid.UUID(appID.Bytes).String(),
		PayloadShape: ShapeWorkflowRun,
		actor:        "qa@example.com",
	})
	if !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("failed secret render must refuse with ErrSecretUnavailable, got %v", err)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `select count(*) from runs where app_id = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("unsigned simulate recorded %d run rows, want 0", n)
	}
}

func TestSimulatePausedRefuses(t *testing.T) {
	s := simulateService(t)
	appID := budgetApp(t, s.Pool, nil)
	ctx := context.Background()
	if err := SetPaused(ctx, s.Pool, "op@example.com", true, "release freeze"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, _, _, err := s.Simulate(ctx, SimulateRequest{
		AppID:        uuid.UUID(appID.Bytes).String(),
		PayloadShape: ShapeWorkflowRun,
		actor:        "qa@example.com",
	})
	if !errors.Is(err, ErrPaused) || !strings.Contains(err.Error(), "release freeze") {
		t.Fatalf("paused simulate must carry the pause reason, got %v", err)
	}
}

func TestSimulateZeroCeilingRefuses(t *testing.T) {
	s := simulateService(t)
	zero := int64(0)
	appID := budgetApp(t, s.Pool, &zero)
	_, _, _, err := s.Simulate(context.Background(), SimulateRequest{
		AppID:        uuid.UUID(appID.Bytes).String(),
		PayloadShape: ShapeWorkflowRun,
	})
	if !errors.Is(err, ErrZeroCeiling) {
		t.Fatalf("zero ceiling must refuse with ErrZeroCeiling, got %v", err)
	}
}

func TestSimulateEnactsWorkflowRun(t *testing.T) {
	s := simulateService(t)
	ctx := context.Background()
	appID := budgetApp(t, s.Pool, nil)
	actor := "qa@example.com"

	result, runID, wfID, err := s.Simulate(ctx, SimulateRequest{
		AppID:        uuid.UUID(appID.Bytes).String(),
		PayloadShape: ShapeWorkflowRun,
		Reason:       "one-click smoke",
		actor:        actor,
	})
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if result != ingest.ResultAccepted || runID == uuid.Nil || wfID >= 0 {
		t.Fatalf("simulate outcome = (result=%d, runID=%q, wfID=%d), want accepted with a run id and a negative synthetic workflow id",
			result, runID, wfID)
	}

	// The run row is simulate-sourced: born from the real ingest insert,
	// never attributed to a person, run_number 0 (synthetic policy D2).
	var source, status string
	var triggeredBy pgtype.Text
	var wfrID int64
	if err := s.Pool.QueryRow(ctx,
		`select trigger_source, triggered_by, workflow_run_id, status from runs where id = $1`,
		pgUUID(runID)).Scan(&source, &triggeredBy, &wfrID, &status); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if source != "simulate" || triggeredBy.Valid || wfrID != wfID {
		t.Fatalf("run row = (source=%s, triggered_by=%v, wfrID=%d), want (simulate, NULL, %d)",
			source, triggeredBy, wfrID, wfID)
	}

	// The signed delivery was recorded with the same row shape a real
	// delivery gets: verified signature, simulate-namespaced delivery id.
	var deliveryID string
	var sigValid bool
	if err := s.Pool.QueryRow(ctx,
		`select delivery_id, signature_valid from webhook_events where workflow_run_id = $1`, wfID).
		Scan(&deliveryID, &sigValid); err != nil {
		t.Fatalf("read webhook_events row: %v", err)
	}
	if !sigValid || !strings.HasPrefix(deliveryID, "simulate:"+actor+":") {
		t.Fatalf("delivery row = (%q, %v), want verified simulate-namespaced delivery", deliveryID, sigValid)
	}

	// The wait-rollout handoff was enqueued inside the same commit — the
	// pipeline entry a real webhook would produce.
	var jobs int
	if err := s.Pool.QueryRow(ctx,
		`select count(*) from river_job where kind = $1`, ingest.KindWaitRollout).Scan(&jobs); err != nil {
		t.Fatalf("count river jobs: %v", err)
	}
	if jobs != 1 {
		t.Fatalf("river_job wait-rollout rows = %d, want 1", jobs)
	}

	// The action is audited against the created run.
	var audits int
	if err := s.Pool.QueryRow(ctx,
		`select count(*) from audit_events where action = $1 and target_type = 'run' and target_id = $2 and actor_email = $3`,
		ActionSimulate, runID, actor).Scan(&audits); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if audits != 1 {
		t.Fatalf("audit rows for simulate on run %s = %d, want 1", runID, audits)
	}
}

func TestSimulateUnknownShapeRecordsPassthrough(t *testing.T) {
	s := simulateService(t)
	ctx := context.Background()
	appID := budgetApp(t, s.Pool, nil)

	result, runID, wfID, err := s.Simulate(ctx, SimulateRequest{
		AppID:        uuid.UUID(appID.Bytes).String(),
		PayloadShape: "ping",
		actor:        "qa@example.com",
	})
	if err != nil {
		t.Fatalf("passthrough simulate: %v", err)
	}
	if result != ingest.ResultRecorded || runID != uuid.Nil || wfID >= 0 {
		t.Fatalf("passthrough outcome = (result=%d, runID=%q, wfID=%d), want recorded with no run row and a negative synthetic id",
			result, runID, wfID)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `select count(*) from runs where app_id = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("passthrough created %d run rows, want 0", n)
	}
}
