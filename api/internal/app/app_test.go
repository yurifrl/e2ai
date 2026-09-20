// Tests for the app package: registry loading/validation (pure) and the
// migration + seeding lifecycle against a real scratch Postgres. Database
// tests are skipped unless E2AI_TEST_DATABASE_URL is set; CI/dev uses an
// ephemeral `docker run postgres:16-alpine`.
package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// registryDir resolves the repo-root registry/ directory from the test cwd
// (package dir = api/internal/app).
func registryDir(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", "registry"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestLoadRegistry(t *testing.T) {
	apps, err := LoadRegistry(registryDir(t))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("want 1 app, got %d: %+v", len(apps), apps)
	}
	a := apps[0]
	want := App{
		Owner:               "NSXBet",
		Repo:                "bet-app",
		WorkflowName:        "Release - Web",
		WorkflowPath:        ".github/workflows/remote_workflow_betweb.yaml",
		Environment:         "staging",
		DisplayTitlePattern: `[BetWeb] Deploy to staging`,
		HealthURL:           "https://stg-bet-app.nsx.dev/healthz",
		Surfaces:            []string{"web"},
		WebhookSecretRef:    "op://SRE Team/e2ai/github_webhook_secret",
		CommentMarker:       "<!-- e2ai:run:bet-app-web -->",
	}
	if a.Owner != want.Owner || a.Repo != want.Repo ||
		a.WorkflowName != want.WorkflowName || a.WorkflowPath != want.WorkflowPath ||
		a.Environment != want.Environment || a.DisplayTitlePattern != want.DisplayTitlePattern ||
		a.HealthURL != want.HealthURL || a.WebhookSecretRef != want.WebhookSecretRef ||
		a.CommentMarker != want.CommentMarker {
		t.Errorf("parsed app mismatch:\n got %+v\nwant %+v", a, want)
	}
	if len(a.Surfaces) != 1 || a.Surfaces[0] != "web" {
		t.Errorf("surfaces: got %v, want [web]", a.Surfaces)
	}
	if a.PlansSource != nil || a.CorpusRoot != nil {
		t.Errorf("future-phase fields must be nil when absent, got %+v/%+v", a.PlansSource, a.CorpusRoot)
	}
}

func TestLoadRegistryRejects(t *testing.T) {
	dir := t.TempDir()

	// Missing required field.
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("incomplete.yaml", "owner: NSXBet\nrepo: bet-app\n")
	if _, err := LoadRegistry(dir); err == nil {
		t.Error("incomplete yaml must fail validation")
	}

	// Plaintext webhook secret instead of an op:// ref (SECR-01).
	write("plaintext.yaml", "owner: o\nrepo: r\nworkflow_name: W\nworkflow_path: p\n"+
		"environment: staging\ndisplay_title_pattern: x\nwebhook_secret_ref: hunter2\n"+
		"comment_marker: m\n")
	_, err := LoadRegistry(dir)
	if err == nil {
		t.Fatal("plaintext webhook secret must be rejected")
	}
}

// testPool dials the E2AI_TEST_DATABASE_URL scratch database, skipping the
// test when no database is configured.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("E2AI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("E2AI_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("dial test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrateSeedLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// Start from a clean scratch database so the whole up/down cycle runs.
	if err := Down(ctx, pool); err != nil {
		t.Fatalf("down before test: %v", err)
	}

	if err := Migrate(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("river migrate: %v", err)
	}

	// Boot-twice idempotency: migrations re-run are a no-op.
	if err := Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate up twice: %v", err)
	}
	if err := MigrateRiver(ctx, pool); err != nil {
		t.Fatalf("river migrate twice: %v", err)
	}

	apps, err := LoadRegistry(registryDir(t))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}

	count, err := SeedApps(ctx, pool, apps)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if count != 1 {
		t.Fatalf("after first seed want 1 registered app, got %d", count)
	}

	var id1, createdAt1 string
	if err := pool.QueryRow(ctx,
		`select id::text, created_at::text from registered_apps`).Scan(&id1, &createdAt1); err != nil {
		t.Fatal(err)
	}

	// Second boot seeds the same registry: still exactly one row, same id,
	// same created_at (config fields follow the file, identity is stable).
	count, err = SeedApps(ctx, pool, apps)
	if err != nil {
		t.Fatalf("reseed: %v", err)
	}
	if count != 1 {
		t.Fatalf("after reseed want 1 registered app, got %d", count)
	}
	var id2, createdAt2 string
	if err := pool.QueryRow(ctx,
		`select id::text, created_at::text from registered_apps`).Scan(&id2, &createdAt2); err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("reseed changed app id: %s -> %s", id1, id2)
	}
	if createdAt1 != createdAt2 {
		t.Errorf("reseed changed created_at: %s -> %s", createdAt1, createdAt2)
	}

	// Re-seed with an updated config field: the field follows the file.
	apps[0].HealthURL = "https://stg-bet-app.nsx.dev/healthz2"
	if _, err := SeedApps(ctx, pool, apps); err != nil {
		t.Fatalf("seed updated: %v", err)
	}
	var healthURL string
	if err := pool.QueryRow(ctx,
		`select health_url from registered_apps`).Scan(&healthURL); err != nil {
		t.Fatal(err)
	}
	if healthURL != "https://stg-bet-app.nsx.dev/healthz2" {
		t.Errorf("updated config field did not follow the file: %q", healthURL)
	}

	// Down + Migrate again: fresh cycle must succeed (migration up/down on a
	// scratch postgres), and River's tables coexist untouched.
	if err := Down(ctx, pool); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := Migrate(ctx, pool, nil); err != nil {
		t.Fatalf("migrate after down: %v", err)
	}
	var riverTables int
	if err := pool.QueryRow(ctx,
		`select count(*) from information_schema.tables where table_name like 'river\_%' escape '\'`).
		Scan(&riverTables); err != nil {
		t.Fatal(err)
	}
	if riverTables == 0 {
		t.Error("river tables missing after Down; river migrations should coexist")
	}
}
