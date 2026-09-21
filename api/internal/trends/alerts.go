// Alert events (TREN-01 alerting, Phase 5 T4): the durable records the
// flake trigger writes (internal/flakes.InsertFlakeThresholdAlert) and this
// surface reads. Acknowledgement is this package's single write — the
// package doc's read-only rule covers run aggregations; an alert queue
// without an ack action would drain nobody.
package trends

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Alert is one alert_events row.
type Alert struct {
	ID           string          `json:"id"`
	AppID        string          `json:"app_id"`
	Kind         string          `json:"kind"` // flake_threshold | flip_rate
	Payload      json.RawMessage `json:"payload"`
	Acknowledged bool            `json:"acknowledged"`
	CreatedAt    time.Time       `json:"created_at"`
}

// ListAlerts returns alerts, newest first, optionally scoped to one app
// and/or unacknowledged only. limit is clamped to a sane band.
func ListAlerts(ctx context.Context, pool *pgxpool.Pool, appID *pgtype.UUID, unacknowledgedOnly bool, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	sql := `select id::text, app_id::text, kind, payload, acknowledged, created_at from alert_events where true`
	args := []any{}
	n := 1
	if appID != nil {
		sql += fmt.Sprintf(" and app_id = $%d", n)
		args = append(args, *appID)
		n++
	}
	if unacknowledgedOnly {
		sql += " and not acknowledged"
	}
	sql += fmt.Sprintf(" order by created_at desc limit $%d", n)
	args = append(args, limit)

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("trends: query alerts: %w", err)
	}
	defer rows.Close()

	alerts := []Alert{}
	for rows.Next() {
		var a Alert
		var payload []byte
		if err := rows.Scan(&a.ID, &a.AppID, &a.Kind, &payload, &a.Acknowledged, &a.CreatedAt); err != nil {
			return nil, fmt.Errorf("trends: scan alert: %w", err)
		}
		a.Payload = payload
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trends: iterate alerts: %w", err)
	}
	return alerts, nil
}

// AcknowledgeAlert flags one alert acknowledged. applied=false with
// exists=true means the alert was already acknowledged (idempotent re-ack
// attempt); exists=false means the id does not exist.
func AcknowledgeAlert(ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) (applied, exists bool, err error) {
	tag, err := pool.Exec(ctx,
		`update alert_events set acknowledged = true where id = $1 and not acknowledged`, id)
	if err != nil {
		return false, false, fmt.Errorf("trends: acknowledge alert: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return true, true, nil
	}
	var present bool
	if err := pool.QueryRow(ctx,
		`select exists(select 1 from alert_events where id = $1)`, id).Scan(&present); err != nil {
		return false, false, fmt.Errorf("trends: probe alert: %w", err)
	}
	return false, present, nil
}
