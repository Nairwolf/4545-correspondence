package rounds

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/pairing"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// Outcome is one generation's result: the round row as stored, and what
// the engine decided, which the caller reports in the job's detail and
// on the admin page.
type Outcome struct {
	Round  gen.Round
	Result pairing.Result
}

// Generate pairs the next round (spec §6.2) and writes it. In
// review_window mode the round lands as a draft that publishes itself
// at publish_at; in auto_publish mode it is published in this same
// transaction. actor is the admin who asked for it, or the zero uuid
// for the scheduled job. sched may be nil, in which case the draft's
// publication is left to the hourly sweep.
func Generate(
	ctx context.Context,
	tx pgx.Tx,
	cfg settings.Settings,
	now time.Time,
	source gen.RoundSource,
	actor pgtype.UUID,
	sched Scheduler,
) (Outcome, error) {
	q := gen.New(tx)

	number, err := q.NextRoundNumber(ctx)
	if err != nil {
		return Outcome{}, fmt.Errorf("next round number: %w", err)
	}
	p, err := loadPool(ctx, q, cfg, number)
	if err != nil {
		return Outcome{}, err
	}
	result := pairing.Generate(p.players, p.history, engineConfig(cfg, number), solverFor(cfg))

	state, publishAt, publishedAt := publication(cfg, now)
	settingsUsed, err := json.Marshal(snapshot(cfg))
	if err != nil {
		return Outcome{}, fmt.Errorf("marshal settings snapshot: %w", err)
	}

	round, err := q.CreateGeneratedRound(ctx, gen.CreateGeneratedRoundParams{
		Number:         number,
		State:          state,
		PublishAt:      timestamp(publishAt),
		PublishedAt:    publishedAt,
		PairAt:         timestamp(publishAt),
		GeneratedBy:    source,
		PoolSize:       int32Ptr(result.PoolSize),
		OddPool:        oddPoolOutcome(result),
		RepeatPairings: int32Ptr(result.RepeatPairings),
		SettingsUsed:   settingsUsed,
	})
	if err != nil {
		if draftConflict(err) {
			return Outcome{}, ErrDraftExists
		}
		return Outcome{}, fmt.Errorf("create round: %w", err)
	}

	if err := writeResult(ctx, q, round.ID, result, p.userIDs); err != nil {
		return Outcome{}, err
	}
	if err := auditRound(ctx, q, "round.generate", actor, round, nil, describe(result)); err != nil {
		return Outcome{}, err
	}
	if err := schedulePublication(ctx, tx, sched, round); err != nil {
		return Outcome{}, err
	}
	return Outcome{Round: round, Result: result}, nil
}

// Regenerate runs the engine again into an existing draft, replacing
// its pairings, byes, double games and exclusions. The round keeps its
// number and its review window: an admin rejecting a draft is asking
// for a different pairing, not a different week. The pool is read
// afresh, which is how §5.8's "the pool is not recalculated during the
// review window" is picked up when it matters.
func Regenerate(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	cfg settings.Settings,
	actor pgtype.UUID,
	sched Scheduler,
) (Outcome, error) {
	q := gen.New(tx)

	round, err := q.GetRoundForUpdate(ctx, roundID)
	if err != nil {
		return Outcome{}, fmt.Errorf("lock round: %w", err)
	}
	if round.State != gen.RoundStateDraft {
		return Outcome{}, ErrNotDraft
	}

	before, err := describeStored(ctx, q, round.ID)
	if err != nil {
		return Outcome{}, err
	}
	for _, clear := range []func(context.Context, int32) error{
		q.DeleteRoundPairings,
		q.DeleteRoundByes,
		q.DeleteRoundDoubleGames,
		q.DeleteRoundExclusions,
	} {
		if err := clear(ctx, round.ID); err != nil {
			return Outcome{}, fmt.Errorf("clear round %d: %w", round.Number, err)
		}
	}

	p, err := loadPool(ctx, q, cfg, round.Number)
	if err != nil {
		return Outcome{}, err
	}
	result := pairing.Generate(p.players, p.history, engineConfig(cfg, round.Number), solverFor(cfg))

	settingsUsed, err := json.Marshal(snapshot(cfg))
	if err != nil {
		return Outcome{}, fmt.Errorf("marshal settings snapshot: %w", err)
	}
	round, err = q.RefreshGeneratedRound(ctx, gen.RefreshGeneratedRoundParams{
		ID:             round.ID,
		GeneratedBy:    round.GeneratedBy,
		PoolSize:       int32Ptr(result.PoolSize),
		OddPool:        oddPoolOutcome(result),
		RepeatPairings: int32Ptr(result.RepeatPairings),
		SettingsUsed:   settingsUsed,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("refresh round: %w", err)
	}

	if err := writeResult(ctx, q, round.ID, result, p.userIDs); err != nil {
		return Outcome{}, err
	}
	if err := auditRound(ctx, q, "round.regenerate", actor, round, before, describe(result)); err != nil {
		return Outcome{}, err
	}
	// publish_at did not move, so this re-enqueues the same job the
	// original generation did — a no-op when it is already queued, and
	// the safety net when the draft came from the CLI, which has no
	// river client to enqueue with.
	if err := schedulePublication(ctx, tx, sched, round); err != nil {
		return Outcome{}, err
	}
	return Outcome{Round: round, Result: result}, nil
}

// publication decides the round's state and timestamps from the mode
// (spec §6.1). In review_window mode pair_at is provisionally the
// planned publication moment; publishing rewrites it to the moment it
// actually happened (§6.3).
func publication(cfg settings.Settings, now time.Time) (gen.RoundState, time.Time, pgtype.Timestamptz) {
	if cfg.PairingMode == settings.PairingModeAutoPublish {
		return gen.RoundStatePublished, now, timestamp(now)
	}
	publishAt := now.Add(time.Duration(cfg.ReviewWindowHours) * time.Hour)
	return gen.RoundStateDraft, publishAt, pgtype.Timestamptz{}
}

func schedulePublication(ctx context.Context, tx pgx.Tx, sched Scheduler, round gen.Round) error {
	if sched == nil || round.State != gen.RoundStateDraft {
		return nil
	}
	if err := sched.SchedulePublish(ctx, tx, round.ID, round.PublishAt.Time); err != nil {
		return fmt.Errorf("schedule publication of round %d: %w", round.Number, err)
	}
	return nil
}

// writeResult stores everything the engine decided: the pairings with
// their §8.5 diagnostics, the odd-pool outcome, and one exclusion row
// per absent player so the dashboard can answer "why didn't I get a
// game this week?" without re-deriving anything.
func writeResult(
	ctx context.Context,
	q *gen.Queries,
	roundID int32,
	result pairing.Result,
	userIDs map[string]pgtype.UUID,
) error {
	for i, p := range result.Pairings {
		_, err := q.InsertGeneratedPairing(ctx, gen.InsertGeneratedPairingParams{
			RoundID:       roundID,
			WhiteUserID:   userIDs[p.White],
			BlackUserID:   userIDs[p.Black],
			Position:      int32Ptr(i + 1),
			RatingGap:     int32Ptr(p.RatingGap),
			ColorPenalty:  int32Ptr(p.ColorPenalty),
			RepeatOfRound: optionalInt32(p.RepeatOfRound),
		})
		if err != nil {
			return fmt.Errorf("insert pairing %d: %w", i+1, err)
		}
	}

	if result.Bye != nil {
		_, err := q.InsertBye(ctx, gen.InsertByeParams{RoundID: roundID, UserID: userIDs[*result.Bye]})
		if err != nil {
			return fmt.Errorf("insert bye: %w", err)
		}
	}
	if result.DoubleGame != nil {
		_, err := q.InsertDoubleGame(ctx, gen.InsertDoubleGameParams{
			RoundID: roundID,
			UserID:  userIDs[*result.DoubleGame],
		})
		if err != nil {
			return fmt.Errorf("insert double game: %w", err)
		}
	}

	for _, e := range result.Exclusions {
		reason, err := exclusionReason(e.Reason)
		if err != nil {
			return err
		}
		params := gen.InsertRoundExclusionParams{
			RoundID: roundID,
			UserID:  userIDs[e.ID],
			Reason:  reason,
		}
		if reason == gen.ExclusionReasonAtCapacity {
			params.OngoingGames = int32Ptr(e.InFlight)
			params.MaxConcurrentGames = int32Ptr(e.Cap)
		}
		if _, err := q.InsertRoundExclusion(ctx, params); err != nil {
			return fmt.Errorf("insert %s exclusion: %w", reason, err)
		}
	}
	return nil
}

func exclusionReason(r pairing.Reason) (gen.ExclusionReason, error) {
	switch r {
	case pairing.ReasonAtCapacity:
		return gen.ExclusionReasonAtCapacity, nil
	case pairing.ReasonInactive:
		return gen.ExclusionReasonInactive, nil
	case pairing.ReasonPaused:
		return gen.ExclusionReasonPaused, nil
	case pairing.ReasonAutoPaused:
		return gen.ExclusionReasonAutoPaused, nil
	case pairing.ReasonBye:
		return gen.ExclusionReasonBye, nil
	default:
		return "", fmt.Errorf("unknown exclusion reason %q", r)
	}
}

func oddPoolOutcome(result pairing.Result) *gen.OddPoolOutcome {
	outcome := gen.OddPoolOutcomeEven
	switch {
	case result.DoubleGame != nil:
		outcome = gen.OddPoolOutcomeDoubleGame
	case result.Bye != nil:
		outcome = gen.OddPoolOutcomeBye
	}
	return &outcome
}

func optionalInt32(v *int) *int32 {
	if v == nil {
		return nil
	}
	return int32Ptr(*v)
}

// describe is the audit row's "after": what the generation produced, in
// the terms an admin reading the log would ask about.
func describe(result pairing.Result) map[string]any {
	after := map[string]any{
		"pool_size":       result.PoolSize,
		"pairings":        len(result.Pairings),
		"exclusions":      len(result.Exclusions),
		"repeat_pairings": result.RepeatPairings,
	}
	if result.DoubleGame != nil {
		after["double_game"] = *result.DoubleGame
	}
	if result.Bye != nil {
		after["bye"] = *result.Bye
	}
	return after
}

// describeStored is the same shape for the round as it stands now, so a
// regeneration's audit row says what it replaced.
func describeStored(ctx context.Context, q *gen.Queries, roundID int32) (map[string]any, error) {
	stored, err := q.ListPairingsForRound(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("list pairings: %w", err)
	}
	pairings := make([]string, 0, len(stored))
	for _, p := range stored {
		pairings = append(pairings, p.WhiteUsername+" vs "+p.BlackUsername)
	}
	return map[string]any{"pairings": pairings}, nil
}

func auditRound(
	ctx context.Context,
	q *gen.Queries,
	action string,
	actor pgtype.UUID,
	round gen.Round,
	before, after any,
) error {
	var beforeJSON, afterJSON []byte
	if before != nil {
		beforeJSON, _ = json.Marshal(before)
	}
	if after != nil {
		afterJSON, _ = json.Marshal(after)
	}
	err := q.CreateAuditLogEntry(ctx, gen.CreateAuditLogEntryParams{
		ActorUserID: actor,
		Action:      action,
		EntityType:  "round",
		EntityID:    strconv.Itoa(int(round.ID)),
		Before:      beforeJSON,
		After:       afterJSON,
	})
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
