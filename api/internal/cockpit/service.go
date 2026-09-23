// Service couples the pool-backed cockpit domain operations with their HTTP
// surface. Constructed with NewService and mounted once in cmd/e2ai-api's
// mux; the nil-pool posture mirrors the other surfaces — unmounted while
// postgres is degraded.
package cockpit

import (
	"log/slog"

	"github.com/NSXBet/e2ai/api/internal/ingest"
	"github.com/NSXBet/e2ai/api/internal/platform/river"
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

	// Queue enqueues the runner-queue execute job inside the trigger
	// transaction (06-02). Inert (and triggers refused) when nil.
	Queue *river.Client

	// Heads resolves the app's default-branch head for retest triggers;
	// a nil Heads refuses fail-closed at trigger time.
	Heads HeadResolver

	// Store is the transactional ingest seam the simulate boundary drives
	// (CONTEXT decision 2: simulate = the real pipeline through its own
	// ingest); a nil Store disables simulate with an explicit 503.
	Store ingest.IngestStore
	// Secrets resolves the app's webhook secret from the same in-memory
	// map the ingest Handler built at boot; a nil Secrets (or an empty
	// result) refuses simulate fail-closed.
	Secrets SecretResolver

	Logger *slog.Logger
}

// NewService builds the cockpit surface over the shared postgres pool.
func NewService(pool *pgxpool.Pool, fallbackUser string, logger *slog.Logger, queue *river.Client, heads HeadResolver, store ingest.IngestStore, secrets SecretResolver) *Service {
	return &Service{
		Pool:         pool,
		FallbackUser: fallbackUser,
		Queue:        queue,
		Heads:        heads,
		Store:        store,
		Secrets:      secrets,
		Logger:       logger,
	}
}
