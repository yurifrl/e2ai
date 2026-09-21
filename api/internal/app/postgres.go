// Package app owns the e2ai application database lifecycle: schema migrations
// and app-registry seeding. Migrations are plain embedded SQL files under
// migrations/ (name = NNNN_desc.sql), tracked in the app_migrations table.
// River's queue schema is migrated separately by River's bundled migrations —
// the two systems share a database but own disjoint table namespaces.
package app

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migration tables owned by this package, in drop order for Down (children
// before parents; app_migrations last).
var appTables = []string{
	"webhook_triage", "test_quarantine", // phase-5 hardening schema (0007)
	"pr_comments", "artifacts", "test_results", "corpus_learnings",
	"session_steps", "agent_sessions", // phase-3 agent schema (0004)
	"runs",
	"plan_approvals", "plan_versions", "plans", "corpus_behaviors",
	"run_plan_resolutions", "webhook_events", "registered_apps", "app_migrations",
}

// Migrate applies all pending embedded migrations in filename order, each in
// its own transaction recorded in app_migrations. Re-running on an already
// migrated database is a no-op (boot-twice idempotent).
func Migrate(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	if _, err := pool.Exec(ctx, `
		create table if not exists app_migrations (
			version int primary key,
			name text not null,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return fmt.Errorf("create app_migrations: %w", err)
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}

	files, err := migrationFiles()
	if err != nil {
		return err
	}
	for _, m := range files {
		if applied[m.version] {
			continue
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %04d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %04d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx,
			`insert into app_migrations (version, name) values ($1, $2)`,
			m.version, m.name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %04d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %04d: %w", m.version, err)
		}
		if logger != nil {
			logger.Info("applied migration", "version", m.version, "name", m.name)
		}
	}
	return nil
}

// Down drops every e2ai application table (scratch-database teardown; River
// tables are untouched). Fresh Migrate/Down/Migrate cycles must all succeed.
func Down(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range appTables {
		if _, err := pool.Exec(ctx, `drop table if exists `+table+` cascade`); err != nil {
			return fmt.Errorf("drop %s: %w", table, err)
		}
	}
	return nil
}

// MigrateRiver runs River's bundled migrations (river_* tables) so the queue
// can start on the same database. Idempotent: applied versions are skipped.
func MigrateRiver(ctx context.Context, pool *pgxpool.Pool) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{})
	if err != nil {
		return fmt.Errorf("create river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate: %w", err)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrationFiles() ([]migration, error) {
	entries, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(entries)
	migrations := make([]migration, 0, len(entries))
	for _, path := range entries {
		version, name, err := parseMigrationName(strings.TrimPrefix(path, "migrations/"))
		if err != nil {
			return nil, err
		}
		sqlBytes, err := fs.ReadFile(migrationsFS, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		migrations = append(migrations, migration{version: version, name: name, sql: string(sqlBytes)})
	}
	return migrations, nil
}

func parseMigrationName(fileName string) (int, string, error) {
	trimmed := strings.TrimSuffix(fileName, ".sql")
	dot := strings.Index(trimmed, "_")
	if dot < 1 { // "NNNN" at minimum, non-empty version
		return 0, "", fmt.Errorf("migration file %q: want NNNN_name.sql", fileName)
	}
	version, err := strconv.Atoi(trimmed[:dot])
	if err != nil {
		return 0, "", fmt.Errorf("migration file %q: bad version: %w", fileName, err)
	}
	return version, trimmed[dot+1:], nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int]bool, error) {
	rows, err := pool.Query(ctx, `select version from app_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read app_migrations: %w", err)
	}
	defer rows.Close()
	applied := map[int]bool{}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan app_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate app_migrations: %w", err)
	}
	return applied, nil
}
