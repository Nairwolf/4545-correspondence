// Package jobs wires the read-only sync workers (spec §7) onto river's
// periodic scheduler. It owns only the scheduling and dispatch: each
// handler is supplied by the caller and is responsible for its own
// job_runs bookkeeping (spec §7's "every job must record its outcome"),
// so the same function backs both `ic serve`'s periodic run and the
// matching one-shot `ic <job>` subcommand.
//
// The sync workers run on plain intervals from process start, which is
// enough to keep standings fresh. Round generation needs the calendar:
// it runs on a cron expression from the settings (pairing.cron, spec
// §4.2), and validate-tokens runs an hour before each generation.
//
// It also enqueues the one-off jobs the rest of the code asks for
// (Scheduler), in the caller's transaction: from serve's running client
// there, and from an insert-only client (NewInsertClient) in the
// one-shot `ic` commands, which have no job runner of their own.
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
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

// RoundHandler is a Handler for a job that acts on a single round
// rather than the whole league.
type RoundHandler func(ctx context.Context, roundID int32, riverJobID int64) error

// CreateGamesHandler runs create-games for one round. lastAttempt is
// true on the attempt after which river gives up: a run that fails
// then must give the round's games up to its players (spec §6.3).
type CreateGamesHandler func(ctx context.Context, args CreateGamesArgs, lastAttempt bool, riverJobID int64) error

// PairingHandler is a Handler for a job that acts on one pairing.
type PairingHandler func(ctx context.Context, pairingID pgtype.UUID, riverJobID int64) error

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
	// ValidateTokens tests every stored Lichess token an hour before
	// each generation (spec §7).
	ValidateTokens Handler
	// CreateGames creates a published round's games on Lichess (spec
	// §6.3), queued by its publication or an admin's retry.
	CreateGames CreateGamesHandler
	// CancelChallenge withdraws on Lichess the challenge of a pairing
	// that will never be played (spec §6.4), queued with the change
	// that retired it.
	CancelChallenge PairingHandler
}

// Job argument types. Most carry no data — the job operates on the
// whole league — but river needs a distinct kind per worker.
type (
	syncGamesArgs      struct{}
	refreshRatingsArgs struct{}
	recomputeArgs      struct{}
	generateRoundArgs  struct{}
	publishSweepArgs   struct{}
	validateTokensArgs struct{}
)

// PublishRoundArgs names the draft to publish. PublishAt is part of the
// args, and so part of river's uniqueness hash, on purpose: river
// counts a COMPLETED job with the same args as a duplicate, so without
// it a round whose window moved could never be re-scheduled.
type PublishRoundArgs struct {
	RoundID   int32     `json:"round_id"`
	PublishAt time.Time `json:"publish_at"`
}

// CreateGamesArgs names the round whose games to create. Only the
// round counts for uniqueness: one create-games job per round may wait
// or run at a time, whether queued by publication or by a retry.
type CreateGamesArgs struct {
	RoundID int32 `json:"round_id" river:"unique"`
	// Retry is an admin's retry, which first looks for games the
	// players have started by hand (spec §6.3 step 2).
	Retry bool `json:"retry"`
}

// CancelChallengeArgs names the pairing whose challenge to withdraw.
// A pairing's challenge is withdrawn once: river's default uniqueness,
// which also counts a completed job, is what is wanted.
type CancelChallengeArgs struct {
	PairingID pgtype.UUID `json:"pairing_id"`
}

// createGamesAttempts is create-games' retry budget (spec §6.3): river's
// back-off spreads ten attempts over about four hours, so a short
// Lichess outage is ridden out before the players are asked to start
// their games by hand.
const createGamesAttempts = 10

// createGamesUnique keeps a second create-games job for a round out
// while one waits or runs, and lets one in once the last has finished,
// for a retry. river's default would also count a completed job.
var createGamesUnique = river.UniqueOpts{
	ByArgs: true,
	ByState: []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	},
}

func (syncGamesArgs) Kind() string       { return "sync-games" }
func (refreshRatingsArgs) Kind() string  { return "refresh-ratings" }
func (recomputeArgs) Kind() string       { return "recompute-aggregates" }
func (generateRoundArgs) Kind() string   { return "generate-round" }
func (publishSweepArgs) Kind() string    { return "publish-round-sweep" }
func (PublishRoundArgs) Kind() string    { return "publish-round" }
func (validateTokensArgs) Kind() string  { return "validate-tokens" }
func (CreateGamesArgs) Kind() string     { return "create-games" }
func (CancelChallengeArgs) Kind() string { return "cancel-challenge" }

// Scheduler enqueues the jobs internal/rounds asks for, through its
// own Scheduler interface, in the caller's transaction: a draft's
// publish-round job with the draft, a round's create-games job with its
// publication or an admin's retry, the sync-games run that follows a
// creation, and the withdrawal of a challenge whose pairing was retired
// by evaluate-activity (internal/activity) or an admin. Each is
// committed with the change it follows from, or not at all. Every path that generates or publishes a round has one:
// the jobs and the web layer on serve's running client, the `ic`
// commands on an insert-only client (spec §7, §8.5).
type Scheduler struct {
	client *river.Client[pgx.Tx]
}

func NewScheduler(client *river.Client[pgx.Tx]) *Scheduler {
	return &Scheduler{client: client}
}

// NewInsertClient builds a river client that can only enqueue jobs, for
// the one-shot `ic` commands. A job they enqueue is worked by serve,
// like any other, at its scheduled time. The client has no workers, so
// river cannot check the kinds it inserts against them; the kinds
// Scheduler inserts are all registered by NewClient.
func NewInsertClient(pool *pgxpool.Pool) (*river.Client[pgx.Tx], error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: slog.Default()})
	if err != nil {
		return nil, fmt.Errorf("build job enqueuer: %w", err)
	}
	return client, nil
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

// ScheduleCreateGames queues create-games for a round, to run at once,
// with its longer retry budget. queued is false when one for the round
// is already waiting or running.
func (s *Scheduler) ScheduleCreateGames(ctx context.Context, tx pgx.Tx, roundID int32, retry bool) (bool, error) {
	res, err := s.client.InsertTx(ctx, tx,
		CreateGamesArgs{RoundID: roundID, Retry: retry},
		&river.InsertOpts{
			MaxAttempts: createGamesAttempts,
			UniqueOpts:  createGamesUnique,
		},
	)
	if err != nil {
		return false, fmt.Errorf("enqueue create-games for round %d: %w", roundID, err)
	}
	return !res.UniqueSkippedAsDuplicate, nil
}

// ScheduleSyncGames queues one sync-games run now, besides the hourly
// ones (spec §7: once right after create-games).
func (s *Scheduler) ScheduleSyncGames(ctx context.Context, tx pgx.Tx) error {
	if _, err := s.client.InsertTx(ctx, tx, syncGamesArgs{}, nil); err != nil {
		return fmt.Errorf("enqueue sync-games: %w", err)
	}
	return nil
}

// ScheduleCancelChallenge queues the withdrawal of a pairing's
// challenge on Lichess (spec §6.4). A second request for the same
// pairing adds nothing.
func (s *Scheduler) ScheduleCancelChallenge(ctx context.Context, tx pgx.Tx, pairingID pgtype.UUID) error {
	_, err := s.client.InsertTx(ctx, tx,
		CancelChallengeArgs{PairingID: pairingID},
		&river.InsertOpts{UniqueOpts: river.UniqueOpts{ByArgs: true}},
	)
	if err != nil {
		return fmt.Errorf("enqueue cancel-challenge for pairing %s: %w", pairingID.String(), err)
	}
	return nil
}

// NewClient builds the river client `serve` runs: one queue, one worker
// at a time (Lichess wants a single request at a time — spec §3.4), and
// the periodic jobs. pairingCron is the pairing.cron setting; it is
// read once here, for both generate-round and validate-tokens, so
// changing it takes a restart until the Phase 6 settings UI can add and
// remove periodic jobs at runtime. The caller
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
	river.AddWorker(workers, workerFor(h.ValidateTokens, validateTokensArgs{}))
	river.AddWorker(workers, river.WorkFunc(func(ctx context.Context, job *river.Job[PublishRoundArgs]) error {
		return h.PublishRound(ctx, job.Args.RoundID, job.ID)
	}))
	river.AddWorker(workers, river.WorkFunc(func(ctx context.Context, job *river.Job[CreateGamesArgs]) error {
		// Attempt counts from 1 while the job runs.
		return h.CreateGames(ctx, job.Args, job.Attempt >= job.MaxAttempts, job.ID)
	}))
	river.AddWorker(workers, river.WorkFunc(func(ctx context.Context, job *river.Job[CancelChallengeArgs]) error {
		return h.CancelChallenge(ctx, job.Args.PairingID, job.ID)
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
			river.NewPeriodicJob(
				before{schedule: schedule, by: time.Hour},
				func() (river.JobArgs, *river.InsertOpts) { return validateTokensArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: false},
			),
		},
	})
}

// before fires a fixed time ahead of another schedule. validate-tokens
// runs an hour before each generation (spec §7), so the pool sees
// tokens checked as late as possible, and it follows pairing.cron
// wherever it is set, CRON_TZ= included.
type before struct {
	schedule cron.Schedule
	by       time.Duration
}

// Next is the first firing of the underlying schedule that is more than
// `by` after t, moved `by` earlier.
func (b before) Next(t time.Time) time.Time {
	return b.schedule.Next(t.Add(b.by)).Add(-b.by)
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
