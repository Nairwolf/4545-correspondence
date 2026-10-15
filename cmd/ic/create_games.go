package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/jobs"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// createGamesResult is what ends up in job_runs.detail.
type createGamesResult struct {
	rounds.CreationReport
	// Search is a retry's search for games started by hand (§7.3).
	Search *syncGamesStats `json:"search,omitempty"`
	// GaveUp is set by the last attempt, when it failed: the pairings
	// left are now the players' to start.
	GaveUp bool `json:"gave_up,omitempty"`
}

// runCreateGames implements the create-games job (spec §6.3) for one
// round, queued by its publication under pairing.game_creation =
// lichess or by an admin's retry. The work itself is
// rounds.GameCreator's; this records the run, runs a retry's search
// for games started by hand first, and gives up on the last attempt.
//
// A failed run returns its error, so river retries it, ten attempts in
// all. When the last one fails, the round's game creation is given up
// (rounds.GiveUp) — written even if the run's own context was
// cancelled, since nothing would retry it — and the run is still
// recorded as failed: the games were not created, and /health and the
// admin banner say so until an admin retries.
func runCreateGames(
	ctx context.Context,
	db txBeginner,
	creator rounds.GameCreator,
	args jobs.CreateGamesArgs,
	lastAttempt bool,
	riverJobID *int64,
) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "create-games", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("create-games: create job run: %w", err)
	}

	var result createGamesResult
	cfg, jobErr := settings.Load(ctx, q)
	if jobErr != nil {
		jobErr = fmt.Errorf("load settings: %w", jobErr)
	}
	if jobErr == nil && args.Retry {
		var search syncGamesStats
		jobErr = searchByHand(ctx, q, creator.Lichess, cfg, args.RoundID, &search)
		result.Search = &search
	}
	if jobErr == nil {
		result.CreationReport, jobErr = creator.CreateGames(ctx, db, args.RoundID, cfg, args.Retry, time.Now())
	}

	if jobErr != nil && lastAttempt {
		round, err := giveUpInTx(context.WithoutCancel(ctx), db, args.RoundID)
		if err != nil {
			jobErr = fmt.Errorf("%w; giving up failed too: %v", jobErr, err)
		} else {
			result.Round = round.Number
			result.State = round.GameCreation
			result.GaveUp = round.GameCreation == gen.GameCreationStateGaveUp
		}
	}

	detail, _ := json.Marshal(result)
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
		ItemsProcessed: int32(result.Created + result.Adopted + result.Challenges),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("create-games: record job outcome", "error", finishErr)
	}

	if jobErr != nil {
		return fmt.Errorf("create-games: %w", jobErr)
	}
	slog.Info("create-games complete",
		"round", result.Round,
		"state", result.State,
		"created", result.Created,
		"adopted", result.Adopted,
		"challenges", result.Challenges,
		"by_hand", result.ByHand,
	)
	return nil
}

// searchByHand is a retry's first step (spec §6.3 step 2): the §7.3
// search, for this round's pairings still waiting, so a game a player
// has already started by hand is attached rather than created a second
// time. Unlike sync-games, it fails if any Lichess call does: a game it
// could not look for may exist.
func searchByHand(
	ctx context.Context,
	q *gen.Queries,
	client lichess.API,
	cfg settings.Settings,
	roundID int32,
	stats *syncGamesStats,
) error {
	unmatched, err := q.ListUnmatchedPairings(ctx)
	if err != nil {
		return fmt.Errorf("list unmatched pairings: %w", err)
	}
	var ours []gen.ListUnmatchedPairingsRow
	for _, p := range unmatched {
		if p.RoundID == roundID {
			ours = append(ours, p)
		}
	}
	if err := matchPairings(ctx, q, client, cfg, ours, stats); err != nil {
		return fmt.Errorf("search for games started by hand: %w", err)
	}
	if stats.LichessCallsFailed > 0 {
		return fmt.Errorf(
			"search for games started by hand: %d Lichess calls failed: %s",
			stats.LichessCallsFailed,
			strings.Join(stats.LichessErrors, "; "),
		)
	}
	return nil
}

func giveUpInTx(ctx context.Context, db txBeginner, roundID int32) (gen.Round, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return gen.Round{}, fmt.Errorf("give up: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	round, err := rounds.GiveUp(ctx, tx, roundID)
	if err != nil {
		return gen.Round{}, err
	}
	return round, tx.Commit(ctx)
}

// queueCreateGames is `ic create-games <round-number>`: an admin's
// retry from the command line, the same as the round page's button
// (spec §8.5). It queues the job for serve to run rather than running
// it here, so it can never run alongside a create-games job serve is
// already working for the round.
func queueCreateGames(ctx context.Context, db txBeginner, sched rounds.Scheduler, arg string) error {
	number, err := strconv.Atoi(arg)
	if err != nil {
		return fmt.Errorf("create-games: %q is not a round number", arg)
	}
	round, err := gen.New(db).GetRoundByNumber(ctx, int32(number))
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("create-games: no round %d", number)
	}
	if err != nil {
		return fmt.Errorf("create-games: look up round %d: %w", number, err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("create-games: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	cfg, err := settings.Load(ctx, gen.New(tx))
	if err != nil {
		return fmt.Errorf("create-games: load settings: %w", err)
	}
	if _, err := rounds.RetryGameCreation(ctx, tx, round.ID, cfg, pgtype.UUID{}, sched); err != nil {
		return fmt.Errorf("create-games: round %d: %w", number, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("create-games: commit: %w", err)
	}
	slog.Info("create-games queued: serve runs it", "round", number)
	return nil
}
