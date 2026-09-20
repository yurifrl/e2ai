// Package agents owns the agent-session orchestrator domain (Phase 3 T1):
// the guarded session status machine, the atomic worker claim, the orphan
// sweep (River periodic), and the budget engine (step ceiling + cost
// accumulator enforced inside the step-append transaction).
//
// Sessions are the state of record for the agentic loop: agentrunner
// processes are ephemeral, all coordination flows through agent_sessions +
// session_steps rows (ARCHITECTURE Pattern 1). Every status change goes
// through Transition, whose guarded UPDATE rejects both transitions the
// table forbids and concurrent drift with a typed *TransitionError. No
// caller writes agent_sessions.status directly.
package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Role is the agent roster (AGNT-01, agent_sessions.role check).
type Role string

const (
	RoleNavigator    Role = "navigator"
	RoleVisionJudge  Role = "vision_judge"
	RolePlanAuthor   Role = "plan_author"
	RoleEdgeHunter   Role = "edge_hunter"
	RoleFlakeAnalyst Role = "flake_analyst"
)

// Status is the session state (agent_sessions.status column).
type Status string

const (
	StatusQueued          Status = "queued"
	StatusClaimed         Status = "claimed"
	StatusRunning         Status = "running"
	StatusCompleted       Status = "completed"
	StatusFailed          Status = "failed"
	StatusKilled          Status = "killed"
	StatusBudgetExhausted Status = "budget_exhausted"
)

// validTransitions is the transition table. Failure exits (→ failed,
// killed, budget_exhausted) are legal from every active stage: a runner
// crash, a kill switch, or a budget breach can surface at any point in the
// loop (AGNT-02, BUDG-01/02). Terminal states have no outgoing edges — a
// finished session is final, and BudgetExhausted/killed are never retried.
var validTransitions = map[Status][]Status{
	StatusQueued:          {StatusClaimed},
	StatusClaimed:         {StatusRunning, StatusFailed, StatusKilled, StatusBudgetExhausted},
	StatusRunning:         {StatusCompleted, StatusFailed, StatusKilled, StatusBudgetExhausted},
	StatusCompleted:       nil,
	StatusFailed:          nil,
	StatusKilled:          nil,
	StatusBudgetExhausted: nil,
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
var ErrInvalidTransition = errors.New("invalid agent session status transition")

// ErrNotFound is returned when the session id does not exist.
var ErrNotFound = errors.New("agent session not found")

// TransitionError is the typed rejection for an illegal or lost-race
// status change. It wraps ErrInvalidTransition (except not-found cases,
// which are ErrNotFound) so callers can errors.Is / errors.As on it.
type TransitionError struct {
	From   Status
	To     Status
	Reason string // "forbidden by transition table" | "status changed concurrently"
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("agent session transition %s → %s rejected: %s", e.From, e.To, e.Reason)
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

// Transition atomically moves the session from its current status to to.
// The UPDATE is guarded on the observed current status, so a concurrent
// worker that moved the row first makes this a typed rejection, never a
// silent overwrite. started_at is stamped on the first departure from
// queued; finished_at is stamped when entering a terminal status.
func Transition(ctx context.Context, db DB, id pgtype.UUID, to Status) error {
	var current Status
	err := db.QueryRow(ctx, `select status from agent_sessions where id = $1`, id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("read agent session status: %w", err)
	}
	if !Can(current, to) {
		return &TransitionError{From: current, To: to, Reason: "forbidden by transition table"}
	}

	tag, err := db.Exec(ctx, `
		update agent_sessions set
			status = $2,
			started_at = coalesce(started_at, now()),
			finished_at = case when $2 in ('completed', 'failed', 'killed', 'budget_exhausted') then now() else finished_at end
		where id = $1 and status = $3
	`, id, to, current)
	if err != nil {
		return fmt.Errorf("update agent session status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Another worker moved the row between the read and the write.
		return &TransitionError{From: current, To: to, Reason: "status changed concurrently"}
	}
	return nil
}

// Heartbeat records liveness. The 90s lapse threshold beats any legit long
// step (RESEARCH pitfalls); a lapsed heartbeat makes the orphan sweep
// requeue the session as failed. Heartbeating a terminal session is a typed
// rejection — that is where the kill switch's 409 semantics land in T2.
func Heartbeat(ctx context.Context, db DB, id pgtype.UUID) error {
	var status Status
	err := db.QueryRow(ctx, `select status from agent_sessions where id = $1`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("read agent session status: %w", err)
	}
	if status.Terminal() {
		return &TransitionError{From: status, To: status, Reason: "session is terminal; heartbeat rejected"}
	}
	tag, err := db.Exec(ctx, `
		update agent_sessions set heartbeat_at = now()
		where id = $1 and status in ('claimed', 'running')
	`, id)
	if err != nil {
		return fmt.Errorf("heartbeat agent session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return &TransitionError{From: status, To: status, Reason: "status changed concurrently"}
	}
	return nil
}
