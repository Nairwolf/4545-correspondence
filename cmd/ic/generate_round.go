package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/activity"
	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// txBeginner is what the round jobs need beyond gen.DBTX: the job_runs
// bookkeeping is written on the handle itself, and the round is built
// inside a transaction begun from it, so a failed generation leaves no
// half-written round while its outcome row survives. A *pgxpool.Pool in
// production; in tests a pgx.Tx, whose Begin opens a savepoint, so the
// whole run rolls back with the test.
type txBeginner interface {
	gen.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// runGenerateRound implements the generate-round job (spec §6.1): it
// pairs the next round on pairing.cron, and backs `ic generate-round`
// as well. sched enqueues the jobs that follow: the draft's
// publish-round job, and the withdrawal of each missed challenge.
// serve's own river client for the scheduled job, an insert-only one
// for the CLI.
//
// It runs evaluate-activity first (spec §6.4), on a job run of its own,
// so a player the missed-start rule pauses is already out of the round
// paired next, and the pairings it retires no longer count towards
// anyone's cap. A failed evaluation is recorded there and does not stop
// the round. client is the Lichess client it asks about challenges.
//
// A second generation while a draft is still waiting is a FAILED run
// carrying that message, not a silent no-op: it is visible on
// /admin/jobs and /health, which is where an admin looking for the
// week's round would go.
func runGenerateRound(
	ctx context.Context,
	db txBeginner,
	client lichess.API,
	source gen.RoundSource,
	sched rounds.Scheduler,
	riverJobID *int64,
) error {
	evaluator := activity.Evaluator{Lichess: client, Scheduler: sched}
	if err := evaluator.Run(ctx, db, riverJobID); err != nil {
		slog.Warn("generate-round: evaluation failed, generating anyway", "error", err)
	}

	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "generate-round", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("generate-round: create job run: %w", err)
	}

	var outcome rounds.Outcome
	cfg, jobErr := settings.Load(ctx, q)
	if jobErr != nil {
		jobErr = fmt.Errorf("load settings: %w", jobErr)
	} else {
		outcome, jobErr = generateInTx(ctx, db, cfg, source, sched)
	}

	detail, _ := json.Marshal(generateDetail(outcome))
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
		ItemsProcessed: int32(len(outcome.Result.Pairings)),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("generate-round: record job outcome", "error", finishErr)
	}

	if jobErr != nil {
		return fmt.Errorf("generate-round: %w", jobErr)
	}
	slog.Info("generate-round complete",
		"round", outcome.Round.Number,
		"state", outcome.Round.State,
		"pairings", len(outcome.Result.Pairings),
		"exclusions", len(outcome.Result.Exclusions),
	)
	return nil
}

func generateInTx(
	ctx context.Context,
	db txBeginner,
	cfg settings.Settings,
	source gen.RoundSource,
	sched rounds.Scheduler,
) (rounds.Outcome, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return rounds.Outcome{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed

	outcome, err := rounds.Generate(ctx, tx, cfg, time.Now(), source, pgtype.UUID{}, sched)
	if err != nil {
		return rounds.Outcome{}, err
	}
	return outcome, tx.Commit(ctx)
}

// generateDetail is the job_runs detail an admin reads on /admin/jobs:
// enough to see what the round looks like without opening it.
func generateDetail(outcome rounds.Outcome) map[string]any {
	detail := map[string]any{
		"round_number":    outcome.Round.Number,
		"state":           string(outcome.Round.State),
		"pool_size":       outcome.Result.PoolSize,
		"pairings":        len(outcome.Result.Pairings),
		"exclusions":      len(outcome.Result.Exclusions),
		"repeat_pairings": outcome.Result.RepeatPairings,
	}
	if outcome.Round.OddPool != nil {
		detail["odd_pool"] = string(*outcome.Round.OddPool)
	}
	return detail
}
