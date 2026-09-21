// Repair-proposal store tests (FLAK-02): same scratch-Postgres pattern as
// store_test.go, so the check constraints, the refuse-to-heal floor, and the
// one-shot decision semantics are exercised against real DDL. The centerpiece
// is the report-only invariant: a proposal on its own NEVER touches
// corpus_behaviors — only an attributed human approval does, and only through
// the same promote transaction the manual flow uses.
package corpus

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testLearning appends one learning fixture for app and returns its id.
func testLearning(t *testing.T, pool *pgxpool.Pool, appID, runID pgtype.UUID) pgtype.UUID {
	t.Helper()
	l, err := AppendLearning(context.Background(), pool, appID, runID,
		"locator failed: button.live-bet not found on live odds page")
	if err != nil {
		t.Fatalf("seed learning fixture: %v", err)
	}
	return l.ID
}

func validRepair(learningID pgtype.UUID) RepairProposal {
	return RepairProposal{
		LearningID:       learningID,
		BehaviorKey:      "PWSPEC-LIVE-BADGE",
		BrokenSelector:   "button.live-bet",
		ProposedSelector: "role=button[name=\"Place bet\"]",
		Rationale:        "a11y snapshot shows an accessible-name button; the css class was renamed",
		Confidence:       0.9,
		EvidenceRef:      "runs/r/step-3/a11y.json",
		ProposedBy:       "navigator",
	}
}

func TestAppendRepairProposalHappyPath(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)
	runID := testRun(t, pool, appID)
	learningID := testLearning(t, pool, appID, runID)

	p, err := AppendRepairProposal(ctx, pool, validRepair(learningID))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if p.Status != RepairProposed {
		t.Fatalf("new proposal status = %q, want %q", p.Status, RepairProposed)
	}
	if p.AppID != appID {
		t.Fatalf("proposal app_id = %v, want inherited %v", p.AppID, appID)
	}
	if !p.ID.Valid {
		t.Fatal("append must return the stored row id")
	}

	// Report-only invariant (part 1): appending a proposal creates ZERO
	// corpus_behaviors rows. The oracle is untouched until a human approves.
	var behaviors int
	if err := pool.QueryRow(ctx,
		`select count(*) from corpus_behaviors where app_id = $1`, appID).Scan(&behaviors); err != nil {
		t.Fatal(err)
	}
	if behaviors != 0 {
		t.Fatalf("proposal append mutated corpus_behaviors (%d rows) — refuse-to-heal violated", behaviors)
	}
}

func TestAppendRepairProposalGates(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)
	runID := testRun(t, pool, appID)
	learningID := testLearning(t, pool, appID, runID)

	// Below the refuse-to-heal floor: refused outright, nothing written.
	below := validRepair(learningID)
	below.Confidence = 0.69
	if _, err := AppendRepairProposal(ctx, pool, below); !errors.Is(err, ErrBelowConfidenceFloor) {
		t.Fatalf("confidence 0.69: err = %v, want ErrBelowConfidenceFloor", err)
	}
	// The floor itself passes (research says ~0.7; gate is >=).
	atFloor := validRepair(learningID)
	atFloor.Confidence = MinRepairConfidence
	if _, err := AppendRepairProposal(ctx, pool, atFloor); err != nil {
		t.Fatalf("confidence exactly at floor must pass: %v", err)
	}

	// Missing required fields.
	for name, mutate := range map[string]func(*RepairProposal){
		"broken_selector":   func(p *RepairProposal) { p.BrokenSelector = "  " },
		"proposed_selector": func(p *RepairProposal) { p.ProposedSelector = "" },
		"rationale":         func(p *RepairProposal) { p.Rationale = "" },
		"behavior_key":      func(p *RepairProposal) { p.BehaviorKey = "" },
		"proposed_by":       func(p *RepairProposal) { p.ProposedBy = " " },
	} {
		p := validRepair(learningID)
		mutate(&p)
		if _, err := AppendRepairProposal(ctx, pool, p); !errors.Is(err, ErrIncompleteProposal) {
			t.Fatalf("missing %s: err = %v, want ErrIncompleteProposal", name, err)
		}
	}

	// Unknown learning: 404 semantics, nothing written.
	missing := validRepair(pgtype.UUID{Bytes: [16]byte{9}, Valid: true})
	if _, err := AppendRepairProposal(ctx, pool, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown learning: err = %v, want ErrNotFound", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`select count(*) from corpus_repair_proposals where learning_id = $1`, learningID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 { // only the at-floor proposal from above
		t.Fatalf("gated proposals must not be written, got %d", count)
	}
}

func TestApproveRepairProposalIsTheOnlyCorpusMutation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)
	runID := testRun(t, pool, appID)
	learningID := testLearning(t, pool, appID, runID)

	p, err := AppendRepairProposal(ctx, pool, validRepair(learningID))
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// Direct application paths (the gates a rogue pipeline stage might try)
	// must all fail without a human approval:
	//   1. Re-append/proposal alone never touches the oracle (covered above).
	//   2. An empty-attribution decision is refused — every write is a
	//      human's, never "the pipeline".
	if _, err := DecideRepairProposal(ctx, pool, p.ID, RepairDecisionApprove, "  "); err == nil {
		t.Fatal("unattributed approval must be refused (human-only corpus writes)")
	}
	//   3. The decision verb itself is closed.
	if _, err := DecideRepairProposal(ctx, pool, p.ID, "auto-apply", "alice"); !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("verb auto-apply: err = %v, want ErrInvalidDecision", err)
	}

	var behaviors int
	if err := pool.QueryRow(ctx,
		`select count(*) from corpus_behaviors where app_id = $1`, appID).Scan(&behaviors); err != nil {
		t.Fatal(err)
	}
	if behaviors != 0 {
		t.Fatalf("corpus mutated before any attributed approval (%d rows)", behaviors)
	}

	// The attributed human approval is the only mutation, and it promotes the
	// learning through the exact manual promote path.
	got, err := DecideRepairProposal(ctx, pool, p.ID, RepairDecisionApprove, "alice")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.Status != RepairApproved || !got.DecidedAt.Valid || got.DecidedBy.String != "alice" {
		t.Fatalf("approved proposal = %+v, want approved/at/alice", got)
	}

	b, err := GetBehavior(ctx, pool, appID, learningBehaviorKey(learningID))
	if err != nil {
		t.Fatalf("approval must promote the proposal's learning into a behavior: %v", err)
	}
	if b.VerifiedBy.String != "alice" {
		t.Fatalf("behavior verified_by = %v, want alice (attributed human)", b.VerifiedBy)
	}

	// One-shot: a second decision on the same proposal is a 409.
	if _, err := DecideRepairProposal(ctx, pool, p.ID, RepairDecisionReject, "bob"); !errors.Is(err, ErrNotAProposal) {
		t.Fatalf("second decision: err = %v, want ErrNotAProposal", err)
	}
}

func TestRejectRepairProposalTouchesNothing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := testApp(t, pool)
	runID := testRun(t, pool, appID)
	learningID := testLearning(t, pool, appID, runID)

	p, err := AppendRepairProposal(ctx, pool, validRepair(learningID))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := DecideRepairProposal(ctx, pool, p.ID, RepairDecisionReject, "alice"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	var behaviors int
	if err := pool.QueryRow(ctx,
		`select count(*) from corpus_behaviors where app_id = $1`, appID).Scan(&behaviors); err != nil {
		t.Fatal(err)
	}
	if behaviors != 0 {
		t.Fatalf("rejection mutated corpus_behaviors (%d rows)", behaviors)
	}

	// The learning stays in the unverified queue for a human to triage.
	var status string
	if err := pool.QueryRow(ctx,
		`select status from corpus_learnings where id = $1`, learningID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != LearningUnverified {
		t.Fatalf("rejected proposal changed learning status to %q, want unverified", status)
	}
}

func TestDecideRepairProposalUnknown(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	missing := pgtype.UUID{Bytes: [16]byte{7}, Valid: true}
	if _, err := DecideRepairProposal(ctx, pool, missing, RepairDecisionReject, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown proposal: err = %v, want ErrNotFound", err)
	}
}
