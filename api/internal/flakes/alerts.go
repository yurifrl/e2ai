// Alert store (TREN-01 alerting, Phase 5 T4): alert_events is the durable
// record a threshold crossing produces — written here, surfaced read-only by
// the trends API, acknowledged by admins. The write path is deliberately
// dedupe-gated: a threshold that stays crossed must fire once per cooldown
// window, not once per tick, or the queue becomes the alert.
package flakes

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AlertCooldownHours is the alert/analyst dedupe window: a crossed flake
// threshold fires one alert and enqueues one flake_analyst session at most
// once per day. Sane default (05-01-PLAN T4); the analyst session dedupe in
// internal/agents/analyst.go reuses this constant so both halves of a
// crossing stay on one clock.
const AlertCooldownHours = 24

// InsertFlakeThresholdAlert inserts a flake_threshold alert for the app
// unless one already fired within AlertCooldownHours. Reports whether a new
// row landed (false = suppressed by the cooldown). The insert is a single
// guarded statement, so concurrent ticks cannot double-fire.
func InsertFlakeThresholdAlert(ctx context.Context, pool *pgxpool.Pool, appID pgtype.UUID, payload []byte) (bool, error) {
	tag, err := pool.Exec(ctx, `
		insert into alert_events (app_id, kind, payload)
		select $1, 'flake_threshold', $2::jsonb
		where not exists (
			select 1 from alert_events
			where app_id = $1
			  and kind = 'flake_threshold'
			  and created_at > now() - make_interval(hours => $3::int)
		)
	`, appID, string(payload), AlertCooldownHours)
	if err != nil {
		return false, fmt.Errorf("flakes: insert threshold alert: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
