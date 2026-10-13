package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// runPublishRound implements the publish-round job: one draft, at the
// end of its review window (spec §6.1). It is scheduled for that
// draft's publish_at when the round is generated, and it is also what
// `ic publish-round <number>` runs to publish early. sched queues the
// round's create-games job when the site creates the games: serve's own
// river client for the job, an insert-only one for the CLI.
//
// Finding the round already published or cancelled is a SUCCESSFUL run
// recording published=false, not a failure: the scheduled job, the
// hourly sweep and an admin's "publish now" all race by design and the
// first one wins.
func runPublishRound(
	ctx context.Context,
	db txBeginner,
	roundID int32,
	sched rounds.Scheduler,
	riverJobID *int64,
) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "publish-round", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("publish-round: create job run: %w", err)
	}

	round, published, jobErr := publishInTx(ctx, db, roundID, sched)

	detail, _ := json.Marshal(map[string]any{
		"round_id":      roundID,
		"number":        round.Number,
		"published":     published,
		"game_creation": round.GameCreation,
	})
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	var processed int32
	if published {
		processed = 1
	}
	if jobErr != nil {
		status = gen.JobRunStatusFailed
		msg := jobErr.Error()
		errMsg = &msg
	}
	if finishErr := q.FinishJobRun(context.WithoutCancel(ctx), gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: processed,
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("publish-round: record job outcome", "error", finishErr)
	}

	if jobErr != nil {
		return fmt.Errorf("publish-round: %w", jobErr)
	}
	slog.Info("publish-round complete", "round_id", roundID, "published", published)
	return nil
}

func publishInTx(ctx context.Context, db txBeginner, roundID int32, sched rounds.Scheduler) (gen.Round, bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return gen.Round{}, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	cfg, err := settings.Load(ctx, gen.New(tx))
	if err != nil {
		return gen.Round{}, false, fmt.Errorf("load settings: %w", err)
	}
	round, published, err := rounds.Publish(ctx, tx, roundID, cfg, time.Now(), pgtype.UUID{}, sched)
	if err != nil {
		return gen.Round{}, false, err
	}
	return round, published, tx.Commit(ctx)
}

// runPublishSweep implements the hourly publish-round-sweep (spec §7):
// it publishes any draft whose review window has passed. Its job is to
// make a lost or never-enqueued publish-round job cost an hour rather
// than a week. sched is serve's river client, as for runPublishRound.
func runPublishSweep(ctx context.Context, db txBeginner, sched rounds.Scheduler, riverJobID *int64) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "publish-round-sweep", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("publish-round-sweep: create job run: %w", err)
	}

	published, jobErr := sweepInTx(ctx, db, sched)

	numbers := make([]int32, 0, len(published))
	for _, round := range published {
		numbers = append(numbers, round.Number)
	}
	detail, _ := json.Marshal(map[string]any{"published": numbers})
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	if jobErr != nil {
		status = gen.JobRunStatusFailed
		msg := jobErr.Error()
		errMsg = &msg
	}
	if finishErr := q.FinishJobRun(context.WithoutCancel(ctx), gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: int32(len(published)),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("publish-round-sweep: record job outcome", "error", finishErr)
	}

	if jobErr != nil {
		return fmt.Errorf("publish-round-sweep: %w", jobErr)
	}
	if len(numbers) > 0 {
		slog.Info("publish-round-sweep published overdue drafts", "rounds", numbers)
	}
	return nil
}

func sweepInTx(ctx context.Context, db txBeginner, sched rounds.Scheduler) ([]gen.Round, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	cfg, err := settings.Load(ctx, gen.New(tx))
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	published, err := rounds.PublishDue(ctx, tx, cfg, time.Now(), sched)
	if err != nil {
		return nil, err
	}
	return published, tx.Commit(ctx)
}
