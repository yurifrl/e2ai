// Package config loads application configuration from environment variables.
// It provides the boundary between deployment environment and application code.
package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all application configuration.
// Populated from environment variables with documented defaults.
type Config struct {
	// Core
	Port        int
	DatabaseURL string
	Debug       bool
	LogLevel    string

	// Auth: BFF-injected identity fallback for local dev (httpx middleware).
	DevFallbackUser string

	// Registry directory of app YAML definitions seeded on boot (APPS-01/02).
	RegistryDir string

	// Runner queue sizing for the river "runner" queue (cmd/e2ai-runner).
	RunnerMaxWorkers int
}

// LoadDotEnv layers env files base -> $ENV -> per-user (later wins), the exact
// semantics of the Tiltfile's `set -a; . .env; . .env.$ENV; . .env.$(whoami).local`.
// Missing files are skipped silently so a bare `go run` still works.
func LoadDotEnv() {
	root := repoRoot()
	layers := []string{filepath.Join(root, ".env")}
	if env := os.Getenv("ENV"); env != "" {
		layers = append(layers, filepath.Join(root, ".env."+env))
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		layers = append(layers, filepath.Join(root, ".env."+u.Username+".local"))
	}
	for _, path := range layers {
		kv, err := godotenv.Read(path)
		if err != nil {
			continue // layer absent; the next one wins if present
		}
		for key, value := range kv {
			_ = os.Setenv(key, value) // later layers override (set -a semantics)
		}
	}
}

// Load builds the Config from the process environment.
func Load() (*Config, error) {
	port, err := getEnvInt("PORT", 8082)
	if err != nil {
		return nil, err
	}
	runnerMaxWorkers, err := getEnvInt("RUNNER_MAX_WORKERS", 1)
	if err != nil {
		return nil, err
	}
	logLevel := strings.ToLower(getEnv("LOG_LEVEL", "info"))
	if logLevel != "debug" && logLevel != "info" && logLevel != "warn" && logLevel != "error" {
		return nil, fmt.Errorf("invalid LOG_LEVEL %q (want debug|info|warn|error)", logLevel)
	}
	return &Config{
		Port:             port,
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://e2ai:e2ai@localhost:5434/e2ai?sslmode=disable"),
		Debug:            getEnvBool("DEBUG", false),
		LogLevel:         logLevel,
		DevFallbackUser:  getEnv("DEV_FALLBACK_USER", ""),
		RegistryDir:      getEnv("REGISTRY_DIR", filepath.Join(repoRoot(), "registry")),
		RunnerMaxWorkers: runnerMaxWorkers,
	}, nil
}

func getEnv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q as int: %w", key, value, err)
	}
	return parsed, nil
}

func getEnvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

// getEnvDuration parses a duration env var with default (reserved for future
// stages: rollout budgets, job deadlines).
func getEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s=%q as duration: %w", key, value, err)
	}
	return parsed, nil
}

// repoRoot walks up from cwd to the directory containing .env (the repo root),
// so binaries work from any working directory depth.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".env")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "." // no .env anywhere up the tree; repoRoot is cwd
		}
		dir = parent
	}
}
