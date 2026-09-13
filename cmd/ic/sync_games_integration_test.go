//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	return gen.New(testTx(t))
}

// testTx is testQueries' transaction, for the tests that need to hand it
// to runSyncGames directly or read rows no generated query exposes.
func testTx(t *testing.T) pgx.Tx {
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

	return tx
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

func TestDoSyncGames_StoresLichessBytesAsRawPayload(t *testing.T) {
	// Spec §7.1: raw_payload is what Lichess sent, not what we decoded.
	// The fake emits RawGames bytes verbatim, exactly as the real stream
	// hands back an ndjson line; the field "undeclaredByOurStruct" has no
	// home in lichess.Game and can only reach the database if the bytes
	// are passed through untouched.
	q := testQueries(t)
	ctx := context.Background()

	white := createUser(t, q, "rawsyncwhite")
	black := createUser(t, q, "rawsyncblack")
	gameID := "rawsync01"
	createPendingPairing(t, q, 80006, white, black, &gameID)

	fake := lichess.NewFake()
	g := fakeGame(gameID, white, black, lichess.StatusResign, "white")
	fake.Games[gameID] = g
	base, err := json.Marshal(g)
	require.NoError(t, err)
	// Splice an undeclared field in; the rest stays a faithful encoding
	// of g so decoding still yields the same game.
	fake.RawGames[gameID] = append([]byte(`{"undeclaredByOurStruct":{"kept":true},`), base[1:]...)

	_, err = doSyncGames(ctx, q, fake, settings.Defaults())
	require.NoError(t, err)

	games, err := q.ListFinishedGamesForUser(ctx, white.ID)
	require.NoError(t, err)
	require.Len(t, games, 1)
	assert.JSONEq(t, string(fake.RawGames[gameID]), string(games[0].RawPayload))
	assert.Contains(t, string(games[0].RawPayload), "undeclaredByOurStruct")
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

// recordedRun reads back the job_runs row runSyncGames wrote, found by
// the river job id the test passed in (unique per test).
func recordedRun(t *testing.T, tx pgx.Tx, riverJobID int64) (status string, errText *string, detail syncGamesStats) {
	t.Helper()
	var raw []byte
	err := tx.QueryRow(
		context.Background(),
		"SELECT status::text, error, detail FROM job_runs WHERE river_job_id = $1",
		riverJobID,
	).Scan(&status, &errText, &raw)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &detail))
	return status, errText, detail
}

func TestRunSyncGames_LichessUnreachableIsRecordedAsFailed(t *testing.T) {
	// Before the B-2 fix this run was recorded as succeeded with no error
	// and runSyncGames returned nil, so /health stayed green while Lichess
	// was down.
	tx := testTx(t)
	q := gen.New(tx)
	ctx := context.Background()

	white := createUser(t, q, "downsyncwhite")
	black := createUser(t, q, "downsyncblack")
	gameID := "downsync1"
	createPendingPairing(t, q, 80010, white, black, &gameID) // guarantees at least one call is attempted

	fake := lichess.NewFake()
	fake.Err = &lichess.APIError{StatusCode: 503, Message: "Service Unavailable"}

	riverJobID := time.Now().UnixNano()
	err := runSyncGames(ctx, tx, fake, &riverJobID)
	require.Error(t, err, "a failed run must return an error so river retries it")

	status, errText, detail := recordedRun(t, tx, riverJobID)
	assert.Equal(t, "failed", status)
	require.NotNil(t, errText)
	assert.Contains(t, *errText, "Lichess calls failed")
	assert.Contains(t, *errText, "Service Unavailable")
	assert.Zero(t, detail.LichessCallsOK)
	assert.Positive(t, detail.LichessCallsFailed)
}

func TestRunSyncGames_PartialLichessFailureSucceedsButRecordsTheError(t *testing.T) {
	tx := testTx(t)
	q := gen.New(tx)
	ctx := context.Background()

	goodWhite := createUser(t, q, "partialsyncwhite1")
	goodBlack := createUser(t, q, "partialsyncblack1")
	createPendingPairing(t, q, 80011, goodWhite, goodBlack, nil)
	badWhite := createUser(t, q, "partialsyncwhite2")
	badBlack := createUser(t, q, "partialsyncblack2")
	createPendingPairing(t, q, 80012, badWhite, badBlack, nil)

	fake := lichess.NewFake()
	good := fakeGame("partialsync1", goodWhite, goodBlack, lichess.StatusMate, "white")
	fake.Games[good.ID] = good
	fake.UserGamesErrFor = map[string]error{
		badWhite.LichessUserID: &lichess.APIError{StatusCode: 404, Message: "Not found"},
	}

	riverJobID := time.Now().UnixNano()
	require.NoError(t, runSyncGames(ctx, tx, fake, &riverJobID))

	status, errText, detail := recordedRun(t, tx, riverJobID)
	assert.Equal(t, "succeeded", status)
	require.NotNil(t, errText, "the failed call must still be visible on the run")
	assert.Contains(t, *errText, badWhite.LichessUserID)
	assert.Equal(t, 1, detail.LichessCallsFailed)
	assert.Equal(t, 1, detail.PairingsMatched)
}

func TestDoSyncGames_DatabaseErrorAfterASuccessfulLichessCallIsFatal(t *testing.T) {
	// The other half of B-2: a database write failing after one Lichess
	// call had succeeded was discarded entirely. Here the re-check call
	// succeeds, then ingesting the matched game fails inside Postgres
	// (accuracy overflows numeric(5,2)) after its Lichess fetch completed.
	//
	// This drives doSyncGames rather than runSyncGames: the database
	// error aborts the test transaction, so the job_runs row can't be
	// written back inside it. How the result is recorded is covered by
	// TestSyncGamesOutcome.
	q := testQueries(t)
	ctx := context.Background()

	okWhite := createUser(t, q, "dberrsyncwhite1")
	okBlack := createUser(t, q, "dberrsyncblack1")
	okID := "dberrsync1"
	createPendingPairing(t, q, 80013, okWhite, okBlack, &okID)

	badWhite := createUser(t, q, "dberrsyncwhite2")
	badBlack := createUser(t, q, "dberrsyncblack2")
	createPendingPairing(t, q, 80014, badWhite, badBlack, nil)

	fake := lichess.NewFake()
	fake.Games[okID] = fakeGame(okID, okWhite, okBlack, lichess.StatusStarted, "")
	bad := fakeGame("dberrsync2", badWhite, badBlack, lichess.StatusMate, "white")
	overflow := 1_000_000
	bad.Players.White.Analysis = &lichess.GamePlayerAnalysis{ACPL: 10, Accuracy: &overflow}
	fake.Games[bad.ID] = bad

	stats, err := doSyncGames(ctx, q, fake, settings.Defaults())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "numeric field overflow")
	// Both the re-check and the fetch of the second white player's games
	// completed before the write failed. Before the fix, any non-zero
	// count here is exactly what made the database error disappear.
	assert.Positive(t, stats.LichessCallsOK, "Lichess calls really did succeed before the write failed")

	status, errMsg := syncGamesOutcome(stats, err)
	assert.Equal(t, gen.JobRunStatusFailed, status)
	require.NotNil(t, errMsg)
}

// cancellingLichess cancels the job's context the moment the job makes
// its first Lichess call — the shape of river's job timeout firing, or a
// SIGTERM arriving, mid-run.
type cancellingLichess struct {
	lichess.API
	cancel context.CancelFunc
}

func (c cancellingLichess) UserGames(
	ctx context.Context,
	username string,
	opts lichess.UserGamesOptions,
) (*lichess.GameStream, error) {
	c.cancel()
	return nil, ctx.Err()
}

func TestRunSyncGames_CancelledContextStillRecordsTheOutcome(t *testing.T) {
	// Before the B-3 fix FinishJobRun ran on the cancelled context, failed,
	// and left the row at "running" — visible forever on /health and /jobs.
	// The transaction itself is opened on context.Background(), so what this
	// proves is the context.WithoutCancel around the final write.
	//
	// The re-check call succeeds before the cancellation, so the run is
	// also proven to be failed rather than forgiven as a partial Lichess
	// failure: it stopped with pairings left unprocessed.
	tx := testTx(t)
	q := gen.New(tx)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	white := createUser(t, q, "cancelsyncwhite")
	black := createUser(t, q, "cancelsyncblack")
	knownID := "cancelsync1"
	createPendingPairing(t, q, 80020, white, black, &knownID)
	createPendingPairing(t, q, 80021, white, black, nil)

	fake := lichess.NewFake()
	fake.Games[knownID] = fakeGame(knownID, white, black, lichess.StatusStarted, "")
	client := cancellingLichess{API: fake, cancel: cancel}

	riverJobID := time.Now().UnixNano()
	err := runSyncGames(ctx, tx, client, &riverJobID)
	require.Error(t, err, "a cancelled run must return an error so river retries it")

	status, errText, detail := recordedRun(t, tx, riverJobID)
	assert.Equal(t, "failed", status)
	require.NotNil(t, errText)
	assert.Contains(t, *errText, "interrupted: context canceled")
	assert.Positive(t, detail.LichessCallsOK)
	assert.Positive(t, detail.LichessCallsFailed)
}
