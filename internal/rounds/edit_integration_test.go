//go:build integration

package rounds_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

func TestFlipColours_SwapsAndClearsDiagnostics(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	alpha := addPlayer(t, tx, "alpha", 2000)
	bravo := addPlayer(t, tx, "bravo", 1900)
	draft := generate(t, tx, settings.Defaults(), nil).Round

	stored, err := q.ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	before := stored[0]

	flipped, err := rounds.FlipColours(ctx, tx, draft.ID, before.ID, pgtype.UUID{})
	require.NoError(t, err)

	assert.Equal(t, before.BlackUserID, flipped.WhiteUserID)
	assert.Equal(t, before.WhiteUserID, flipped.BlackUserID)
	assert.Nil(t, flipped.RatingGap, "diagnostics no longer describe the stored colours")
	assert.Nil(t, flipped.ColorPenalty)
	assert.Nil(t, flipped.RepeatOfRound)

	var count int
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE action = 'pairing.flip' AND entity_type = 'pairing' AND entity_id = $1`,
		before.ID.String(),
	).Scan(&count))
	assert.Equal(t, 1, count, "the flip is in the audit log")

	_ = alpha
	_ = bravo
}

func TestFlipColours_RefusesAPublishedRound(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)
	cfg := settings.Defaults()
	cfg.PairingMode = settings.PairingModeAutoPublish
	round := generate(t, tx, cfg, nil).Round

	stored, err := q.ListPairingsForRound(ctx, round.ID)
	require.NoError(t, err)
	require.Len(t, stored, 1)

	_, err = rounds.FlipColours(ctx, tx, round.ID, stored[0].ID, pgtype.UUID{})
	assert.ErrorIs(t, err, rounds.ErrNotDraft)
}

func TestRemovePairing_ExcludesBothPlayers(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)
	draft := generate(t, tx, settings.Defaults(), nil).Round

	stored, err := q.ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	pairing := stored[0]

	require.NoError(t, rounds.RemovePairing(ctx, tx, draft.ID, pairing.ID, pgtype.UUID{}))

	remaining, err := q.ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	assert.Empty(t, remaining)

	exclusions, err := q.ListExclusionsForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, exclusions, 2)
	for _, e := range exclusions {
		assert.Equal(t, gen.ExclusionReasonRemovedByAdmin, e.Reason)
		assert.True(t, e.UserID == pairing.WhiteUserID || e.UserID == pairing.BlackUserID)
	}
}

func TestRemovePairing_LeavesTheVolunteersOtherGameAlone(t *testing.T) {
	// Five players is an odd pool: someone plays twice. Removing one of
	// their two games must not exclude them — their other pairing this
	// round still stands — it should only drop their double_games row.
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)
	addPlayer(t, tx, "charlie", 1800)
	addPlayer(t, tx, "delta", 1700)
	addPlayer(t, tx, "echo", 1600)

	outcome := generate(t, tx, settings.Defaults(), nil)
	require.NotNil(t, outcome.Result.DoubleGame, "a pool of five always produces a double game or a bye")
	if outcome.Result.DoubleGame == nil {
		t.Skip("this pool resolved to a bye, not a double game; rerun")
	}
	volunteerID := *outcome.Result.DoubleGame

	stored, err := q.ListPairingsForRound(ctx, outcome.Round.ID)
	require.NoError(t, err)
	var toRemove gen.ListPairingsForRoundRow
	for _, p := range stored {
		if p.WhiteUserID.String() == volunteerID || p.BlackUserID.String() == volunteerID {
			toRemove = p
			break
		}
	}
	require.NotEmpty(t, toRemove.ID.String())

	require.NoError(t, rounds.RemovePairing(ctx, tx, outcome.Round.ID, toRemove.ID, pgtype.UUID{}))

	doubles, err := q.ListDoubleGamesForRound(ctx, outcome.Round.ID)
	require.NoError(t, err)
	assert.Empty(t, doubles, "no longer playing two games this round")

	exclusions, err := q.ListExclusionsForRound(ctx, outcome.Round.ID)
	require.NoError(t, err)
	for _, e := range exclusions {
		assert.NotEqual(t, volunteerID, e.UserID.String(), "the volunteer still has their other game")
	}

	remaining, err := q.ListPairingsForRound(ctx, outcome.Round.ID)
	require.NoError(t, err)
	var stillPlaying bool
	for _, p := range remaining {
		if p.WhiteUserID.String() == volunteerID || p.BlackUserID.String() == volunteerID {
			stillPlaying = true
		}
	}
	assert.True(t, stillPlaying)
}

func TestSwapPairings_ExchangesOpponents(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	alpha := addPlayer(t, tx, "alpha", 2000)
	bravo := addPlayer(t, tx, "bravo", 1900)
	charlie := addPlayer(t, tx, "charlie", 1800)
	delta := addPlayer(t, tx, "delta", 1700)
	draft := generate(t, tx, settings.Defaults(), nil).Round

	stored, err := q.ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)

	require.NoError(t, rounds.SwapPairings(ctx, tx, settings.Defaults(), draft.ID, stored[0].ID, stored[1].ID, pgtype.UUID{}))

	after, err := q.ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, after, 2)

	pairs := map[[2]string]bool{}
	for _, p := range after {
		pairs[[2]string{p.WhiteUserID.String(), p.BlackUserID.String()}] = true
		pairs[[2]string{p.BlackUserID.String(), p.WhiteUserID.String()}] = true
		assert.NotEqual(t, gen.PairingMethodManualExternal, "", "sanity")
		assert.Nil(t, p.Position, "a swapped pairing carries no stale diagnostics")
	}

	// The four players must now form the OTHER two pairs than the
	// original two (whichever those were), i.e. no player kept their
	// original opponent.
	origPairs := map[[2]string]bool{}
	for _, p := range stored {
		origPairs[[2]string{p.WhiteUserID.String(), p.BlackUserID.String()}] = true
	}
	for k := range origPairs {
		assert.False(t, pairs[k], "a swap must change both pairs' opponents")
	}

	// And it is still a perfect matching over the same four players.
	seen := map[string]int{}
	for _, p := range after {
		seen[p.WhiteUserID.String()]++
		seen[p.BlackUserID.String()]++
	}
	for _, u := range []gen.User{alpha, bravo, charlie, delta} {
		assert.Equal(t, 1, seen[u.ID.String()], "%s must appear in exactly one pairing after the swap", u.LichessUsername)
	}
}

func TestSwapPairings_RefusesAPublishedRound(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)
	addPlayer(t, tx, "charlie", 1800)
	addPlayer(t, tx, "delta", 1700)
	cfg := settings.Defaults()
	cfg.PairingMode = settings.PairingModeAutoPublish
	round := generate(t, tx, cfg, nil).Round

	stored, err := q.ListPairingsForRound(ctx, round.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)

	err = rounds.SwapPairings(ctx, tx, cfg, round.ID, stored[0].ID, stored[1].ID, pgtype.UUID{})
	assert.ErrorIs(t, err, rounds.ErrNotDraft)
}

func TestMarkFailed_OnlyPublishedPendingPairings(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)

	t.Run("a draft's pairing cannot be marked failed", func(t *testing.T) {
		draft := generate(t, tx, settings.Defaults(), nil).Round
		stored, err := q.ListPairingsForRound(ctx, draft.ID)
		require.NoError(t, err)
		require.Len(t, stored, 1)

		_, err = rounds.MarkFailed(ctx, tx, draft.ID, stored[0].ID, pgtype.UUID{})
		assert.ErrorIs(t, err, rounds.ErrNotFound)

		_, cancelErr := rounds.Cancel(ctx, tx, draft.ID, "cleanup", pgtype.UUID{})
		require.NoError(t, cancelErr)
	})

	t.Run("a published round's pending pairing can be", func(t *testing.T) {
		cfg := settings.Defaults()
		cfg.PairingMode = settings.PairingModeAutoPublish
		round := generate(t, tx, cfg, nil).Round
		stored, err := q.ListPairingsForRound(ctx, round.ID)
		require.NoError(t, err)
		require.Len(t, stored, 1)

		failed, err := rounds.MarkFailed(ctx, tx, round.ID, stored[0].ID, pgtype.UUID{})
		require.NoError(t, err)
		assert.Equal(t, gen.PairingStatusFailed, failed.Status)

		// Idempotent-in-spirit: a second call finds nothing pending left.
		_, err = rounds.MarkFailed(ctx, tx, round.ID, stored[0].ID, pgtype.UUID{})
		assert.ErrorIs(t, err, rounds.ErrNotFound)
	})
}
