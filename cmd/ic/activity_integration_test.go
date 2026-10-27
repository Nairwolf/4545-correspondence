//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// publishedRound writes a round published at publishedAt, two days per
// move and rated, with one by-hand pairing of white against black.
func publishedRound(
	t *testing.T,
	q *gen.Queries,
	publishedAt time.Time,
	white, black gen.User,
) (gen.Round, gen.Pairing) {
	t.Helper()
	ctx := context.Background()
	number, err := q.NextRoundNumber(ctx)
	require.NoError(t, err)
	at := pgtype.Timestamptz{Time: publishedAt, Valid: true}
	round, err := q.CreateGeneratedRound(ctx, gen.CreateGeneratedRoundParams{
		Number:       number,
		State:        gen.RoundStatePublished,
		PublishAt:    at,
		PublishedAt:  at,
		PairAt:       at,
		GeneratedBy:  gen.RoundSourceSchedule,
		SettingsUsed: []byte(`{"days_per_move": 2, "rated": true}`),
	})
	require.NoError(t, err)
	p, err := q.InsertGeneratedPairing(ctx, gen.InsertGeneratedPairingParams{
		RoundID:     round.ID,
		WhiteUserID: white.ID,
		BlackUserID: black.ID,
	})
	require.NoError(t, err)
	return round, p
}

// challengeSent turns a by-hand pairing into the challenge the
// challenger's token sent on the Fake, as create-games records one.
func (tt tokenTest) challengeSent(
	t *testing.T,
	p gen.Pairing,
	challenger gen.User,
	secret tokencrypt.Secret,
	opponent string,
) string {
	t.Helper()
	ch, err := tt.fake.CreateChallenge(
		context.Background(),
		secret,
		opponent,
		lichess.ChallengeRequest{Days: 2, Rated: true, Color: lichess.ColorWhite},
	)
	require.NoError(t, err)
	id := ch.ID
	n, err := tt.q.RecordChallenge(context.Background(), gen.RecordChallengeParams{
		ID:               p.ID,
		ChallengerUserID: challenger.ID,
		LichessGameID:    &id,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	tt.fake.Calls = nil
	return id
}

func cancelDetail(t *testing.T, run gen.JobRun) cancelChallengeResult {
	t.Helper()
	var detail cancelChallengeResult
	require.NoError(t, json.Unmarshal(run.Detail, &detail))
	return detail
}

// --- cancel-challenge ------------------------------------------------

func TestRunCancelChallenge_WithdrawsWithTheChallengersToken(t *testing.T) {
	// Spec §6.4: the missed challenge is withdrawn on Lichess, so a late
	// acceptance cannot create a game no pairing counts. Only its sender
	// can withdraw it.
	tt := newTokenTest(t)
	alice := tt.player(t, "alice", "tok-alice", []string{lichess.ScopeChallengeWrite})
	bob := createUser(t, tt.q, "bob")
	_, p := publishedRound(t, tt.q, time.Now().Add(-7*24*time.Hour), alice, bob)
	id := tt.challengeSent(t, p, alice, "tok-alice", "bob")
	_, err := tt.tx.Exec(context.Background(), `UPDATE pairings SET status = 'failed' WHERE id = $1`, p.ID)
	require.NoError(t, err)
	riverJobID := time.Now().UnixNano()

	require.NoError(t, runCancelChallenge(context.Background(), tt.tx, tt.fake, tt.key, p.ID, &riverJobID))

	assert.Equal(t, "canceled", tt.fake.Challenges[0].Status)
	require.Len(t, tt.fake.Calls, 1)
	assert.Equal(t, lichess.FakeCall{Method: "CancelChallenge", Token: "tok-alice"}, tt.fake.Calls[0])
	run := jobRunByRiverID(t, tt.q, riverJobID)
	assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
	assert.Equal(t, cancelChallengeResult{Challenge: id, Challenger: "alice", Outcome: "withdrawn"}, cancelDetail(t, run))
}

func TestRunCancelChallenge_AChallengeLichessNoLongerHasNeedsNothing(t *testing.T) {
	// Declined, withdrawn already, or a game whose players have moved:
	// Lichess answers 404, and there is nothing left to do.
	tt := newTokenTest(t)
	alice := tt.player(t, "alice", "tok-alice", []string{lichess.ScopeChallengeWrite})
	bob := createUser(t, tt.q, "bob")
	_, p := publishedRound(t, tt.q, time.Now().Add(-7*24*time.Hour), alice, bob)
	tt.challengeSent(t, p, alice, "tok-alice", "bob")
	_, err := tt.tx.Exec(context.Background(), `UPDATE pairings SET status = 'failed' WHERE id = $1`, p.ID)
	require.NoError(t, err)
	tt.fake.Challenges = nil
	riverJobID := time.Now().UnixNano()

	require.NoError(t, runCancelChallenge(context.Background(), tt.tx, tt.fake, tt.key, p.ID, &riverJobID))

	run := jobRunByRiverID(t, tt.q, riverJobID)
	assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
	assert.Equal(t, "gone", cancelDetail(t, run).Outcome)
}

func TestRunCancelChallenge_WithoutAWorkingTokenFailsForGood(t *testing.T) {
	// The challenger's token lapsed since: nothing can withdraw the
	// challenge, and a retry would fail the same way. The run fails,
	// visibly, and river is told not to retry it.
	tt := newTokenTest(t)
	alice := tt.player(t, "alice", "tok-alice", []string{lichess.ScopeChallengeWrite})
	bob := createUser(t, tt.q, "bob")
	_, p := publishedRound(t, tt.q, time.Now().Add(-7*24*time.Hour), alice, bob)
	tt.challengeSent(t, p, alice, "tok-alice", "bob")
	_, err := tt.tx.Exec(context.Background(), `UPDATE pairings SET status = 'failed' WHERE id = $1`, p.ID)
	require.NoError(t, err)
	_, err = tt.tx.Exec(context.Background(), `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, alice.ID)
	require.NoError(t, err)
	riverJobID := time.Now().UnixNano()

	err = runCancelChallenge(context.Background(), tt.tx, tt.fake, tt.key, p.ID, &riverJobID)

	var cancel *river.JobCancelError
	require.True(t, errors.As(err, &cancel), "not retried: %v", err)
	assert.Empty(t, tt.fake.Calls)
	run := jobRunByRiverID(t, tt.q, riverJobID)
	assert.Equal(t, gen.JobRunStatusFailed, run.Status)
	require.NotNil(t, run.Error)
	assert.Contains(t, *run.Error, "alice's token no longer works")
	assert.NotContains(t, *run.Error, "tok-alice")
}

func TestRunCancelChallenge_ALichessFailureIsRetried(t *testing.T) {
	tt := newTokenTest(t)
	alice := tt.player(t, "alice", "tok-alice", []string{lichess.ScopeChallengeWrite})
	bob := createUser(t, tt.q, "bob")
	_, p := publishedRound(t, tt.q, time.Now().Add(-7*24*time.Hour), alice, bob)
	tt.challengeSent(t, p, alice, "tok-alice", "bob")
	_, err := tt.tx.Exec(context.Background(), `UPDATE pairings SET status = 'failed' WHERE id = $1`, p.ID)
	require.NoError(t, err)
	tt.fake.CancelErr = &lichess.APIError{StatusCode: 503, Message: "down"}
	riverJobID := time.Now().UnixNano()

	err = runCancelChallenge(context.Background(), tt.tx, tt.fake, tt.key, p.ID, &riverJobID)

	require.Error(t, err)
	var cancel *river.JobCancelError
	assert.False(t, errors.As(err, &cancel), "a retry may succeed")
	assert.Equal(t, gen.JobRunStatusFailed, jobRunByRiverID(t, tt.q, riverJobID).Status)
}

func TestRunCancelChallenge_SkipsWhatIsNotAMissedChallenge(t *testing.T) {
	tt := newTokenTest(t)
	alice := tt.player(t, "alice", "tok-alice", []string{lichess.ScopeChallengeWrite})
	bob := createUser(t, tt.q, "bob")
	_, byHand := publishedRound(t, tt.q, time.Now().Add(-7*24*time.Hour), alice, bob)
	_, pending := publishedRound(t, tt.q, time.Now().Add(-7*24*time.Hour), alice, bob)
	tt.challengeSent(t, pending, alice, "tok-alice", "bob")

	for _, p := range []gen.Pairing{byHand, pending} {
		riverJobID := time.Now().UnixNano()
		require.NoError(t, runCancelChallenge(context.Background(), tt.tx, tt.fake, tt.key, p.ID, &riverJobID))
		assert.Equal(t, "skipped", cancelDetail(t, jobRunByRiverID(t, tt.q, riverJobID)).Outcome)
	}
	assert.Empty(t, tt.fake.Calls)
}

// --- generation evaluates first ----------------------------------------

// isolatePool keeps whatever the database already holds out of a
// generation: no approved player, pending pairing or draft but the
// test's own. All of it rolls back with the transaction.
func isolatePool(t *testing.T, tx pgx.Tx) {
	t.Helper()
	for _, stmt := range []string{
		`UPDATE users SET status = 'pending' WHERE status = 'approved'`,
		`UPDATE pairings SET status = 'cancelled' WHERE status = 'pending'`,
		`UPDATE rounds SET state = 'cancelled' WHERE state = 'draft'`,
	} {
		_, err := tx.Exec(context.Background(), stmt)
		require.NoError(t, err)
	}
}

func TestRunGenerateRound_RetiresUnstartedPairingsFirst(t *testing.T) {
	// Spec §6.4: evaluate-activity runs before the pool is read, so a
	// pairing nobody started no longer counts towards a cap. Alice's
	// limit is one game; last week's unstarted pairing would otherwise
	// keep her out of this week's round.
	tx := testTx(t)
	isolatePool(t, tx)
	q := gen.New(tx)
	ctx := context.Background()
	alice, bob := createUser(t, q, "alice"), createUser(t, q, "bob")
	_, err := q.SetPlayerCapacity(ctx, gen.SetPlayerCapacityParams{UserID: alice.ID, MaxConcurrentGames: ptr(int32(1))})
	require.NoError(t, err)
	_, unstarted := publishedRound(t, q, time.Now().Add(-7*24*time.Hour), alice, bob)

	require.NoError(t, runGenerateRound(ctx, tx, lichess.NewFake(), gen.RoundSourceManual, nil, nil))

	got, err := q.GetPairingByID(ctx, gen.GetPairingByIDParams{ID: unstarted.ID, RoundID: unstarted.RoundID})
	require.NoError(t, err)
	assert.Equal(t, gen.PairingStatusFailed, got.Status)
	draft, err := q.GetDraftRound(ctx)
	require.NoError(t, err)
	pairings, err := q.ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	assert.Len(t, pairings, 1, "alice is paired: her cap no longer counts last week's pairing")
}

func TestRunGenerateRound_AFailedEvaluationDoesNotStopTheRound(t *testing.T) {
	tt := newTokenTest(t)
	isolatePool(t, tt.tx)
	ctx := context.Background()
	alice := tt.player(t, "alice", "tok-alice", []string{lichess.ScopeChallengeWrite})
	bob := createUser(t, tt.q, "bob")
	_, p := publishedRound(t, tt.q, time.Now().Add(-7*24*time.Hour), alice, bob)
	tt.challengeSent(t, p, alice, "tok-alice", "bob")
	tt.fake.Err = errors.New("lichess: 503")
	riverJobID := time.Now().UnixNano()

	require.NoError(t, runGenerateRound(ctx, tt.tx, tt.fake, gen.RoundSourceManual, nil, &riverJobID))

	_, err := tt.q.GetDraftRound(ctx)
	require.NoError(t, err, "the round is generated")
	runs, err := tt.q.ListRecentJobRuns(ctx, 200)
	require.NoError(t, err)
	statuses := map[string]gen.JobRunStatus{}
	for _, run := range runs {
		if run.RiverJobID != nil && *run.RiverJobID == riverJobID {
			statuses[run.JobName] = run.Status
		}
	}
	assert.Equal(t, map[string]gen.JobRunStatus{
		"evaluate-activity": gen.JobRunStatusFailed,
		"generate-round":    gen.JobRunStatusSucceeded,
	}, statuses, "both runs belong to the one river job")
}

// --- the 48-hour reminder ------------------------------------------------

func TestDoSyncGames_RemindsTheUnstartedOnce(t *testing.T) {
	tt := newTokenTest(t)
	ctx := context.Background()
	alice, bob := createUser(t, tt.q, "alice"), createUser(t, tt.q, "bob")
	round, p := publishedRound(t, tt.q, time.Now().Add(-49*time.Hour), alice, bob)

	stats, err := doSyncGames(ctx, tt.q, tt.fake, mustSettings(t, tt.q))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, stats.Unstarted, 1)
	_, err = doSyncGames(ctx, tt.q, tt.fake, mustSettings(t, tt.q))
	require.NoError(t, err)

	for _, player := range []gen.User{alice, bob} {
		notices := tt.notices(t, player)
		require.Len(t, notices, 1, player.LichessUsername)
		assert.Equal(t, string(notify.Unstarted), notices[0].Category)
		assert.Equal(t, "round:"+strconv.Itoa(int(round.Number))+":unstarted:"+p.ID.String(), *notices[0].DedupeKey)
	}
}

func TestDoSyncGames_NoReminderWhileAGameCouldNotBeLookedFor(t *testing.T) {
	// A game the run could not look for may have started: no reminder
	// that it hasn't, until a run that could.
	tt := newTokenTest(t)
	ctx := context.Background()
	alice, bob := createUser(t, tt.q, "alice"), createUser(t, tt.q, "bob")
	publishedRound(t, tt.q, time.Now().Add(-49*time.Hour), alice, bob)
	tt.fake.UserGamesErrFor = map[string]error{"alice": errors.New("lichess: 503")}

	stats, err := doSyncGames(ctx, tt.q, tt.fake, mustSettings(t, tt.q))
	require.NoError(t, err)

	assert.Zero(t, stats.Unstarted)
	assert.Empty(t, tt.notices(t, alice))
	assert.Empty(t, tt.notices(t, bob))
}
