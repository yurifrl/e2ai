// Package postgres owns the pgx connection pool lifecycle.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgx pool with lifecycle helpers.
type Pool struct {
	*pgxpool.Pool
}

// Open parses the database URL and establishes the pool, failing fast on an
// unreachable database (callers that must tolerate an absent DB use
// OpenWithRetry instead).
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping pool: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// OpenWithRetry connects with a bounded retry budget. Local dev starts tilt
// pods and bare processes on their own schedule, so a missing postgres is a
// degraded start (healthz reports it), never a crash loop.
func OpenWithRetry(ctx context.Context, logger *slog.Logger, databaseURL string, attempts int, delay time.Duration) (*Pool, error) {
	var pool *Pool
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		pool, lastErr = Open(ctx, databaseURL)
		if lastErr == nil {
			return pool, nil
		}
		logger.Warn("postgres unavailable", "attempt", attempt, "of", attempts, "error", lastErr)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// Healthy reports whether the database answers a ping right now.
func (p *Pool) Healthy(ctx context.Context) bool {
	if p == nil || p.Pool == nil {
		return false
	}
	return p.Ping(ctx) == nil
}
