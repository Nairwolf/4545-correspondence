// Package jobs wires the read-only sync workers (spec §7) onto river's
// periodic scheduler. It owns only the scheduling and dispatch: each
// handler is supplied by the caller and is responsible for its own
// job_runs bookkeeping (spec §7's "every job must record its outcome"),
// so the same function backs both `ic serve`'s periodic run and the
// matching one-shot `ic <job>` subcommand.
//
// The sync workers run on plain intervals from process start, which is
// enough to keep standings fresh. Only round generation needs the
// calendar, and it is the one job driven by a cron expression from the
// settings (pairing.cron, spec §4.2).
//
// A job that overruns JobTimeout has its context cancelled; the handler
// records the run as failed with that reason and river retries it up to
// MaxAttempts. Shutdown is soft: cancelling the context passed to Start
// gives a running job softStopTimeout to finish before its own context
// is cancelled, and it still records an outcome either way.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/robfig/cron/v3"
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

// RoundHandler is a Handler for the one job that acts on a single round
// rather than the whole league.
type RoundHandler func(ctx context.Context, roundID int32, riverJobID int64) error

// Handlers is the set of background jobs `serve` runs.
type Handlers struct {
	SyncGames      Handler
	RefreshRatings Handler
	Recompute      Handler
	// GenerateRound pairs the week's round on pairing.cron (spec §6.1).
	GenerateRound Handler
	// PublishRound ends one draft's review window, scheduled for that
	// draft's publish_at.
	PublishRound RoundHandler
	// PublishSweep publishes any draft whose window has passed — the
	// hourly safety net of spec §7.
	PublishSweep Handler
}

// Job argument types. Most carry no data — the job operates on the
// whole league — but river needs a distinct kind per worker.
type (
	syncGamesArgs      struct{}
	refreshRatingsArgs struct{}
	recomputeArgs      struct{}
	generateRoundArgs  struct{}
	publishSweepArgs   struct{}
)

// PublishRoundArgs names the draft to publish. PublishAt is part of the
// args, and so part of river's uniqueness hash, on purpose: river
// counts a COMPLETED job with the same args as a duplicate, so without
// it a round whose window moved could never be re-scheduled.
type PublishRoundArgs struct {
	RoundID   int32     `json:"round_id"`
	PublishAt time.Time `json:"publish_at"`
}

func (syncGamesArgs) Kind() string      { return "sync-games" }
func (refreshRatingsArgs) Kind() string { return "refresh-ratings" }
func (recomputeArgs) Kind() string      { return "recompute-aggregates" }
func (generateRoundArgs) Kind() string  { return "generate-round" }
func (publishSweepArgs) Kind() string   { return "publish-round-sweep" }
func (PublishRoundArgs) Kind() string   { return "publish-round" }

// Scheduler enqueues a draft's publish-round job in the transaction
// that created the draft, so a round and its publication are committed
// together or not at all. It is what internal/rounds calls through its
// own Scheduler interface.
type Scheduler struct {
	client *river.Client[pgx.Tx]
}

func NewScheduler(client *river.Client[pgx.Tx]) *Scheduler {
	return &Scheduler{client: client}
}

func (s *Scheduler) SchedulePublish(ctx context.Context, tx pgx.Tx, roundID int32, publishAt time.Time) error {
	_, err := s.client.InsertTx(ctx, tx,
		PublishRoundArgs{RoundID: roundID, PublishAt: publishAt.UTC()},
		&river.InsertOpts{
			ScheduledAt: publishAt,
			UniqueOpts:  river.UniqueOpts{ByArgs: true},
		},
	)
	if err != nil {
		return fmt.Errorf("enqueue publish-round for round %d: %w", roundID, err)
	}
	return nil
}

// NewClient builds the river client `serve` runs: one queue, one worker
// at a time (Lichess wants a single request at a time — spec §3.4), and
// the periodic jobs. pairingCron is the pairing.cron setting; it is
// read once here, so changing it takes a restart until the Phase 6
// settings UI can add and remove periodic jobs at runtime. The caller
// starts and stops the client. Runs are bounded by JobTimeout and
// shutdown is soft (see both constants); river derives its own
// stuck-job rescue window from JobTimeout, so RescueStuckJobsAfter
// needs no separate setting.
func NewClient(
	pool *pgxpool.Pool,
	h Handlers,
	pairingCron string,
	logger *slog.Logger,
) (*river.Client[pgx.Tx], error) {
	// The first wall-clock schedule in the codebase: the other jobs run
	// on an interval from process start, but a league that pairs "every
	// Monday at noon" needs the calendar. River does not catch up a
	// firing missed while the binary was down — §6.1's manual
	// generation covers that.
	schedule, err := cron.ParseStandard(pairingCron)
	if err != nil {
		return nil, fmt.Errorf("parse pairing.cron %q: %w", pairingCron, err)
	}

	workers := river.NewWorkers()
	river.AddWorker(workers, workerFor(h.SyncGames, syncGamesArgs{}))
	river.AddWorker(workers, workerFor(h.RefreshRatings, refreshRatingsArgs{}))
	river.AddWorker(workers, workerFor(h.Recompute, recomputeArgs{}))
	river.AddWorker(workers, workerFor(h.GenerateRound, generateRoundArgs{}))
	river.AddWorker(workers, workerFor(h.PublishSweep, publishSweepArgs{}))
	river.AddWorker(workers, river.WorkFunc(func(ctx context.Context, job *river.Job[PublishRoundArgs]) error {
		return h.PublishRound(ctx, job.Args.RoundID, job.ID)
	}))

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
			periodic(time.Hour, publishSweepArgs{}),
			river.NewPeriodicJob(
				schedule,
				func() (river.JobArgs, *river.InsertOpts) { return generateRoundArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: false},
			),
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
