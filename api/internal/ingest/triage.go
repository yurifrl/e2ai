// Triage (EDGE-01/APPS-03): signature-valid deliveries no registered app
// claims are recorded as webhook_triage rows instead of vanishing into a
// debug log — the admin triage queue is the human gate for extending the
// registry. Redeliveries upsert (occurrences + last_seen_at), they never
// duplicate.
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReasonUnknownApp marks the only triage reason produced today.
const ReasonUnknownApp = "unknown-app"

// TriageStore persists the unknown-delivery event. The indirection keeps the
// webhook handler testable with a stub (the main.go wiring passes the pgx
// implementation).
type TriageStore interface {
	RecordUnknownDelivery(ctx context.Context, event UnknownDelivery) error
}

// UnknownDelivery is one triaged delivery.
type UnknownDelivery struct {
	DeliveryID    string
	WorkflowName  string
	DisplayTitle  string
	WorkflowRunID int64
	Payload       []byte // verbatim body (evidence trail)
}

// Triage is the pgx-backed TriageStore.
type Triage struct {
	Pool *pgxpool.Pool
}

// RecordUnknownDelivery upserts on delivery_id: first sight inserts,
// redeliveries bump occurrences and last_seen_at.
func (t *Triage) RecordUnknownDelivery(ctx context.Context, ev UnknownDelivery) error {
	if t == nil || t.Pool == nil {
		return fmt.Errorf("ingest: triage store not wired")
	}
	payload := ev.Payload
	if payload == nil {
		payload = []byte("{}")
	}
	var runID any // int64 or null (unknown run ids still happen)
	if ev.WorkflowRunID > 0 {
		runID = ev.WorkflowRunID
	}
	_, err := t.Pool.Exec(ctx, `
		insert into webhook_triage
			(delivery_id, workflow_name, display_title, workflow_run_id, payload, signature_valid, reason)
		values ($1, $2, $3, $4, $5, true, $6)
		on conflict (delivery_id) do update set
			last_seen_at = now(),
			occurrences = webhook_triage.occurrences + 1
	`, ev.DeliveryID, ev.WorkflowName, ev.DisplayTitle, runID, payload, ReasonUnknownApp)
	if err != nil {
		return fmt.Errorf("ingest: record unknown delivery: %w", err)
	}
	return nil
}

// TriageEntry is one webhook_triage row as served to the admin surface.
type TriageEntry struct {
	ID            string          `json:"id"`
	DeliveryID    string          `json:"delivery_id"`
	WorkflowName  string          `json:"workflow_name"`
	DisplayTitle  string          `json:"display_title"`
	WorkflowRunID int64           `json:"workflow_run_id,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Reason        string          `json:"reason"`
	CreatedAt     time.Time       `json:"created_at"`
	LastSeenAt    time.Time       `json:"last_seen_at"`
	Occurrences   int             `json:"occurrences"`
}

// ListTriage returns the triage queue, newest activity first.
func ListTriage(ctx context.Context, pool *pgxpool.Pool) ([]TriageEntry, error) {
	rows, err := pool.Query(ctx, `
		select id, delivery_id, workflow_name, display_title,
		       coalesce(workflow_run_id, 0), payload, reason,
		       created_at, last_seen_at, occurrences
		from webhook_triage
		order by last_seen_at desc, created_at desc
	`)
	if err != nil {
		return nil, fmt.Errorf("ingest: list triage: %w", err)
	}
	defer rows.Close()

	entries := []TriageEntry{}
	for rows.Next() {
		var e TriageEntry
		var id pgtype.UUID
		if err := rows.Scan(&id, &e.DeliveryID, &e.WorkflowName, &e.DisplayTitle,
			&e.WorkflowRunID, &e.Payload, &e.Reason,
			&e.CreatedAt, &e.LastSeenAt, &e.Occurrences); err != nil {
			return nil, fmt.Errorf("ingest: scan triage row: %w", err)
		}
		b, _ := id.Value()
		if s, ok := b.(string); ok {
			e.ID = s
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ingest: iterate triage rows: %w", err)
	}
	return entries, nil
}

// NewTriage builds the pgx-backed TriageStore for main.go wiring.
func NewTriage(pool *pgxpool.Pool) *Triage { return &Triage{Pool: pool} }
