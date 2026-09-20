// Atomic worker claim (AGNT-02): one UPDATE with FOR UPDATE SKIP LOCKED
// hands each queued session to exactly one winner, even under concurrent
// claimers. Mirror of the RESEARCH.md §Session claim shape.
package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoSessions is returned when no queued session is available to claim.
var ErrNoSessions = errors.New("no queued agent session")

// ClaimNext atomically claims the oldest queued session for workerID and
// switches it to claimed. Exactly one concurrent claimer wins any given
// row: the subquery takes FOR UPDATE SKIP LOCKED, so losers skip the row
// and read nothing to claim. heartbeat_at is stamped at claim time so a
// worker that dies between claim and first loop beat still lapses cleanly.
func ClaimNext(ctx context.Context, pool *pgxpool.Pool, workerID string) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := pool.QueryRow(ctx, `
		update agent_sessions set
			status = 'claimed',
			worker_id = $1,
			claimed_at = now(),
			heartbeat_at = now()
		where id = (
			select id from agent_sessions
			where status = 'queued'
			order by created_at
			for update skip locked
			limit 1
		)
		returning id
	`, workerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, ErrNoSessions
	}
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("claim next agent session: %w", err)
	}
	return id, nil
}
