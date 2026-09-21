// Alert-surface query tests (Phase 5 T4): listing filters (scope,
// unacknowledged) and idempotent acknowledgement.
package trends

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/NSXBet/e2ai/api/internal/app"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("E2AI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial scratch postgres: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := app.Migrate(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`truncate registered_apps cascade`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

func seedAppAndAlerts(t *testing.T, pool *pgxpool.Pool) (appID pgtype.UUID, ids []pgtype.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker)
		values ('yuri', $1, 'e2ai', '.github/workflows/e2ai.yml', 'prod',
			 '*', 'ref', 'marker')
		returning id
	`, uuid.NewString()).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	for range 2 {
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, `
			insert into alert_events (app_id, kind, payload) values ($1, 'flake_threshold', '{}')
			returning id
		`, appID).Scan(&id); err != nil {
			t.Fatalf("seed alert: %v", err)
		}
		ids = append(ids, id)
	}
	return appID, ids
}

func TestListAlertsFilters(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID, ids := seedAppAndAlerts(t, pool)

	all, err := ListAlerts(ctx, pool, &appID, false, 0)
	if err != nil {
		t.Fatalf("ListAlerts all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all alerts = %d, want 2", len(all))
	}

	// Acknowledge one; the unacknowledged filter then sees exactly one.
	if _, _, err := AcknowledgeAlert(ctx, pool, ids[0]); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}

	unacked, err := ListAlerts(ctx, pool, &appID, true, 0)
	if err != nil {
		t.Fatalf("ListAlerts unacknowledged: %v", err)
	}
	if len(unacked) != 1 {
		t.Fatalf("unacknowledged alerts = %d, want 1", len(unacked))
	}
	if unacked[0].ID == ids[0].String() || unacked[0].Acknowledged {
		t.Errorf("unacknowledged filter returned the acknowledged alert %s", unacked[0].ID)
	}

	// App scoping: a second app's alerts stay invisible.
	var other pgtype.UUID
	if err := pool.QueryRow(ctx, `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker)
		values ('yuri', $1, 'e2ai', '.github/workflows/e2ai.yml', 'prod',
			 '*', 'ref', 'marker')
		returning id
	`, uuid.NewString()).Scan(&other); err != nil {
		t.Fatalf("seed other app: %v", err)
	}
	scoped, err := ListAlerts(ctx, pool, &other, false, 0)
	if err != nil {
		t.Fatalf("ListAlerts scoped: %v", err)
	}
	if len(scoped) != 0 {
		t.Fatalf("other app's alerts = %d, want 0 (leaked across apps)", len(scoped))
	}
}

func TestAcknowledgeAlertIdempotence(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	_, ids := seedAppAndAlerts(t, pool)

	applied, exists, err := AcknowledgeAlert(ctx, pool, ids[0])
	if err != nil {
		t.Fatalf("first AcknowledgeAlert: %v", err)
	}
	if !applied || !exists {
		t.Fatalf("first ack = (applied %v, exists %v), want (true, true)", applied, exists)
	}

	applied, exists, err = AcknowledgeAlert(ctx, pool, ids[0])
	if err != nil {
		t.Fatalf("second AcknowledgeAlert: %v", err)
	}
	if applied || !exists {
		t.Fatalf("re-ack = (applied %v, exists %v), want (false, true) — idempotent no-op", applied, exists)
	}

	unknown := pgtype.UUID{}
	if err := unknown.Scan(uuid.NewString()); err != nil {
		t.Fatalf("parse unknown id: %v", err)
	}
	applied, exists, err = AcknowledgeAlert(ctx, pool, unknown)
	if err != nil {
		t.Fatalf("unknown AcknowledgeAlert: %v", err)
	}
	if applied || exists {
		t.Fatalf("unknown id = (applied %v, exists %v), want (false, false)", applied, exists)
	}
}
