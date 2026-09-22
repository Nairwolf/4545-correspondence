package rounds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/pairing"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// ErrNotFound is returned when a pairing id does not belong to the
// round it was addressed through — a stale form or a mistyped id,
// never a state an admin's own click can produce.
var ErrNotFound = errors.New("pairing not found in this round")

// The pairing edits of spec §8.5: flip, remove and swap, all draft-only
// and server-side enforced, each nulling the stored diagnostics (they
// would otherwise describe colours or opponents that no longer exist)
// and recording edited_by and an audit row. Mark failed is the one
// exception — it acts on a PUBLISHED round's still-pending pairing, the
// manual stand-in for Phase 5's missed-start job (decision 7).

// FlipColours swaps a pairing's two colours in place (spec §8.5).
func FlipColours(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	pairingID pgtype.UUID,
	actor pgtype.UUID,
) (gen.Pairing, error) {
	q := gen.New(tx)

	round, err := q.GetRoundForUpdate(ctx, roundID)
	if err != nil {
		return gen.Pairing{}, fmt.Errorf("lock round: %w", err)
	}
	if round.State != gen.RoundStateDraft {
		return gen.Pairing{}, ErrNotDraft
	}

	before, err := q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: pairingID, RoundID: roundID})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Pairing{}, ErrNotFound
	}
	if err != nil {
		return gen.Pairing{}, fmt.Errorf("get pairing: %w", err)
	}

	after, err := q.FlipPairingColours(ctx, gen.FlipPairingColoursParams{
		ID:          pairingID,
		WhiteUserID: before.BlackUserID,
		BlackUserID: before.WhiteUserID,
		EditedBy:    actor,
	})
	if err != nil {
		return gen.Pairing{}, fmt.Errorf("flip pairing: %w", err)
	}
	if err := auditPairing(ctx, q, "pairing.flip", actor, pairingID, before, after); err != nil {
		return gen.Pairing{}, err
	}
	return after, nil
}

// RemovePairing takes one pairing out of a draft (spec §8.5). Both
// players get a removed_by_admin exclusion so the dashboard can say
// why they have no game this week — unless the player is the odd
// pool's double-game volunteer and this was only one of their two
// games, in which case they still have the other one: only their
// double_games record is dropped, since they are no longer playing
// twice.
func RemovePairing(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	pairingID pgtype.UUID,
	actor pgtype.UUID,
) error {
	q := gen.New(tx)

	round, err := q.GetRoundForUpdate(ctx, roundID)
	if err != nil {
		return fmt.Errorf("lock round: %w", err)
	}
	if round.State != gen.RoundStateDraft {
		return ErrNotDraft
	}

	before, err := q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: pairingID, RoundID: roundID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get pairing: %w", err)
	}
	if _, err := q.DeletePairing(ctx, pairingID); err != nil {
		return fmt.Errorf("delete pairing: %w", err)
	}

	for _, userID := range []pgtype.UUID{before.WhiteUserID, before.BlackUserID} {
		stillDouble, err := q.DeleteDoubleGame(ctx, gen.DeleteDoubleGameParams{RoundID: roundID, UserID: userID})
		if err != nil {
			return fmt.Errorf("clear double game: %w", err)
		}
		if stillDouble > 0 {
			continue // their other pairing this round stands; they simply stop playing two
		}
		if _, err := q.InsertRoundExclusion(ctx, gen.InsertRoundExclusionParams{
			RoundID: roundID,
			UserID:  userID,
			Reason:  gen.ExclusionReasonRemovedByAdmin,
		}); err != nil {
			return fmt.Errorf("insert removed_by_admin exclusion: %w", err)
		}
	}

	return auditPairing(ctx, q, "pairing.remove", actor, pairingID, before, nil)
}

// SwapPairings exchanges the opponents of two pairings in one draft:
// A's white plays B's black, and B's white plays A's black. Colours are
// re-derived by the engine's own colour step (pairing.AssignColours)
// rather than kept from the originals, so a swap can never leave either
// new pair worse balanced than Generate would have made it.
func SwapPairings(
	ctx context.Context,
	tx pgx.Tx,
	cfg settings.Settings,
	roundID int32,
	pairingAID, pairingBID pgtype.UUID,
	actor pgtype.UUID,
) error {
	q := gen.New(tx)

	round, err := q.GetRoundForUpdate(ctx, roundID)
	if err != nil {
		return fmt.Errorf("lock round: %w", err)
	}
	if round.State != gen.RoundStateDraft {
		return ErrNotDraft
	}

	a, err := q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: pairingAID, RoundID: roundID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get pairing a: %w", err)
	}
	b, err := q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: pairingBID, RoundID: roundID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get pairing b: %w", err)
	}

	aWhite, err := playerSnapshot(ctx, q, cfg, a.WhiteUserID)
	if err != nil {
		return err
	}
	aBlack, err := playerSnapshot(ctx, q, cfg, a.BlackUserID)
	if err != nil {
		return err
	}
	bWhite, err := playerSnapshot(ctx, q, cfg, b.WhiteUserID)
	if err != nil {
		return err
	}
	bBlack, err := playerSnapshot(ctx, q, cfg, b.BlackUserID)
	if err != nil {
		return err
	}

	// Delete both old rows before inserting the new ones: an in-place
	// UPDATE could transiently collide with pairings_round_white_black
	// when the target (round, white, black) tuple is what a sibling row
	// is about to vacate.
	if _, err := q.DeletePairing(ctx, pairingAID); err != nil {
		return fmt.Errorf("delete pairing a: %w", err)
	}
	if _, err := q.DeletePairing(ctx, pairingBID); err != nil {
		return fmt.Errorf("delete pairing b: %w", err)
	}

	first, second := pairing.AssignColours(aWhite, bBlack)
	newA, err := q.InsertEditedPairing(ctx, gen.InsertEditedPairingParams{
		RoundID:        roundID,
		WhiteUserID:    pgUUID(first),
		BlackUserID:    pgUUID(second),
		CreationMethod: a.CreationMethod,
		Status:         gen.PairingStatusPending,
		EditedBy:       actor,
	})
	if err != nil {
		return fmt.Errorf("insert swapped pairing a: %w", err)
	}

	third, fourth := pairing.AssignColours(bWhite, aBlack)
	newB, err := q.InsertEditedPairing(ctx, gen.InsertEditedPairingParams{
		RoundID:        roundID,
		WhiteUserID:    pgUUID(third),
		BlackUserID:    pgUUID(fourth),
		CreationMethod: b.CreationMethod,
		Status:         gen.PairingStatusPending,
		EditedBy:       actor,
	})
	if err != nil {
		return fmt.Errorf("insert swapped pairing b: %w", err)
	}

	after := map[string]any{
		"a": fmt.Sprintf("%s vs %s", newA.WhiteUserID, newA.BlackUserID),
		"b": fmt.Sprintf("%s vs %s", newB.WhiteUserID, newB.BlackUserID),
	}
	before := map[string]any{
		"a": fmt.Sprintf("%s vs %s", a.WhiteUserID, a.BlackUserID),
		"b": fmt.Sprintf("%s vs %s", b.WhiteUserID, b.BlackUserID),
	}
	return auditRound(ctx, q, "pairing.swap", actor, round, before, after)
}

// MarkFailed is the admin stand-in for Phase 5's missed-start job
// (decision 7): a published round's still-pending pairing — one
// nobody ever started — is marked failed by hand. It does not touch
// capacity or exclusions; the player simply drops out of the in-flight
// count once the pairing is no longer pending (spec §5.8 decision 7).
func MarkFailed(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	pairingID pgtype.UUID,
	actor pgtype.UUID,
) (gen.Pairing, error) {
	q := gen.New(tx)

	after, err := q.MarkPublishedPairingFailed(ctx, gen.MarkPublishedPairingFailedParams{
		ID:      pairingID,
		RoundID: roundID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Pairing{}, ErrNotFound
	}
	if err != nil {
		return gen.Pairing{}, fmt.Errorf("mark pairing failed: %w", err)
	}

	before := map[string]any{"status": string(gen.PairingStatusPending)}
	afterDetail := map[string]any{"status": string(after.Status)}
	if err := auditPairing(ctx, q, "pairing.fail", actor, pairingID, before, afterDetail); err != nil {
		return gen.Pairing{}, err
	}
	return after, nil
}

// playerSnapshot is the colour-relevant slice of a player's current
// standing: just enough for pairing.AssignColours. A player with no
// standings row yet falls back to rating.unrated_default and a neutral
// colour score, the same rule loadPool applies to the whole pool.
func playerSnapshot(ctx context.Context, q *gen.Queries, cfg settings.Settings, id pgtype.UUID) (pairing.Player, error) {
	standing, err := q.GetPlayerStanding(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pairing.Player{ID: id.String(), PowerRating: cfg.UnratedDefault}, nil
	}
	if err != nil {
		return pairing.Player{}, fmt.Errorf("get standing: %w", err)
	}
	return pairing.Player{
		ID:          id.String(),
		PowerRating: int(standing.PowerRating),
		ColorScore:  int(standing.ColorScore),
	}, nil
}

func pgUUID(p pairing.Player) pgtype.UUID {
	var id pgtype.UUID
	_ = id.Scan(p.ID)
	return id
}

func auditPairing(
	ctx context.Context,
	q *gen.Queries,
	action string,
	actor pgtype.UUID,
	pairingID pgtype.UUID,
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
		EntityType:  "pairing",
		EntityID:    pairingID.String(),
		Before:      beforeJSON,
		After:       afterJSON,
	})
	if err != nil {
		return fmt.Errorf("audit %s: %w", action, err)
	}
	return nil
}
