// Package jobs wires the read-only sync workers (spec §7) onto river's
// periodic scheduler. It owns only the scheduling and dispatch: each
// handler is supplied by the caller and is responsible for its own
// job_runs bookkeeping (spec §7's "every job must record its outcome"),
// so the same function backs both `ic serve`'s periodic run and the
// matching one-shot `ic <job>` subcommand.
//
// Intervals are fixed for Phase 1. The cron settings in spec §4.2 drive
// the Phase 4 pairing scheduler, not these sync workers, and wall-clock
// alignment ("nightly", "daily") arrives with that scheduler; a plain
// interval from process start is enough to keep standings fresh now.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Handler runs one periodic job to completion. It receives river's own
// job ID so the handler can record it on the job_runs row
// (job_runs.river_job_id), tying the persisted outcome back to the row
// in river's queue tables.
type Handler func(ctx context.Context, riverJobID int64) error

// Handlers is the set of background jobs `serve` runs.
type Handlers struct {
	SyncGames      Handler
	RefreshRatings Handler
	Recompute      Handler
}

// Job argument types. They carry no data — each job operates on the
// whole league — but river needs a distinct kind per worker.
type (
	syncGamesArgs      struct{}
	refreshRatingsArgs struct{}
	recomputeArgs      struct{}
)

func (syncGamesArgs) Kind() string      { return "sync-games" }
func (refreshRatingsArgs) Kind() string { return "refresh-ratings" }
func (recomputeArgs) Kind() string      { return "recompute-aggregates" }

// NewClient builds the river client `serve` runs: one queue, one worker
// at a time (Lichess wants a single request at a time — spec §3.4), and
// the three periodic jobs. The caller starts and stops it.
func NewClient(pool *pgxpool.Pool, h Handlers, logger *slog.Logger) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, workerFor(h.SyncGames, syncGamesArgs{}))
	river.AddWorker(workers, workerFor(h.RefreshRatings, refreshRatingsArgs{}))
	river.AddWorker(workers, workerFor(h.Recompute, recomputeArgs{}))

	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:      logger,
		MaxAttempts: 3,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			periodic(time.Hour, syncGamesArgs{}),
			periodic(24*time.Hour, refreshRatingsArgs{}),
			periodic(24*time.Hour, recomputeArgs{}),
		},
	})
}

func workerFor[T river.JobArgs](run Handler, _ T) river.Worker[T] {
	return river.WorkFunc(func(ctx context.Context, job *river.Job[T]) error {
		return run(ctx, job.ID)
	})
}

func periodic[T river.JobArgs](every time.Duration, args T) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(every),
		func() (river.JobArgs, *river.InsertOpts) { return args, nil },
		&river.PeriodicJobOpts{RunOnStart: false},
	)
}
