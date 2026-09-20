// Package approvals implements the plan-version approval state machine
// (APPR-01) and its REST surface (APPR-02): drafts queue for human review,
// only pending_approval versions take a decision, and every decision is
// attributed (approver, time, notes). The state machine is deliberately
// small — research §Pattern 4:
//
//	draft ──submit──▶ pending_approval
//	pending_approval ──approve──▶ approved   (previous approved → superseded)
//	pending_approval ──reject───▶ rejected
//
// Anything else is a conflict surfaced as 409 to the UI: a decided version
// never flips again (unique(plan_version_id, decision) backstops it), and
// sync-created pending versions are decided one at a time — approving one
// does not silently invalidate the others, they simply stay pending until
// decided or superseded by a later approval.
package approvals

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Version statuses (plan_versions.status — the 0002 check constraint is the
// vocabulary source of truth; plan-level status uses 'pending', version
// level 'pending_approval').
const (
	StatusDraft           = "draft"
	StatusPendingApproval = "pending_approval"
	StatusApproved        = "approved"
	StatusRejected        = "rejected"
	StatusSuperseded      = "superseded"
)

var (
	// ErrNotFound marks an unknown version id (404).
	ErrNotFound = errors.New("approvals: version not found")
	// ErrNotPending marks a decide attempt on a version that is not
	// awaiting a decision (draft, already decided, or superseded) — 409.
	ErrNotPending = errors.New("approvals: version is not awaiting a decision")
	// ErrNotDraft marks a submit attempt on a version that is not a draft
	// — 409.
	ErrNotDraft = errors.New("approvals: version is not a draft")
	// ErrBadDecision marks an unknown decision verb — 400.
	ErrBadDecision = errors.New("approvals: unknown decision")
)

// Transition is the pure heart of the state machine: current version status
// + decision verb → next version status. The DB layer enforces the same
// rule atomically (conditional UPDATE), so this function is the documented
// contract the store implements and the table-test target.
func Transition(from, decision string) (string, error) {
	switch decision {
	case "submit":
		if from == StatusDraft {
			return StatusPendingApproval, nil
		}
		return "", ErrNotDraft
	case "approved":
		if from == StatusPendingApproval {
			return StatusApproved, nil
		}
		return "", ErrNotPending
	case "rejected":
		if from == StatusPendingApproval {
			return StatusRejected, nil
		}
		return "", ErrNotPending
	default:
		return "", ErrBadDecision
	}
}

// Approval is one attributed decision row (the wire PlanApproval shape).
type Approval struct {
	ID            pgtype.UUID
	PlanID        string
	PlanVersionID pgtype.UUID
	Decision      string
	Approver      string
	Notes         *string
	DecidedAt     time.Time
}

// Submit moves a draft version to pending_approval for human review. The
// derivation pipeline uses it; a submit endpoint can grow on the same seam.
func Submit(ctx context.Context, pool *pgxpool.Pool, versionID pgtype.UUID) error {
	tag, err := pool.Exec(ctx, `
		update plan_versions set status = $2
		where id = $1 and status = $3`,
		versionID, StatusPendingApproval, StatusDraft)
	if err != nil {
		return fmt.Errorf("submit version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var status string
		err := pool.QueryRow(ctx,
			`select status from plan_versions where id = $1`, versionID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lookup version: %w", err)
		}
		return fmt.Errorf("%w: status %q", ErrNotDraft, status)
	}
	var planID string
	if err := pool.QueryRow(ctx,
		`select plan_id from plan_versions where id = $1`, versionID).Scan(&planID); err != nil {
		return fmt.Errorf("lookup submitted version plan: %w", err)
	}
	if err := refreshPlanStatus(ctx, pool, planID); err != nil {
		return err
	}
	return nil
}

// refreshPlanStatus re-derives plans.status from its version statuses with
// the same rank expression as the sync pipeline (sync.go upsertPlanVersion);
// pool and tx both satisfy execer.
func refreshPlanStatus(ctx context.Context, q execer, planID string) error {
	if _, err := q.Exec(ctx, `
		update plans set
			status = (select case max(rank)
					when 4 then 'approved'
					when 3 then 'pending'
					when 2 then 'rejected'
					else 'draft' end
				from (select case status
						when 'approved' then 4
						when 'pending_approval' then 3
						when 'superseded' then 3
						when 'rejected' then 2
						else 1 end as rank
					from plan_versions where plan_id = $1) ranks),
			updated_at = now()
		where id = $1`, planID); err != nil {
		return fmt.Errorf("refresh plan status: %w", err)
	}
	return nil
}

// execer abstracts the Exec surface shared by pgxpool.Pool and pgx.Tx.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Decide applies one human decision to a pending_approval version and
// returns the attributed approval row. The status flip is a conditional
// UPDATE inside the transaction: a version that left pending between queue
// view and click conflicts (409) instead of double-deciding. Approving
// supersedes the plan's currently approved version — the previously
// approved version keeps existing for diffing, it just stops being current
// — and refreshes the plan-level status with the same rank expression the
// sync pipeline uses (sync.go upsertPlanVersion).
func Decide(ctx context.Context, pool *pgxpool.Pool, versionID pgtype.UUID, decision, approver, notes string) (Approval, error) {
	next, err := Transition(StatusPendingApproval, decision)
	if err != nil {
		return Approval{}, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return Approval{}, fmt.Errorf("begin decide: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var planID string
	err = tx.QueryRow(ctx, `
		update plan_versions set status = $2
		where id = $1 and status = $3
		returning plan_id`,
		versionID, next, StatusPendingApproval).Scan(&planID)
	if errors.Is(err, pgx.ErrNoRows) {
		var status string
		qerr := tx.QueryRow(ctx,
			`select status from plan_versions where id = $1`, versionID).Scan(&status)
		if errors.Is(qerr, pgx.ErrNoRows) {
			return Approval{}, ErrNotFound
		}
		if qerr != nil {
			return Approval{}, fmt.Errorf("lookup version: %w", qerr)
		}
		return Approval{}, fmt.Errorf("%w: status %q", ErrNotPending, status)
	}
	if err != nil {
		return Approval{}, fmt.Errorf("decide version: %w", err)
	}

	// Empty notes are not worth a row value: store NULL so the wire shape
	// renders null consistently.
	var storedNotes *string
	if trimmed := strings.TrimSpace(notes); trimmed != "" {
		storedNotes = &trimmed
	}

	a := Approval{
		PlanID:        planID,
		PlanVersionID: versionID,
		Decision:      decision,
		Approver:      approver,
		Notes:         storedNotes,
	}
	err = tx.QueryRow(ctx, `
		insert into plan_approvals (plan_version_id, decision, approver, notes)
		values ($1, $2, $3, $4)
		returning id, decided_at`,
		versionID, decision, approver, storedNotes).Scan(&a.ID, &a.DecidedAt)
	if err != nil {
		return Approval{}, fmt.Errorf("record approval: %w", err)
	}

	if decision == "approved" {
		if _, err := tx.Exec(ctx, `
			update plan_versions set status = 'superseded'
			where plan_id = $1 and id <> $2 and status = 'approved'`,
			planID, versionID); err != nil {
			return Approval{}, fmt.Errorf("supersede previous approval: %w", err)
		}
	}

	// Plan-level status refresh — same rank expression as the sync pipeline
	// (plan vocabulary is draft|pending|approved|rejected; superseded counts
	// with pending at rank 3).
	if err := refreshPlanStatus(ctx, tx, planID); err != nil {
		return Approval{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Approval{}, fmt.Errorf("commit decide: %w", err)
	}
	return a, nil
}
