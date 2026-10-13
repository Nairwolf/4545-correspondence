package rounds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// Publish ends a draft's review window (spec §6.3). Three callers race
// by design — the job scheduled at publish_at, the hourly sweep, and an
// admin's "publish now" — so the state change is a guarded update:
// whichever gets there first publishes the round, and the others find
// zero rows and report published=false. A round that was cancelled
// meanwhile is the same no-op, which is what lets Cancel leave its
// scheduled job alone rather than hunting it down in river's tables.
//
// Publication also records how the round's games will be created,
// from cfg's pairing.game_creation (startGameCreation): by hand, or by
// a create-games job queued here, in the same transaction. The players
// are told what the round means for them (§10).
func Publish(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	cfg settings.Settings,
	now time.Time,
	actor pgtype.UUID,
	sched Scheduler,
) (round gen.Round, published bool, err error) {
	q := gen.New(tx)

	locked, err := q.GetRoundForUpdate(ctx, roundID)
	if err != nil {
		return gen.Round{}, false, fmt.Errorf("lock round: %w", err)
	}

	round, err = q.PublishRound(ctx, gen.PublishRoundParams{ID: roundID, PublishedAt: timestamp(now)})
	if errors.Is(err, pgx.ErrNoRows) {
		return locked, false, nil
	}
	if err != nil {
		return gen.Round{}, false, fmt.Errorf("publish round: %w", err)
	}
	if round, err = startGameCreation(ctx, tx, round, cfg, sched); err != nil {
		return gen.Round{}, false, err
	}

	after := map[string]any{
		"number":        round.Number,
		"published_at":  now,
		"game_creation": round.GameCreation,
	}
	if err := auditRound(ctx, q, "round.publish", actor, round, nil, after); err != nil {
		return gen.Round{}, false, err
	}
	if err := notifyPublished(ctx, q, round); err != nil {
		return gen.Round{}, false, err
	}
	return round, true, nil
}

// errNoScheduler is publication under pairing.game_creation = lichess
// without a job queue: nothing would ever create the round's games, so
// the round is not published at all.
var errNoScheduler = errors.New(
	"no job queue to create the round's games: publishing under pairing.game_creation = lichess needs one",
)

// startGameCreation records how a round just published will have its
// games created (spec §6.3): under manual, by its players, as before
// Phase 5; under lichess, by a create-games job queued here, in the
// publishing transaction, so the round is never published without it.
func startGameCreation(
	ctx context.Context,
	tx pgx.Tx,
	round gen.Round,
	cfg settings.Settings,
	sched Scheduler,
) (gen.Round, error) {
	state := gen.GameCreationStateManual
	if cfg.GameCreation == settings.GameCreationLichess {
		if sched == nil {
			return gen.Round{}, errNoScheduler
		}
		state = gen.GameCreationStateCreating
	}

	round, err := gen.New(tx).StartGameCreation(ctx, gen.StartGameCreationParams{
		ID:           round.ID,
		GameCreation: state,
	})
	if err != nil {
		return gen.Round{}, fmt.Errorf("record game creation of round %d: %w", round.Number, err)
	}
	if state == gen.GameCreationStateCreating {
		// A round published a moment ago has no job yet, so this one is
		// always queued.
		if _, err := sched.ScheduleCreateGames(ctx, tx, round.ID, false); err != nil {
			return gen.Round{}, fmt.Errorf("queue game creation of round %d: %w", round.Number, err)
		}
	}
	return round, nil
}

// PublishDue publishes every draft whose review window has ended. It is
// the hourly sweep of spec §7 — the safety net for a publish-round job
// lost to a restart or a failed enqueue. At most one draft can exist at
// a time, so the loop is a formality rather than a batch.
func PublishDue(
	ctx context.Context,
	tx pgx.Tx,
	cfg settings.Settings,
	now time.Time,
	sched Scheduler,
) ([]gen.Round, error) {
	due, err := gen.New(tx).ListDraftsDue(ctx, timestamp(now))
	if err != nil {
		return nil, fmt.Errorf("list drafts due: %w", err)
	}

	var published []gen.Round
	for _, draft := range due {
		round, ok, err := Publish(ctx, tx, draft.ID, cfg, now, pgtype.UUID{}, sched)
		if err != nil {
			return nil, err
		}
		if ok {
			published = append(published, round)
		}
	}
	return published, nil
}

// Cancel discards a draft, with a reason that is shown to admins on the
// round page and kept in the audit log. Its number goes back to the
// pool (rounds_number_live), so the next generation reuses it and the
// sequence has no gaps. A published round is final (spec §6.3): its
// games may exist on Lichess, and the review window was the undo.
func Cancel(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	reason string,
	actor pgtype.UUID,
) (gen.Round, error) {
	q := gen.New(tx)

	if _, err := q.GetRoundForUpdate(ctx, roundID); err != nil {
		return gen.Round{}, fmt.Errorf("lock round: %w", err)
	}

	round, err := q.CancelRound(ctx, gen.CancelRoundParams{ID: roundID, Notes: &reason})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Round{}, ErrNotDraft
	}
	if err != nil {
		return gen.Round{}, fmt.Errorf("cancel round: %w", err)
	}
	if err := q.CancelPairingsForRound(ctx, roundID); err != nil {
		return gen.Round{}, fmt.Errorf("cancel pairings: %w", err)
	}

	after := map[string]any{"number": round.Number, "reason": reason}
	if err := auditRound(ctx, q, "round.cancel", actor, round, nil, after); err != nil {
		return gen.Round{}, err
	}
	return round, nil
}
