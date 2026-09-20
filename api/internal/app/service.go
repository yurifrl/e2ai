// Registry seeding (APPS-01/02): on boot, every registry/*.yaml file is loaded,
// validated, and upserted into registered_apps keyed by
// (owner, repo, workflow_name, environment). Idempotent: re-seeding the same
// registry never produces duplicate rows.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/jackc/pgx/v5/pgxpool"
)

// App mirrors a registry/*.yaml file; field names match the YAML keys and the
// registered_apps columns one-to-one. Secrets are refs only (SECR-01).
type App struct {
	Owner               string   `yaml:"owner"`
	Repo                string   `yaml:"repo"`
	WorkflowName        string   `yaml:"workflow_name"`
	WorkflowPath        string   `yaml:"workflow_path"`
	Environment         string   `yaml:"environment"`
	DisplayTitlePattern string   `yaml:"display_title_pattern"`
	HealthURL           string   `yaml:"health_url"`
	Surfaces            []string `yaml:"surfaces"`
	// Future phases (reserved columns): nil when absent from the file.
	PlansSource      *string `yaml:"plans_source"`
	CorpusRoot       *string `yaml:"corpus_root"`
	WebhookSecretRef string  `yaml:"webhook_secret_ref"`
	CommentMarker    string  `yaml:"comment_marker"`
}

// LoadRegistry parses every *.yaml file in dir into validated App values.
// A malformed file is a hard error — config-as-code means boot fails loudly
// instead of silently running with a half registry.
func LoadRegistry(dir string) ([]App, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read registry dir %s: %w", dir, err)
	}
	apps := make([]App, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.Name(), err)
		}
		var app App
		if err := yaml.Unmarshal(data, &app); err != nil {
			return nil, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if err := app.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		apps = append(apps, app)
	}
	return apps, nil
}

func (a App) validate() error {
	required := [][2]string{
		{"owner", a.Owner}, {"repo", a.Repo},
		{"workflow_name", a.WorkflowName}, {"workflow_path", a.WorkflowPath},
		{"environment", a.Environment}, {"display_title_pattern", a.DisplayTitlePattern},
		{"webhook_secret_ref", a.WebhookSecretRef}, {"comment_marker", a.CommentMarker},
	}
	for _, kv := range required {
		if kv[1] == "" {
			return fmt.Errorf("missing required field %s", kv[0])
		}
	}
	if !strings.HasPrefix(a.WebhookSecretRef, "op://") {
		return fmt.Errorf("webhook_secret_ref must be an op:// ref (plaintext secrets are never stored in the registry)")
	}
	return nil
}

// SeedApps upserts the registry into registered_apps. Config fields follow the
// file on re-seed; id and created_at are stable across boots. Returns the row
// count now registered (after upsert), so boot-twice stays one row per app.
func SeedApps(ctx context.Context, pool *pgxpool.Pool, apps []App) (int, error) {
	for _, app := range apps {
		if _, err := pool.Exec(ctx, `
			insert into registered_apps
				(owner, repo, workflow_name, workflow_path, environment,
				 display_title_pattern, health_url, surfaces,
				 plans_source, corpus_root, webhook_secret_ref, comment_marker)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			on conflict (owner, repo, workflow_name, environment) do update set
				workflow_path = excluded.workflow_path,
				display_title_pattern = excluded.display_title_pattern,
				health_url = excluded.health_url,
				surfaces = excluded.surfaces,
				plans_source = excluded.plans_source,
				corpus_root = excluded.corpus_root,
				webhook_secret_ref = excluded.webhook_secret_ref,
				comment_marker = excluded.comment_marker
		`, app.Owner, app.Repo, app.WorkflowName, app.WorkflowPath, app.Environment,
			app.DisplayTitlePattern, app.HealthURL, app.Surfaces,
			app.PlansSource, app.CorpusRoot, app.WebhookSecretRef, app.CommentMarker); err != nil {
			return 0, fmt.Errorf("upsert app %s/%s %q (%s): %w",
				app.Owner, app.Repo, app.WorkflowName, app.Environment, err)
		}
	}
	if len(apps) == 0 {
		return 0, nil
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from registered_apps`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count registered_apps: %w", err)
	}
	return count, nil
}

// Bootstrap is the api boot path: schema up (app migrations, then River's),
// then registry seeding. pool must be non-nil.
func Bootstrap(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, registryDir string) error {
	if err := Migrate(ctx, pool, logger); err != nil {
		return fmt.Errorf("app migrations: %w", err)
	}
	if err := MigrateRiver(ctx, pool); err != nil {
		return fmt.Errorf("river migrations: %w", err)
	}
	apps, err := LoadRegistry(registryDir)
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	count, err := SeedApps(ctx, pool, apps)
	if err != nil {
		return fmt.Errorf("seed registry: %w", err)
	}
	if logger != nil {
		logger.Info("registry seeded", "apps", count, "dir", registryDir)
	}
	return nil
}
