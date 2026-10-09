// Arm state of record (COCK-04): a single-row table the pause control
// renders and every actuating boundary (manual trigger, webhook simulate,
// kill switch) consults. Paused is server-owned — the client renders it but
// never owns it, so the state survives reloads by construction.
package cockpit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNoActor refuses actorless mutations before any write: an audit row
// without an operator email must never exist, so a caller that cannot name
// the actor cannot pause or unpause. The HTTP boundary refuses placeholder
// identities earlier still; this is the domain's line of defense.
var ErrNoActor = errors.New("cockpit mutation requires an actor email")

// State is one read of cockpit_state.
type State struct {
	Paused       bool
	PausedReason string
	PausedBy     string
	PausedAt     *time.Time
	UpdatedAt    time.Time
}

// Read returns the current arm state. A missing row reads as unpaused (the
// seeded default); every write upserts, so a lost seed self-heals on the
// first write.
func Read(ctx context.Context, pool *pgxpool.Pool) (State, error) {
	var s State
	err := pool.QueryRow(ctx, `
		select paused, coalesce(paused_reason, ''), coalesce(paused_by, ''), paused_at, updated_at
		from cockpit_state
		where id = true`).Scan(&s.Paused, &s.PausedReason, &s.PausedBy, &s.PausedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{UpdatedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read cockpit_state: %w", err)
	}
	return s, nil
}

// readTx is Read against an open transaction — the in-tx pause re-check
// (REVIEW WR-03) reads the row the pause tx is committing on the same
// connection it will spend on. Same missing-row default as Read.
func readTx(ctx context.Context, tx pgx.Tx) (State, error) {
	var s State
	err := tx.QueryRow(ctx, `
		select paused, coalesce(paused_reason, ''), coalesce(paused_by, ''), paused_at, updated_at
		from cockpit_state
		where id = true`).Scan(&s.Paused, &s.PausedReason, &s.PausedBy, &s.PausedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{UpdatedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("read cockpit_state: %w", err)
	}
	return s, nil
}

// SetPaused flips the arm state and appends the audit row in one transaction:
// the flag change and its attribution commit together or not at all
// (AUDT-01). Unpausing clears the pause attribution — the row always
// describes the current state; history lives in audit_events.
func SetPaused(ctx context.Context, pool *pgxpool.Pool, actorEmail string, paused bool, reason string) error {
	if actorEmail == "" {
		return ErrNoActor
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pause tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if paused {
		if _, err := tx.Exec(ctx, `
			insert into cockpit_state (id, paused, paused_reason, paused_by, paused_at, updated_at)
			values (true, true, nullif($1, ''), $2, now(), now())
			on conflict (id) do update set
				paused = true,
				paused_reason = excluded.paused_reason,
				paused_by = excluded.paused_by,
				paused_at = now(),
				updated_at = excluded.updated_at`,
			reason, actorEmail); err != nil {
			return fmt.Errorf("upsert cockpit_state: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			insert into cockpit_state (id, paused, paused_reason, paused_by, paused_at, updated_at)
			values (true, false, null, null, null, now())
			on conflict (id) do update set
				paused = false,
				paused_reason = null,
				paused_by = null,
				paused_at = null,
				updated_at = excluded.updated_at`); err != nil {
			return fmt.Errorf("upsert cockpit_state: %w", err)
		}
	}

	action := ActionPause
	if !paused {
		action = ActionUnpause
	}
	if err := AppendAudit(ctx, tx, AuditEntry{
		ActorEmail: actorEmail,
		Action:     action,
		TargetType: "cockpit_state",
		TargetID:   "cockpit_state",
		Reason:     reason,
		Details:    map[string]any{"paused": paused},
	}); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pause tx: %w", err)
	}
	return nil
}
