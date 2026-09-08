//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// testQueries opens a transaction against TEST_DATABASE_URL and rolls
// it back when the test ends — mirrors internal/standings' own helper;
// duplicated rather than shared, since sharing it would mean either
// package importing the other's test-only code across package
// boundaries for no real benefit at this size.
func testQueries(t *testing.T) *gen.Queries {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	return gen.New(tx)
}

func createUser(t *testing.T, q *gen.Queries, username string) gen.User {
	t.Helper()
	ctx := context.Background()
	user, err := q.CreateApprovedUser(ctx, gen.CreateApprovedUserParams{
		LichessUsername: username,
		LichessUserID:   username,
	})
	require.NoError(t, err)
	_, err = q.CreatePlayerProfile(ctx, user.ID)
	require.NoError(t, err)
	return user
}

func createPendingPairing(t *testing.T, q *gen.Queries, roundNumber int32, white, black gen.User, gameID *string) gen.Pairing {
	t.Helper()
	ctx := context.Background()
	ts := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	round, err := q.CreateRound(ctx, gen.CreateRoundParams{
		Number:      roundNumber,
		State:       gen.RoundStatePublished,
		PublishAt:   ts,
		PublishedAt: ts,
		PairAt:      ts,
		GeneratedBy: gen.RoundSourceImported,
	})
	require.NoError(t, err)
	pairing, err := q.UpsertManualPairing(ctx, gen.UpsertManualPairingParams{
		RoundID:        round.ID,
		WhiteUserID:    white.ID,
		BlackUserID:    black.ID,
		CreationMethod: gen.PairingMethodManualExternal,
		LichessGameID:  gameID,
	})
	require.NoError(t, err)
	return pairing
}

func fakeGame(id string, white, black gen.User, status, winner string) lichess.Game {
	g := lichess.Game{
		ID:          id,
		Rated:       true,
		Variant:     "standard",
		DaysPerTurn: 2,
		Status:      status,
		Winner:      winner,
		CreatedAt:   time.Now().Add(-time.Hour).UnixMilli(),
		LastMoveAt:  time.Now().UnixMilli(),
	}
	g.Players.White = lichess.GamePlayer{User: &lichess.GamePlayerUser{ID: white.LichessUserID}, Rating: 1500}
	g.Players.Black = lichess.GamePlayer{User: &lichess.GamePlayerUser{ID: black.LichessUserID}, Rating: 1500}
	return g
}

func TestDoSyncGames_RechecksKnownGameIDAndFinishesIt(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	white := createUser(t, q, "rechecksyncwhite")
	black := createUser(t, q, "rechecksyncblack")
	gameID := "rechecksync1"
	createPendingPairing(t, q, 80001, white, black, &gameID)

	fake := lichess.NewFake()
	fake.Games[gameID] = fakeGame(gameID, white, black, lichess.StatusResign, "white")

	stats, err := doSyncGames(ctx, q, fake, settings.Defaults())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.InProgressChecked)
	assert.Equal(t, 1, stats.GamesFinished)

	game, err := q.ListFinishedGamesForUser(ctx, white.ID)
	require.NoError(t, err)
	require.Len(t, game, 1)
	assert.Equal(t, gen.GameResultWhiteWin, *game[0].Result)

	whiteStanding, err := q.GetPlayerStanding(ctx, white.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(1), whiteStanding.Wins)
}

func TestDoSyncGames_MatchesAnUnmatchedPairing(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	white := createUser(t, q, "matchsyncwhite")
	black := createUser(t, q, "matchsyncblack")
	createPendingPairing(t, q, 80002, white, black, nil) // no game id yet

	fake := lichess.NewFake()
	game := fakeGame("matchsync1", white, black, lichess.StatusMate, "black")
	fake.Games[game.ID] = game

	stats, err := doSyncGames(ctx, q, fake, settings.Defaults())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PairingsMatched)
	assert.Equal(t, 1, stats.GamesFinished)

	blackStanding, err := q.GetPlayerStanding(ctx, black.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(1), blackStanding.Wins)
}

func TestDoSyncGames_AmbiguousMatchFlagsThePairingAndAttachesNothing(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	white := createUser(t, q, "ambigsyncwhite")
	black := createUser(t, q, "ambigsyncblack")
	createPendingPairing(t, q, 80003, white, black, nil)

	fake := lichess.NewFake()
	fake.Games["ambig1"] = fakeGame("ambig1", white, black, lichess.StatusMate, "white")
	fake.Games["ambig2"] = fakeGame("ambig2", white, black, lichess.StatusResign, "black")

	stats, err := doSyncGames(ctx, q, fake, settings.Defaults())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PairingsAmbiguous)
	assert.Equal(t, 0, stats.GamesFinished)

	got, err := q.ListUnmatchedPairings(ctx)
	require.NoError(t, err)
	assert.Empty(t, got, "an ambiguous pairing must not still show up as unmatched — it's flagged, not retried every run")

	_, err = q.GetPlayerStanding(ctx, white.ID)
	assert.Error(t, err) // never computed — nothing was ever matched, so Recompute was never called
}

func TestDoSyncGames_LichessErrorOnOnePlayerDoesNotFailTheWholeJob(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	// Two independent unmatched pairings, two different white players:
	// one Lichess call fails, the other succeeds — the job must still
	// report success and still make the progress it could (spec §7.2).
	goodWhite := createUser(t, q, "gracefulsyncwhite1")
	goodBlack := createUser(t, q, "gracefulsyncblack1")
	createPendingPairing(t, q, 80004, goodWhite, goodBlack, nil)

	badWhite := createUser(t, q, "gracefulsyncwhite2")
	badBlack := createUser(t, q, "gracefulsyncblack2")
	createPendingPairing(t, q, 80005, badWhite, badBlack, nil)

	fake := lichess.NewFake()
	goodGame := fakeGame("gracefulgame1", goodWhite, goodBlack, lichess.StatusMate, "white")
	fake.Games[goodGame.ID] = goodGame
	fake.UserGamesErrFor = map[string]error{
		badWhite.LichessUserID: &lichess.APIError{StatusCode: 404, Message: "Not found"},
	}

	stats, err := doSyncGames(ctx, q, fake, settings.Defaults())
	require.NoError(t, err) // the job itself must not error out
	assert.Equal(t, 1, stats.PairingsMatched)
	assert.Equal(t, 1, stats.LichessCallsFailed)
	assert.GreaterOrEqual(t, stats.LichessCallsOK, 1)

	_, err = q.GetPlayerStanding(ctx, badWhite.ID)
	assert.Error(t, err) // the failed pairing was never touched
}
