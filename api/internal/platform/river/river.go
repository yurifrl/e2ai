// Package river builds River clients bound to a pgx pool. Queues are
// per-process: the api container drains only the "default" queue, the runner
// container drains only "runner" (mirror of the incidents-admin jobs factory).
// Process binaries go through this package only — the riverqueue library stays
// an implementation detail here.
package river

import (
	"fmt"
	"time"

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

// Workers bundles job workers registered on a client.
type Workers = river.Workers

// JobArgs aliases River's job-args interface for call sites.
type JobArgs = river.JobArgs

// PeriodicJob aliases River's periodic job for call sites.
type PeriodicJob = river.PeriodicJob

// NewWorkers returns an empty worker bundle for WithWorkers.
func NewWorkers() *Workers { return river.NewWorkers() }

// AddWorker registers a worker on a bundle, keyed by its job args type.
func AddWorker[T JobArgs](workers *Workers, worker river.Worker[T]) {
	river.AddWorker(workers, worker)
}

// NewPeriodicJob builds one periodic job: insert the constructor's args every
// interval. runOnStart fires once immediately at client start.
func NewPeriodicJob(interval time.Duration, constructor func() JobArgs, runOnStart bool) *PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) { return constructor(), nil },
		&river.PeriodicJobOpts{RunOnStart: runOnStart},
	)
}

// DefaultClient builds the api-process River client: the "default" queue only.
// Unknown-kind insert validation stays off: the api client enqueues the whole
// job chain (and runner-queue jobs worked by a different process), so kinds
// whose workers aren't registered yet (or register elsewhere) must insert.
// Worker/periodic wiring is supplied by main.go through the options below.
func DefaultClient(pool *pgxpool.Pool, maxWorkers int, opts ...Option) (*Client, error) {
	return newClient(pool, map[string]river.QueueConfig{
		QueueDefault: {MaxWorkers: maxWorkers},
	}, opts...)
}

// RunnerClient builds the runner-process River client: the "runner" queue only.
// Worker wiring is supplied by main.go through the options (WithWorkers).
func RunnerClient(pool *pgxpool.Pool, maxWorkers int, opts ...Option) (*Client, error) {
	return newClient(pool, map[string]river.QueueConfig{
		QueueRunner: {MaxWorkers: maxWorkers},
	}, opts...)
}

// Option customizes the River client config without exposing raw river types
// at call sites.
type Option func(*river.Config)

// WithWorkers registers job workers on the client.
func WithWorkers(workers *river.Workers) Option {
	return func(cfg *river.Config) { cfg.Workers = workers }
}

// WithPeriodicJobs schedules River periodic jobs (e.g. the reconcile sweep).
func WithPeriodicJobs(jobs ...*river.PeriodicJob) Option {
	return func(cfg *river.Config) { cfg.PeriodicJobs = append(cfg.PeriodicJobs, jobs...) }
}

// WithQueues overrides the process's queue map (tests use it to add the
// "runner" queue so execute-handoff inserts validate end to end).
func WithQueues(queues map[string]river.QueueConfig) Option {
	return func(cfg *river.Config) { cfg.Queues = queues }
}

func newClient(pool *pgxpool.Pool, queues map[string]river.QueueConfig, opts ...Option) (*Client, error) {
	cfg := &river.Config{
		Queues:              queues,
		Workers:             river.NewWorkers(),
		SkipUnknownJobCheck: true,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return nil, fmt.Errorf("create river client: %w", err)
	}
	return client, nil
}
