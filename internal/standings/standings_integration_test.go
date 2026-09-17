//go:build integration

package standings_test

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
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// testQueries opens a transaction against TEST_DATABASE_URL and rolls
// it back when the test ends, so integration tests never need manual
// cleanup and never pollute the dev database regardless of pass/fail.
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

func createTestUser(t *testing.T, q *gen.Queries, username string) gen.User {
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

// createTestPairing creates a fresh round and a pairing between white
// and black, returning the pairing (round numbers must be globally
// unique, so each call needs its own).
func createTestPairing(t *testing.T, q *gen.Queries, roundNumber int32, white, black gen.User) gen.Pairing {
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
	})
	require.NoError(t, err)
	return pairing
}

// upsertFinishedGame writes a minimal finished games row directly
// (bypassing internal/ingest, whose own mapping is already covered by
// its own tests) so this test can focus on standings.Recompute.
func upsertFinishedGame(
	t *testing.T,
	q *gen.Queries,
	pairing gen.Pairing,
	roundNumber int32,
	white, black gen.User,
	gameID string,
	result gen.GameResult,
	finishedAt time.Time,
) {
	t.Helper()
	termination := gen.GameTerminationResign
	whiteRating, blackRating := int32(1500), int32(1500)
	_, err := q.UpsertGame(context.Background(), gen.UpsertGameParams{
		LichessGameID:     gameID,
		PairingID:         pairing.ID,
		RoundNumber:       roundNumber,
		WhiteUserID:       white.ID,
		BlackUserID:       black.ID,
		Status:            gen.GameStatusFinished,
		Result:            &result,
		Termination:       &termination,
		LichessStatus:     "resign",
		WhiteRatingAtGame: &whiteRating,
		BlackRatingAtGame: &blackRating,
		StartedAt:         pgtype.Timestamptz{Time: finishedAt.Add(-time.Hour), Valid: true},
		LastMoveAt:        pgtype.Timestamptz{Time: finishedAt, Valid: true},
		FinishedAt:        pgtype.Timestamptz{Time: finishedAt, Valid: true},
		RawPayload:        []byte(`{}`),
	})
	require.NoError(t, err)

	// Ingestion always attaches the game to its pairing (sync-games),
	// and a pairing left pending with no game id is counted as a game
	// in flight (spec §5.8, decided 2026-09-17) — so a fixture that
	// skipped this step would make a finished game look ongoing.
	require.NoError(t, q.AttachGameToPairing(context.Background(), gen.AttachGameToPairingParams{
		ID:            pairing.ID,
		LichessGameID: &gameID,
		Status:        gen.PairingStatusCompleted,
	}))
}

func TestRecompute_ComputesFromFinishedGames(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	white := createTestUser(t, q, "standingswhite")
	black := createTestUser(t, q, "standingsblack")

	now := time.Now()
	p1 := createTestPairing(t, q, 90001, white, black)
	upsertFinishedGame(t, q, p1, 90001, white, black, "standtest1", gen.GameResultWhiteWin, now.Add(-2*time.Hour))
	p2 := createTestPairing(t, q, 90002, white, black)
	upsertFinishedGame(t, q, p2, 90002, white, black, "standtest2", gen.GameResultDraw, now.Add(-1*time.Hour))

	cfg := settings.Defaults()
	_, err := standings.Recompute(ctx, q, white.ID, cfg)
	require.NoError(t, err)

	got, err := q.GetPlayerStanding(ctx, white.ID)
	require.NoError(t, err)

	assert.Equal(t, int32(2), got.GamesPlayed)
	assert.Equal(t, int32(1), got.Wins)
	assert.Equal(t, int32(1), got.Draws)
	assert.Equal(t, int32(0), got.Losses)
	assert.Equal(t, int32(0), got.Ongoing)
	assert.Equal(t, int32(2), got.ColorScore) // both games as white
	assert.Equal(t, int32(5), got.Xp)         // win(3) + draw(2)
	assert.Equal(t, int32(2), got.Level)      // floor(sqrt(5)) = 2
	assert.True(t, got.IsUnrated)             // no rating_snapshots row seeded
	assert.Nil(t, got.Rating)
	assert.True(t, got.IsEligible) // approved + active (profile default)
	require.NotNil(t, got.LastKPerfRating)
}

func TestRecompute_IsIdempotent(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	white := createTestUser(t, q, "idempotentwhite")
	black := createTestUser(t, q, "idempotentblack")
	p := createTestPairing(t, q, 90003, white, black)
	upsertFinishedGame(t, q, p, 90003, white, black, "standtest3", gen.GameResultWhiteWin, time.Now())

	cfg := settings.Defaults()
	_, err := standings.Recompute(ctx, q, white.ID, cfg)
	require.NoError(t, err)
	first, err := q.GetPlayerStanding(ctx, white.ID)
	require.NoError(t, err)

	_, err = standings.Recompute(ctx, q, white.ID, cfg)
	require.NoError(t, err)
	second, err := q.GetPlayerStanding(ctx, white.ID)
	require.NoError(t, err)

	assert.Equal(t, first.GamesPlayed, second.GamesPlayed)
	assert.Equal(t, first.Xp, second.Xp)
	assert.Equal(t, first.PowerRating, second.PowerRating)
}

func TestRecompute_LevelUpSetsLastLevelUpFields(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()

	white := createTestUser(t, q, "levelupwhite")
	black := createTestUser(t, q, "levelupblack")
	cfg := settings.Defaults()

	// Baseline: no games yet, first computation is never itself a level up.
	leveledUp, err := standings.Recompute(ctx, q, white.ID, cfg)
	require.NoError(t, err)
	assert.False(t, leveledUp)
	baseline, err := q.GetPlayerStanding(ctx, white.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(0), baseline.Level)

	// One win (xp=3, level=1) crosses the level-0-to-1 boundary.
	p := createTestPairing(t, q, 90004, white, black)
	upsertFinishedGame(t, q, p, 90004, white, black, "standtest4", gen.GameResultWhiteWin, time.Now())

	leveledUp, err = standings.Recompute(ctx, q, white.ID, cfg)
	require.NoError(t, err)
	assert.True(t, leveledUp)

	after, err := q.GetPlayerStanding(ctx, white.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(1), after.Level)
	require.NotNil(t, after.LastLevelUpRound)
	assert.Equal(t, int32(90004), *after.LastLevelUpRound)
	assert.True(t, after.LastLevelUpAt.Valid)
}
