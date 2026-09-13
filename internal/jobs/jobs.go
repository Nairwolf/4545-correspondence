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
//
// A job that overruns JobTimeout has its context cancelled; the handler
// records the run as failed with that reason and river retries it up to
// MaxAttempts. Shutdown is soft: cancelling the context passed to Start
// gives a running job softStopTimeout to finish before its own context
// is cancelled, and it still records an outcome either way.
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

// JobTimeout bounds a single run. River's own default is one minute,
// which a real sync-games run can exceed without anything being wrong:
// a 429 costs a 60s wait on its own, and a full 300-id batch streams for
// 10-15s at Lichess's 20-30 games/s, once per white player with an
// unmatched pairing. Fifteen minutes is the worst plausible run with
// headroom; past it something is genuinely stuck and the run should be
// recorded as failed rather than held open. It is exported so serve's
// startup sweep can explain itself in the same terms.
const JobTimeout = 15 * time.Minute

// softStopTimeout is the grace a running job gets when the context
// passed to Start is cancelled (serve's SIGTERM). Without it river
// cancels the work context immediately, which is StopAndCancel in all
// but name. It must stay well under serve's 30s shutdown deadline: the
// grace runs concurrently with the HTTP drain, and what follows it — the
// job's context being cancelled, its outcome row written, Stop
// returning — has to fit in what's left.
const softStopTimeout = 20 * time.Second

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
// the three periodic jobs. The caller starts and stops it. Runs are
// bounded by JobTimeout and shutdown is soft (see both constants);
// river derives its own stuck-job rescue window from JobTimeout, so
// RescueStuckJobsAfter needs no separate setting.
func NewClient(pool *pgxpool.Pool, h Handlers, logger *slog.Logger) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	river.AddWorker(workers, workerFor(h.SyncGames, syncGamesArgs{}))
	river.AddWorker(workers, workerFor(h.RefreshRatings, refreshRatingsArgs{}))
	river.AddWorker(workers, workerFor(h.Recompute, recomputeArgs{}))

	return river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:          logger,
		MaxAttempts:     3,
		JobTimeout:      JobTimeout,
		SoftStopTimeout: softStopTimeout,
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
