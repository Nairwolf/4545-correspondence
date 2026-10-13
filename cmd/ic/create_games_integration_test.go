//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/jobs"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// creatingRound writes a round as its publication under
// pairing.game_creation = lichess leaves it: published, at two days per
// move, rated, its games being created.
func creatingRound(t *testing.T, q *gen.Queries, number int32, pairs ...[2]gen.User) (gen.Round, []gen.Pairing) {
	t.Helper()
	ctx := context.Background()
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	round, err := q.CreateGeneratedRound(ctx, gen.CreateGeneratedRoundParams{
		Number:       number,
		State:        gen.RoundStatePublished,
		PublishAt:    now,
		PublishedAt:  now,
		PairAt:       now,
		GeneratedBy:  gen.RoundSourceSchedule,
		SettingsUsed: []byte(`{"days_per_move": 2, "rated": true}`),
	})
	require.NoError(t, err)
	var pairings []gen.Pairing
	for _, pair := range pairs {
		p, err := q.InsertGeneratedPairing(ctx, gen.InsertGeneratedPairingParams{
			RoundID:     round.ID,
			WhiteUserID: pair[0].ID,
			BlackUserID: pair[1].ID,
		})
		require.NoError(t, err)
		pairings = append(pairings, p)
	}
	round, err = q.StartGameCreation(ctx, gen.StartGameCreationParams{
		ID:           round.ID,
		GameCreation: gen.GameCreationStateCreating,
	})
	require.NoError(t, err)
	return round, pairings
}

func (tt tokenTest) creator() rounds.GameCreator {
	return rounds.GameCreator{Lichess: tt.fake, Key: tt.key, Organiser: "tok-organiser"}
}

func (tt tokenTest) gameCreation(t *testing.T) {
	t.Helper()
	_, err := tt.q.UpsertSetting(context.Background(), gen.UpsertSettingParams{
		Key:   "pairing.game_creation",
		Value: []byte(`"lichess"`),
	})
	require.NoError(t, err)
}

func createGamesDetail(t *testing.T, run gen.JobRun) createGamesResult {
	t.Helper()
	var detail createGamesResult
	require.NoError(t, json.Unmarshal(run.Detail, &detail))
	return detail
}

var writeScope = []string{lichess.ScopeChallengeWrite}

func TestRunCreateGames_RecordsWhatItCreated(t *testing.T) {
	tt := newTokenTest(t)
	tt.gameCreation(t)
	ctx := context.Background()
	white := tt.player(t, "cgwhite", "tok-cgwhite", writeScope)
	black := tt.player(t, "cgblack", "tok-cgblack", writeScope)
	round, pairings := creatingRound(t, tt.q, 9701, [2]gen.User{white, black})

	require.NoError(t, runCreateGames(ctx, tt.tx, tt.creator(), jobs.CreateGamesArgs{RoundID: round.ID}, false, ptr(int64(9701))))

	run := jobRunByRiverID(t, tt.q, 9701)
	assert.Equal(t, "create-games", run.JobName)
	assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
	assert.Nil(t, run.Error)
	assert.EqualValues(t, 1, run.ItemsProcessed)
	detail := createGamesDetail(t, run)
	assert.Equal(t, 1, detail.Created)
	assert.Equal(t, gen.GameCreationStateCreated, detail.State)
	assert.Equal(t, []string{tt.fake.Bulks[0].ID}, detail.Bulks)
	assert.Nil(t, detail.Search, "only a retry searches first")

	p, err := tt.q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: pairings[0].ID, RoundID: round.ID})
	require.NoError(t, err)
	assert.Equal(t, gen.PairingStatusCreated, p.Status)
}

func TestRunCreateGames_TheLastAttemptGivesTheGamesUpToThePlayers(t *testing.T) {
	// Spec §6.3: river retries a failed run; when the last attempt
	// fails, the players are told to start their games by hand.
	tt := newTokenTest(t)
	tt.gameCreation(t)
	ctx := context.Background()
	white := tt.player(t, "cgwhite", "tok-cgwhite", writeScope)
	black := tt.player(t, "cgblack", "tok-cgblack", writeScope)
	round, _ := creatingRound(t, tt.q, 9702, [2]gen.User{white, black})
	tt.fake.CreateBulkErr = errors.New("lichess: 503 Service Unavailable")
	args := jobs.CreateGamesArgs{RoundID: round.ID}

	err := runCreateGames(ctx, tt.tx, tt.creator(), args, false, ptr(int64(9702)))
	require.Error(t, err)
	run := jobRunByRiverID(t, tt.q, 9702)
	assert.Equal(t, gen.JobRunStatusFailed, run.Status)
	require.NotNil(t, run.Error)
	assert.Contains(t, *run.Error, "503")
	assert.NotContains(t, *run.Error, "tok-", "never a token in the error text")
	still, err := tt.q.GetRoundByID(ctx, round.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.GameCreationStateCreating, still.GameCreation, "an attempt with retries left only fails")
	assert.Empty(t, tt.notices(t, white))

	err = runCreateGames(ctx, tt.tx, tt.creator(), args, true, ptr(int64(9703)))
	require.Error(t, err)
	run = jobRunByRiverID(t, tt.q, 9703)
	assert.Equal(t, gen.JobRunStatusFailed, run.Status, "the games were not created")
	detail := createGamesDetail(t, run)
	assert.True(t, detail.GaveUp)
	assert.Equal(t, gen.GameCreationStateGaveUp, detail.State)
	given, err := tt.q.GetRoundByID(ctx, round.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.GameCreationStateGaveUp, given.GameCreation)
	for _, user := range []gen.User{white, black} {
		notices := tt.notices(t, user)
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0].Body, "start it by hand")
	}
}

func TestRunCreateGames_ARetryAttachesAGameStartedByHand(t *testing.T) {
	// Spec §6.3 step 2: after giving up, a game the players started by
	// hand is attached, not created a second time; the others are
	// created, and their players told not to start another.
	tt := newTokenTest(t)
	tt.gameCreation(t)
	ctx := context.Background()
	a := tt.player(t, "cga", "tok-cga", writeScope)
	b := tt.player(t, "cgb", "tok-cgb", writeScope)
	c := tt.player(t, "cgc", "tok-cgc", writeScope)
	d := tt.player(t, "cgd", "tok-cgd", writeScope)
	round, pairings := creatingRound(t, tt.q, 9704, [2]gen.User{a, b}, [2]gen.User{c, d})
	_, err := giveUpInTx(ctx, tt.tx, round.ID)
	require.NoError(t, err)
	tt.fake.Games["byhand1"] = fakeGame("byhand1", a, b, lichess.StatusStarted, "")

	_, err = rounds.RetryGameCreation(ctx, tt.tx, round.ID, mustSettings(t, tt.q), pgtype.UUID{}, noQueue{})
	require.NoError(t, err)
	require.NoError(t, runCreateGames(ctx, tt.tx, tt.creator(), jobs.CreateGamesArgs{RoundID: round.ID, Retry: true}, false, ptr(int64(9704))))

	run := jobRunByRiverID(t, tt.q, 9704)
	assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
	detail := createGamesDetail(t, run)
	require.NotNil(t, detail.Search)
	assert.Equal(t, 1, detail.Search.PairingsMatched)
	assert.Equal(t, 1, detail.Created)

	byHand, err := tt.q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: pairings[0].ID, RoundID: round.ID})
	require.NoError(t, err)
	require.NotNil(t, byHand.LichessGameID)
	assert.Equal(t, "byhand1", *byHand.LichessGameID)
	assert.Equal(t, gen.PairingMethodManualExternal, byHand.CreationMethod)
	assert.Equal(t, gen.PairingStatusInProgress, byHand.Status)

	require.Len(t, tt.fake.BulkRequests, 1)
	assert.Len(t, tt.fake.BulkRequests[0].Pairs, 1, "only the game nobody started")
	created, err := tt.q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: pairings[1].ID, RoundID: round.ID})
	require.NoError(t, err)
	assert.Equal(t, gen.PairingMethodBulk, created.CreationMethod)

	titles := func(user gen.User) []string {
		var out []string
		for _, n := range tt.notices(t, user) {
			out = append(out, n.Title)
		}
		return out
	}
	assert.Len(t, titles(a), 1, "only the give-up notice: their game was theirs all along")
	assert.Contains(t, titles(c), "Round 9704: your game against cgd has now been created")
}

func TestRunCreateGames_ARetryThatCannotSearchCreatesNothing(t *testing.T) {
	// A game the search could not look for may exist: creating it again
	// could give the players two.
	tt := newTokenTest(t)
	tt.gameCreation(t)
	ctx := context.Background()
	a := tt.player(t, "cga", "tok-cga", writeScope)
	b := tt.player(t, "cgb", "tok-cgb", writeScope)
	round, _ := creatingRound(t, tt.q, 9705, [2]gen.User{a, b})
	tt.fake.UserGamesErrFor = map[string]error{"cga": errors.New("lichess: 500")}

	err := runCreateGames(ctx, tt.tx, tt.creator(), jobs.CreateGamesArgs{RoundID: round.ID, Retry: true}, false, ptr(int64(9705)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "search for games started by hand")
	assert.Empty(t, tt.fake.BulkRequests)
}

func TestQueueCreateGames_TheCLIQueuesARetryForServe(t *testing.T) {
	tt := newTokenTest(t)
	tt.gameCreation(t)
	ctx := context.Background()
	a := tt.player(t, "cga", "tok-cga", writeScope)
	b := tt.player(t, "cgb", "tok-cgb", writeScope)
	round, _ := creatingRound(t, tt.q, 9706, [2]gen.User{a, b})
	_, err := giveUpInTx(ctx, tt.tx, round.ID)
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	enqueuer, err := jobs.NewInsertClient(pool)
	require.NoError(t, err)
	sched := jobs.NewScheduler(enqueuer)

	require.NoError(t, queueCreateGames(ctx, tt.tx, sched, "9706"))
	var retry bool
	err = tt.tx.QueryRow(ctx, `
		SELECT (args->>'retry')::bool FROM river_job
		WHERE kind = 'create-games' AND (args->>'round_id')::int = $1`, round.ID).Scan(&retry)
	require.NoError(t, err, "queued for serve")
	assert.True(t, retry)
	after, err := tt.q.GetRoundByID(ctx, round.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.GameCreationStateCreating, after.GameCreation)

	err = queueCreateGames(ctx, tt.tx, sched, "9706")
	require.ErrorIs(t, err, rounds.ErrAlreadyQueued)
	assert.Empty(t, tt.fake.Calls, "the command itself never calls Lichess")
}

// noQueue accepts every job and keeps none: the tests that call the job
// themselves only need the round claimed.
type noQueue struct{ rounds.Scheduler }

func (noQueue) ScheduleCreateGames(context.Context, pgx.Tx, int32, bool) (bool, error) {
	return true, nil
}

func mustSettings(t *testing.T, q *gen.Queries) settings.Settings {
	t.Helper()
	cfg, err := settings.Load(context.Background(), q)
	require.NoError(t, err)
	return cfg
}

func ptr[T any](v T) *T { return &v }
