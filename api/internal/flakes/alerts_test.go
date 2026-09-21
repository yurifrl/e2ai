// Alert-store tests (Phase 5 T4): the cooldown dedupe is the whole point of
// the alert store — a persistently crossed threshold must fire once per
// window, and concurrent ticks must not double-fire.
package flakes

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

func seedApp(t *testing.T, pool *pgxpool.Pool) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	err := pool.QueryRow(context.Background(), `
		insert into registered_apps
			(owner, repo, workflow_name, workflow_path, environment,
			 display_title_pattern, webhook_secret_ref, comment_marker)
		values ('yuri', $1, 'e2ai', '.github/workflows/e2ai.yml', 'prod',
			 '*', 'ref', 'marker')
		returning id
	`, uuid.NewString()).Scan(&id)
	if err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return id
}

func TestInsertFlakeThresholdAlertDedupe(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	appID := seedApp(t, pool)

	fired, err := InsertFlakeThresholdAlert(ctx, pool, appID, []byte(`{"threshold":0.25,"max_rate":0.8}`))
	if err != nil {
		t.Fatalf("first InsertFlakeThresholdAlert: %v", err)
	}
	if !fired {
		t.Fatal("first crossing must fire the alert")
	}

	fired, err = InsertFlakeThresholdAlert(ctx, pool, appID, []byte(`{"threshold":0.25,"max_rate":0.9}`))
	if err != nil {
		t.Fatalf("second InsertFlakeThresholdAlert: %v", err)
	}
	if fired {
		t.Fatal("second crossing inside the cooldown must be suppressed")
	}

	var n int
	if err := pool.QueryRow(ctx,
		`select count(*) from alert_events where app_id = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if n != 1 {
		t.Fatalf("alert rows = %d, want exactly 1", n)
	}

	// Independent apps have independent cooldowns.
	other := seedApp(t, pool)
	fired, err = InsertFlakeThresholdAlert(ctx, pool, other, []byte(`{}`))
	if err != nil {
		t.Fatalf("other app InsertFlakeThresholdAlert: %v", err)
	}
	if !fired {
		t.Fatal("a different app must get its own alert")
	}
}
