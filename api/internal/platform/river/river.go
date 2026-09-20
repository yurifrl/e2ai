// Package river builds River clients bound to a pgx pool. Queues are
// per-process: the api container drains only the "default" queue, the runner
// container drains only "runner" (mirror of the incidents-admin jobs factory).
// Process binaries go through this package only — the riverqueue library stays
// an implementation detail here.
package river

import (
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Queue names. Jobs publish onto these; each process subscribes to exactly one.
const (
	QueueDefault = "default"
	QueueRunner  = "runner"
)

// Client is the concrete River client type every e2ai process holds.
type Client = river.Client[pgx.Tx]

// DefaultClient builds the api-process River client: the "default" queue only.
// Workers are empty at scaffold time (Wave 1 registers job args).
func DefaultClient(pool *pgxpool.Pool, maxWorkers int) (*Client, error) {
	return newClient(pool, map[string]river.QueueConfig{
		QueueDefault: {MaxWorkers: maxWorkers},
	})
}

// RunnerClient builds the runner-process River client: the "runner" queue only.
func RunnerClient(pool *pgxpool.Pool, maxWorkers int) (*Client, error) {
	return newClient(pool, map[string]river.QueueConfig{
		QueueRunner: {MaxWorkers: maxWorkers},
	})
}

func newClient(pool *pgxpool.Pool, queues map[string]river.QueueConfig) (*Client, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Queues:  queues,
		Workers: river.NewWorkers(),
	})
	if err != nil {
		return nil, fmt.Errorf("create river client: %w", err)
	}
	return client, nil
}
