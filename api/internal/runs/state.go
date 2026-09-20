// Package runs owns the run pipeline domain: the guarded status state
// machine (queued → waiting-rollout → provisioning → executing → reporting →
// completed | failed) and the read queries backing the runs list / detail
// surfaces (INTG-02 pipeline support, APPS-02 groundwork).
//
// Every status change goes through Transition, whose guarded UPDATE rejects
// both transitions the table forbids and concurrent drift (another worker
// already moved the row) with a typed *TransitionError. No caller writes
// runs.status directly.
package runs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Status is the run pipeline state (runs.status column).
type Status string

const (
	StatusQueued         Status = "queued"
	StatusWaitingRollout Status = "waiting-rollout"
	StatusProvisioning   Status = "provisioning"
	StatusExecuting      Status = "executing"
	StatusReporting      Status = "reporting"
	StatusCompleted      Status = "completed"
	StatusFailed         Status = "failed"
)

// validTransitions is the transition table. Failure exits (→ failed) are legal
// from any active stage — infra errors can surface at every gate (T7 rollout
// timeout, T8 runner crash, T9 verdict-derivation failure) — but terminal
// states (completed, failed) have no outgoing edges: a finished run is final.
var validTransitions = map[Status][]Status{
	StatusQueued:         {StatusWaitingRollout, StatusFailed},
	StatusWaitingRollout: {StatusProvisioning, StatusFailed},
	StatusProvisioning:   {StatusExecuting, StatusFailed},
	StatusExecuting:      {StatusReporting, StatusFailed},
	StatusReporting:      {StatusCompleted, StatusFailed},
	StatusCompleted:      nil,
	StatusFailed:         nil,
}

// Can reports whether from → to is a legal transition.
func Can(from, to Status) bool {
	for _, next := range validTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Terminal reports whether the status is final (no further transitions).
func (s Status) Terminal() bool {
	return len(validTransitions[s]) == 0
}

// ErrInvalidTransition is the sentinel wrapped by every TransitionError.
var ErrInvalidTransition = errors.New("invalid run status transition")

// ErrNotFound is returned when the run id does not exist.
var ErrNotFound = errors.New("run not found")

// TransitionError is the typed rejection for an illegal or lost-race status
// change. It wraps ErrInvalidTransition (except not-found cases, which are
// ErrNotFound) so callers can errors.Is / errors.As on it.
type TransitionError struct {
	From   Status
	To     Status
	Reason string // "forbidden by transition table" | "status changed concurrently"
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("run transition %s → %s rejected: %s", e.From, e.To, e.Reason)
}

func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

// DB is the executor surface the package needs; satisfied by both
// *pgxpool.Pool and pgx.Tx, so callers compose transitions with their own
// transactions when they must.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Transition atomically moves the run from its current status to to. The
// UPDATE is guarded on the observed current status, so a concurrent worker
// that moved the row first makes this a typed rejection, never a silent
// overwrite. started_at is stamped on the first departure from queued;
// finished_at is stamped when entering a terminal status.
func Transition(ctx context.Context, db DB, id pgtype.UUID, to Status) error {
	var current Status
	err := db.QueryRow(ctx, `select status from runs where id = $1`, id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("read run status: %w", err)
	}
	if !Can(current, to) {
		return &TransitionError{From: current, To: to, Reason: "forbidden by transition table"}
	}

	tag, err := db.Exec(ctx, `
		update runs set
			status = $2,
			started_at = coalesce(started_at, now()),
			finished_at = case when $2 in ('completed', 'failed') then now() else finished_at end
		where id = $1 and status = $3
	`, id, to, current)
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Another worker moved the row between the read and the update.
		var nowStatus Status
		err := db.QueryRow(ctx, `select status from runs where id = $1`, id).Scan(&nowStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		if err != nil {
			return fmt.Errorf("re-read run status after lost race: %w", err)
		}
		return &TransitionError{From: nowStatus, To: to, Reason: "status changed concurrently"}
	}
	return nil
}
