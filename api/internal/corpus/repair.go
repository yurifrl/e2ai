// Repair proposals (FLAK-02, Phase 5 T7): the report-only half of selector
// repair. When a navigator step fails on locator grounds, the agent may
// APPEND a proposal row here — never a plan rewrite, never a corpus edit.
// A proposal attaches to a corpus learning record (the failure's observation)
// and queues for human review; only an approval mutates the oracle, and it
// does so through the same promote path the manual learning flow uses, so
// approval cannot introduce a second corpus write semantics.
package corpus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Proposal lifecycle. proposed = awaiting a human decision (the only state a
// pipeline stage can create); approved/rejected = terminal, attributed.
const (
	RepairProposed = "proposed"
	RepairApproved = "approved"
	RepairRejected = "rejected"
)

// MinRepairConfidence is the refuse-to-heal floor (RESEARCH decision 4):
// proposals below it are refused at append time — the agent must escalate
// with an observation for human review instead of "healing" on a hunch.
const MinRepairConfidence = 0.7

// Repair decisions on the decide endpoint; approved-only execution per the
// plans/approvals pattern.
const (
	RepairDecisionApprove = "approve"
	RepairDecisionReject  = "reject"
)

// Sentinel errors surfaced by the repair store; REST callers map them to
// 404 / 400 / 409 / 422.
var (
	// ErrNotAProposal: the referenced proposal is not in the proposed state —
	// every human decision is one-shot.
	ErrNotAProposal = errors.New("corpus: repair proposal already decided")
	// ErrInvalidDecision: decision verb is neither approve nor reject.
	ErrInvalidDecision = errors.New("corpus: repair decision must be approve or reject")
	// ErrIncompleteProposal: a required proposal field is empty — the agent
	// must name the broken selector, its replacement, and the why.
	ErrIncompleteProposal = errors.New("corpus: repair proposal is incomplete")
	// ErrBelowConfidenceFloor: refuse-to-heal gate (MinRepairConfidence).
	ErrBelowConfidenceFloor = errors.New("corpus: repair confidence below refuse-to-heal floor")
)

// RepairProposal is one row of corpus_repair_proposals.
type RepairProposal struct {
	ID               pgtype.UUID
	LearningID       pgtype.UUID
	AppID            pgtype.UUID
	BehaviorKey      string
	BrokenSelector   string
	ProposedSelector string
	Rationale        string
	Confidence       float64
	EvidenceRef      string
	Status           string
	ProposedBy       string
	DecidedBy        pgtype.Text
	DecidedAt        pgtype.Timestamptz
	CreatedAt        pgtype.Timestamptz
}

const repairColumns = `id, learning_id, app_id, behavior_key, broken_selector,
	proposed_selector, rationale, confidence, evidence_ref, status, proposed_by,
	decided_by, decided_at, created_at`

func scanRepair(row pgx.Row) (*RepairProposal, error) {
	var p RepairProposal
	err := row.Scan(&p.ID, &p.LearningID, &p.AppID, &p.BehaviorKey, &p.BrokenSelector,
		&p.ProposedSelector, &p.Rationale, &p.Confidence, &p.EvidenceRef, &p.Status,
		&p.ProposedBy, &p.DecidedBy, &p.DecidedAt, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// AppendRepairProposal records a navigator repair suggestion. Refuse-to-heal
// gates run here (defense in depth — the agent side checks first): below the
// confidence floor, or missing any required field, nothing is written and the
// caller must escalate as a plain observation instead.
func AppendRepairProposal(ctx context.Context, pool *pgxpool.Pool, in RepairProposal) (*RepairProposal, error) {
	in.BehaviorKey = strings.TrimSpace(in.BehaviorKey)
	in.BrokenSelector = strings.TrimSpace(in.BrokenSelector)
	in.ProposedSelector = strings.TrimSpace(in.ProposedSelector)
	in.Rationale = strings.TrimSpace(in.Rationale)
	in.EvidenceRef = strings.TrimSpace(in.EvidenceRef)
	if in.BehaviorKey == "" || in.BrokenSelector == "" || in.ProposedSelector == "" || in.Rationale == "" {
		return nil, ErrIncompleteProposal
	}
	if in.Confidence < MinRepairConfidence {
		return nil, fmt.Errorf("%w: %.2f < %.2f", ErrBelowConfidenceFloor, in.Confidence, MinRepairConfidence)
	}
	if strings.TrimSpace(in.ProposedBy) == "" {
		return nil, ErrIncompleteProposal
	}

	// The proposal inherits app_id from its learning — one source of truth,
	// and the agent cannot cross-post into another app's queue.
	var learningAppID pgtype.UUID
	var learningStatus string
	err := pool.QueryRow(ctx, `select app_id, status from corpus_learnings where id = $1`, in.LearningID).
		Scan(&learningAppID, &learningStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: load learning for repair: %w", err)
	}

	p, err := scanRepair(pool.QueryRow(ctx, `
		insert into corpus_repair_proposals
			(learning_id, app_id, behavior_key, broken_selector, proposed_selector,
			 rationale, confidence, evidence_ref, status, proposed_by)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		returning `+repairColumns,
		in.LearningID, learningAppID, in.BehaviorKey, in.BrokenSelector, in.ProposedSelector,
		in.Rationale, in.Confidence, in.EvidenceRef, RepairProposed, strings.TrimSpace(in.ProposedBy)))
	if err != nil {
		return nil, fmt.Errorf("corpus: append repair proposal: %w", err)
	}
	return p, nil
}

// ListRepairProposals returns the app's repair proposals newest first (the
// human-review queue surface).
func ListRepairProposals(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID) ([]RepairProposal, error) {
	rows, err := pool.Query(ctx, `
		select `+repairColumns+` from corpus_repair_proposals
		where app_id = $1 order by created_at desc, id desc`, appID)
	if err != nil {
		return nil, fmt.Errorf("corpus: list repair proposals: %w", err)
	}
	defer rows.Close()
	var out []RepairProposal
	for rows.Next() {
		var p RepairProposal
		if err := rows.Scan(&p.ID, &p.LearningID, &p.AppID, &p.BehaviorKey, &p.BrokenSelector,
			&p.ProposedSelector, &p.Rationale, &p.Confidence, &p.EvidenceRef, &p.Status,
			&p.ProposedBy, &p.DecidedBy, &p.DecidedAt, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("corpus: scan repair proposal: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("corpus: iterate repair proposals: %w", err)
	}
	return out, nil
}

// GetRepairProposal loads one proposal by id, or ErrNotFound.
func GetRepairProposal(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) (*RepairProposal, error) {
	p, err := scanRepair(pool.QueryRow(ctx,
		`select `+repairColumns+` from corpus_repair_proposals where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: get repair proposal: %w", err)
	}
	return p, nil
}

// DecideRepairProposal applies the one-shot human decision. Approval is the
// ONLY path from a proposal to an oracle mutation, and it reuses the manual
// promote transaction verbatim: the proposal's learning is promoted with
// verified_by=decidedBy, exactly as if a reviewer had clicked promote.
// Rejection is a terminal status change and touches nothing else.
func DecideRepairProposal(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID, decision, decidedBy string) (*RepairProposal, error) {
	decidedBy = strings.TrimSpace(decidedBy)
	if decidedBy == "" {
		return nil, fmt.Errorf("corpus: repair decision requires an attributed user (CORP-02: human-only writes)")
	}
	if decision != RepairDecisionApprove && decision != RepairDecisionReject {
		return nil, ErrInvalidDecision
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("corpus: begin repair decision: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// One-shot: only rows still in the proposed state can take a decision.
	// Zero rows updated means absent (404) or already-decided (409) — the
	// re-read below disambiguates without a race window.
	newStatus := RepairApproved
	if decision == RepairDecisionReject {
		newStatus = RepairRejected
	}
	p, err := scanRepair(tx.QueryRow(ctx, `
		update corpus_repair_proposals
		set status = $2, decided_by = $3, decided_at = now()
		where id = $1 and status = $4
		returning `+repairColumns,
		id, newStatus, decidedBy, RepairProposed))
	if errors.Is(err, pgx.ErrNoRows) {
		// Absent (404) or already decided (409); re-read post-rollback to tell
		// them apart without a race window.
		if _, getErr := GetRepairProposal(ctx, pool, id); getErr != nil {
			return nil, ErrNotFound
		}
		return nil, ErrNotAProposal
	}
	if err != nil {
		return nil, fmt.Errorf("corpus: decide repair proposal: %w", err)
	}

	if decision == RepairDecisionApprove {
		// The single oracle mutation this flow is allowed to trigger, through
		// the same transaction and the same promote semantics as the manual
		// flow. A learning that raced to promoted/dismissed fails the whole
		// decision and rolls the status flip back with it.
		if _, err := promoteLearningTx(ctx, tx, p.LearningID, decidedBy); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("corpus: commit repair decision: %w", err)
	}
	return p, nil
}
