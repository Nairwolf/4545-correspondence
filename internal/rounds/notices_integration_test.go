//go:build integration

package rounds_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

func noticesOf(t *testing.T, tx pgx.Tx, user gen.User) []gen.Notification {
	t.Helper()
	list, err := gen.New(tx).ListNotificationsForUser(context.Background(), gen.ListNotificationsForUserParams{
		UserID: user.ID,
		Limit:  100,
	})
	require.NoError(t, err)
	return list
}

func publish(t *testing.T, tx pgx.Tx, round gen.Round) bool {
	t.Helper()
	_, published, err := rounds.Publish(context.Background(), tx, round.ID, time.Now(), pgtype.UUID{})
	require.NoError(t, err)
	return published
}

func TestPublish_TellsEachPlayerTheirPairing(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	alpha := addPlayer(t, tx, "alpha", 2000)
	bravo := addPlayer(t, tx, "bravo", 1900)

	draft := generate(t, tx, settings.Defaults(), nil).Round
	assert.Empty(t, noticesOf(t, tx, alpha), "a draft can still change: nobody is told yet")

	require.True(t, publish(t, tx, draft))

	pairings, err := gen.New(tx).ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, pairings, 1)
	p := pairings[0]
	white, black := alpha, bravo
	if p.WhiteUserID != alpha.ID {
		white, black = bravo, alpha
	}

	for _, c := range []struct {
		user             gen.User
		opponent, colour string
	}{
		{white, black.LichessUsername, "white"},
		{black, white.LichessUsername, "black"},
	} {
		list := noticesOf(t, tx, c.user)
		require.Len(t, list, 1, c.colour)
		n := list[0]
		assert.Equal(t, "round", n.Category)
		assert.Equal(t, fmt.Sprintf("Round %d: you play %s with %s", draft.Number, c.opponent, c.colour), n.Title)
		assert.Contains(t, n.Body, "a rated correspondence game at 2 days per move, you with "+c.colour)
		require.NotNil(t, n.DedupeKey)
		assert.Equal(t, fmt.Sprintf("round:%d:paired:%s", draft.Number, p.ID.String()), *n.DedupeKey)
	}

	// The job, the sweep and an admin all try; only the first publishes,
	// and only it notifies.
	assert.False(t, publish(t, tx, draft))
	assert.Len(t, noticesOf(t, tx, alpha), 1)
}

func TestGenerate_AutoPublishTellsThePlayersAtOnce(t *testing.T) {
	tx := testTx(t)
	alpha := addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)

	cfg := settings.Defaults()
	cfg.PairingMode = settings.PairingModeAutoPublish
	round := generate(t, tx, cfg, nil).Round

	list := noticesOf(t, tx, alpha)
	require.Len(t, list, 1)
	assert.Contains(t, list[0].Title, fmt.Sprintf("Round %d: you play bravo", round.Number))
}

func TestPublish_TheVolunteerGetsOneNoticeForBothGames(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	players := []gen.User{
		addPlayer(t, tx, "alpha", 2000),
		addPlayer(t, tx, "bravo", 1900),
		addPlayer(t, tx, "charlie", 1800),
	}

	draft := generate(t, tx, settings.Defaults(), nil).Round
	doubles, err := gen.New(tx).ListDoubleGamesForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, doubles, 1, "three players and a willing volunteer: a double game, not a bye")
	require.True(t, publish(t, tx, draft))

	for _, u := range players {
		list := noticesOf(t, tx, u)
		require.Len(t, list, 1, u.LichessUsername)
		n := list[0]
		if u.ID != doubles[0].UserID {
			assert.Contains(t, n.Title, "you play "+doubles[0].LichessUsername)
			continue
		}
		assert.Equal(t, fmt.Sprintf("Round %d: 2 games this week", draft.Number), n.Title)
		assert.Contains(t, n.Body, "2 games: with white against ", "white first, whatever the pairing order")
		assert.Contains(t, n.Body, " and with black against ")
		assert.Contains(t, n.Body, "a rated correspondence game at 2 days per move")
		require.NotNil(t, n.DedupeKey)
		assert.Equal(t, fmt.Sprintf("round:%d:double", draft.Number), *n.DedupeKey)
	}
}

func TestPublish_TellsTheByeWhyAndSparesTheExcluded(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	players := []gen.User{
		addPlayer(t, tx, "alpha", 2000),
		addPlayer(t, tx, "bravo", 1900),
		addPlayer(t, tx, "charlie", 1800),
	}
	paused := addPlayer(t, tx, "delta", 1700)
	setActive(t, tx, paused, false)

	cfg := settings.Defaults()
	cfg.OddPoolStrategy = settings.OddPoolByeOnly
	draft := generate(t, tx, cfg, nil).Round
	byes, err := gen.New(tx).ListByesForRound(ctx, draft.ID)
	require.NoError(t, err)
	require.Len(t, byes, 1)
	require.True(t, publish(t, tx, draft))

	for _, u := range players {
		list := noticesOf(t, tx, u)
		require.Len(t, list, 1, u.LichessUsername)
		if u.ID != byes[0].UserID {
			continue
		}
		assert.Equal(t, fmt.Sprintf("Round %d: you sit out this week", draft.Number), list[0].Title)
		assert.Equal(t, rounds.ByeSentence(settings.OddPoolByeOnly), list[0].Body)
	}
	assert.Empty(t, noticesOf(t, tx, paused), "a paused player's exclusion is on their dashboard, not in a notice")
}
