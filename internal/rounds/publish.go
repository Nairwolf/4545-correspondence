package rounds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// Publish ends a draft's review window (spec §6.3). Three callers race
// by design — the job scheduled at publish_at, the hourly sweep, and an
// admin's "publish now" — so the state change is a guarded update:
// whichever gets there first publishes the round, and the others find
// zero rows and report published=false. A round that was cancelled
// meanwhile is the same no-op, which is what lets Cancel leave its
// scheduled job alone rather than hunting it down in river's tables.
//
// Publication is only the state change: the pairings stay
// manual_external/pending, players challenge each other by hand, and
// sync-games discovers the games (§7.3). Phase 5 creates them on
// Lichess between the lock and the update.
func Publish(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	now time.Time,
	actor pgtype.UUID,
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

	after := map[string]any{"number": round.Number, "published_at": now}
	if err := auditRound(ctx, q, "round.publish", actor, round, nil, after); err != nil {
		return gen.Round{}, false, err
	}
	return round, true, nil
}

// PublishDue publishes every draft whose review window has ended. It is
// the hourly sweep of spec §7 — the safety net for a scheduled job lost
// to a restart, or a draft generated from the CLI, which has no job
// runner to schedule one. At most one draft can exist at a time, so the
// loop is a formality rather than a batch.
func PublishDue(ctx context.Context, tx pgx.Tx, now time.Time) ([]gen.Round, error) {
	due, err := gen.New(tx).ListDraftsDue(ctx, timestamp(now))
	if err != nil {
		return nil, fmt.Errorf("list drafts due: %w", err)
	}

	var published []gen.Round
	for _, draft := range due {
		round, ok, err := Publish(ctx, tx, draft.ID, now, pgtype.UUID{})
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
// sequence has no gaps. Cancelling a PUBLISHED round is out of scope
// for Phase 4: games may already exist behind it, and calling them off
// means cancelling them on Lichess too (§6.3, Phase 5).
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
