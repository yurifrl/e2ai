// Service couples the pool-backed cockpit domain operations with their HTTP
// surface. Constructed with NewService and mounted once in cmd/e2ai-api's
// mux; the nil-pool posture mirrors the other surfaces — unmounted while
// postgres is degraded.
package cockpit

import (
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Service is the cockpit surface: state reads (RunViewPermission) and
// attributed mutations (RunEditPermission).
type Service struct {
	Pool *pgxpool.Pool

	// FallbackUser is the api binary's configured dev-fallback identity
	// (cfg.DevFallbackUser). Mutations attributed to it are refused at the
	// boundary — the approvals pattern (approvals/http.go:128).
	FallbackUser string

	Logger *slog.Logger
}

// NewService builds the cockpit surface over the shared postgres pool.
func NewService(pool *pgxpool.Pool, fallbackUser string, logger *slog.Logger) *Service {
	return &Service{Pool: pool, FallbackUser: fallbackUser, Logger: logger}
}
