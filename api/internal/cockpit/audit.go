// Package cockpit is the control-plane substrate for Phase 6: the arm's
// pause state, the audit trail every cockpit mutation appends in the same
// transaction as its domain write, and the trigger-time budget gate. Plan
// 06-01 ships the pause vertical (audit + state + HTTP surface); Plans 02,
// 04, and 05 layer the trigger boundary, Command Center read model, and
// webhook simulation on top of the same substrate.
package cockpit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Audit actions. Closed by the audit_events check constraint; mirrors the
// migration's vocabulary one-to-one.
const (
	ActionTrigger  = "trigger"
	ActionSimulate = "simulate"
	ActionPause    = "pause"
	ActionUnpause  = "unpause"
)

// AuditEntry is one attributed cockpit mutation: the actor, a
// closed-vocabulary action, a target reference, an optional reason, and
// free-form details.
type AuditEntry struct {
	ActorEmail string
	Action     string
	TargetType string
	TargetID   string
	Reason     string // empty → NULL
	Details    map[string]any
}

// AppendAudit writes the audit row on the caller's open transaction so the
// domain mutation and its attribution commit together or not at all
// (AUDT-01). The caller owns begin/commit; this never touches transaction
// state, which is what makes partial writes structurally impossible.
func AppendAudit(ctx context.Context, tx pgx.Tx, e AuditEntry) error {
	details := []byte("{}")
	if len(e.Details) > 0 {
		raw, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("marshal audit details: %w", err)
		}
		details = raw
	}
	var reason any
	if e.Reason != "" {
		reason = e.Reason
	}
	if _, err := tx.Exec(ctx, `
		insert into audit_events (actor_email, action, target_type, target_id, reason, details)
		values ($1, $2, $3, $4, $5, $6)`,
		e.ActorEmail, e.Action, e.TargetType, e.TargetID, reason, details); err != nil {
		return fmt.Errorf("insert audit_events: %w", err)
	}
	return nil
}
