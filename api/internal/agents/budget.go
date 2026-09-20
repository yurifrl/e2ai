// Budget engine (BUDG-01): step ceiling + cost accumulator hard stops
// enforced inside the same transaction as the step insert. A breach flips
// the session to budget_exhausted — terminal and non-retryable everywhere
// (River MaxAttempts 1 in T4, gateway cap documented in T7).
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrBudgetExhausted is the sentinel for a step- or cost-ceiling breach.
// Callers must treat it as non-retryable (documented BudgetExhausted
// semantics, T3/T4 contract).
var ErrBudgetExhausted = errors.New("agent session budget exhausted")

// ErrTerminal is returned when appending a step to a session already in a
// terminal state (completed/failed/killed/budget_exhausted). A lapsed or
// killed session's in-flight appends bounce here.
var ErrTerminal = errors.New("agent session is terminal")

// Step is one append-only trajectory row (session_steps).
type Step struct {
	Kind           string          // navigate|act|observe|judge|assert|error
	Decision       json.RawMessage // what the agent decided
	Action         json.RawMessage // what it did about it
	Evidence       json.RawMessage // artifact refs
	TokensIn       int
	TokensOut      int
	CostMillicents int64
}

// CostMillicents converts token counts into a step cost using per-model
// price rates (millicents per 1k tokens, from the T3 price table's Go
// mirror). Rounding truncates at whole millicents.
func CostMillicents(tokensIn, tokensOut int, milliCentsPer1KIn, milliCentsPer1KOut int64) int64 {
	return (int64(tokensIn)*milliCentsPer1KIn + int64(tokensOut)*milliCentsPer1KOut) / 1000
}

// AppendStep appends one trajectory step and applies budget accounting in
// the SAME transaction: the session row is locked FOR UPDATE, ceiling
// breaches flip status to budget_exhausted (terminal, finished_at stamped)
// and reject the step, success bumps steps_used and cost_millicents.
func AppendStep(ctx context.Context, pool *pgxpool.Pool, sessionID pgtype.UUID, step Step) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin step append: %w", err)
	}
	defer tx.Rollback(ctx)

	var status Status
	var stepsUsed int
	var cost int64
	var stepCeiling int
	var costCeiling int64
	err = tx.QueryRow(ctx, `
		select status, steps_used, cost_millicents, step_ceiling, cost_ceiling_millicents
		from agent_sessions where id = $1 for update
	`, sessionID).Scan(&status, &stepsUsed, &cost, &stepCeiling, &costCeiling)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}
	if err != nil {
		return fmt.Errorf("read agent session for budget: %w", err)
	}
	if status.Terminal() {
		return fmt.Errorf("%w: %s", ErrTerminal, status)
	}

	// Hard stops are PROSPECTIVE (BUDG-02): a step that would cross a
	// ceiling is rejected and terminates the session in the same tx —
	// budgets never overshoot, and the runner path closes at the boundary.
	if stepCeiling > 0 && stepsUsed+1 > stepCeiling {
		note := fmt.Sprintf("step ceiling %d reached", stepCeiling)
		if err := markBudgetExhausted(ctx, tx, sessionID, note); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit budget exhaustion: %w", err)
		}
		return fmt.Errorf("%w: %s", ErrBudgetExhausted, note)
	}
	if costCeiling > 0 && cost+step.CostMillicents > costCeiling {
		note := fmt.Sprintf("cost ceiling %d millicents reached", costCeiling)
		if err := markBudgetExhausted(ctx, tx, sessionID, note); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit budget exhaustion: %w", err)
		}
		return fmt.Errorf("%w: %s", ErrBudgetExhausted, note)
	}

	if _, err := tx.Exec(ctx, `
		insert into session_steps (session_id, idx, kind, decision, action, evidence, tokens_in, tokens_out, cost_millicents)
		values ($1, $2, $3, coalesce($4, '{}'::jsonb), coalesce($5, '{}'::jsonb),
		coalesce($6, '[]'::jsonb), $7, $8, $9)
	`, sessionID, stepsUsed, step.Kind, step.Decision, step.Action, step.Evidence,
		step.TokensIn, step.TokensOut, step.CostMillicents); err != nil {
		return fmt.Errorf("insert session step: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update agent_sessions set
			steps_used = steps_used + 1,
			cost_millicents = cost_millicents + $2
		where id = $1
	`, sessionID, step.CostMillicents); err != nil {
		return fmt.Errorf("update session budget accumulator: %w", err)
	}
	return tx.Commit(ctx)
}

// markBudgetExhausted flips the locked session to its terminal budget
// status with the breach recorded in error_note (BUDG-01 evidence trail).
func markBudgetExhausted(ctx context.Context, tx pgx.Tx, id pgtype.UUID, note string) error {
	_, err := tx.Exec(ctx, `
		update agent_sessions set
			status = 'budget_exhausted',
			finished_at = now(),
			error_note = coalesce(error_note || ' | ', '') || $2
		where id = $1
	`, id, note)
	if err != nil {
		return fmt.Errorf("mark budget exhausted: %w", err)
	}
	return nil
}
