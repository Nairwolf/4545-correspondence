//go:build integration

package activity_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/activity"
	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// world is one test's league: the database, the Fake that plays
// Lichess, the scheduler that records each withdrawal asked for, and
// the moment the evaluation runs at.
type world struct {
	tx    pgx.Tx
	q     *gen.Queries
	fake  *lichess.Fake
	sched *withdrawals
	cfg   settings.Settings
	now   time.Time
}

// withdrawals records the challenges an evaluation asked to withdraw.
type withdrawals struct{ pairings []pgtype.UUID }

func (w *withdrawals) ScheduleCancelChallenge(_ context.Context, _ pgx.Tx, pairingID pgtype.UUID) error {
	w.pairings = append(w.pairings, pairingID)
	return nil
}

func newWorld(t *testing.T) world {
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

	// Whatever the database already holds stays out of the evaluation
	// and the pool: no other pending pairing, approved player or draft.
	// All of it rolls back with the transaction.
	for _, stmt := range []string{
		`UPDATE pairings SET status = 'cancelled' WHERE status = 'pending'`,
		`UPDATE users SET status = 'pending' WHERE status = 'approved'`,
		`UPDATE rounds SET state = 'cancelled' WHERE state = 'draft'`,
	} {
		_, err := tx.Exec(ctx, stmt)
		require.NoError(t, err)
	}

	return world{
		tx:    tx,
		q:     gen.New(tx),
		fake:  lichess.NewFake(),
		sched: &withdrawals{},
		cfg:   settings.Defaults(),
		now:   time.Now(),
	}
}

func (w world) player(t *testing.T, name string) gen.User {
	t.Helper()
	ctx := context.Background()
	user, err := w.q.CreateApprovedUser(ctx, gen.CreateApprovedUserParams{LichessUsername: name, LichessUserID: name})
	require.NoError(t, err)
	_, err = w.q.CreatePlayerProfile(ctx, user.ID)
	require.NoError(t, err)
	require.NoError(t, w.q.UpsertPlayerStanding(ctx, gen.UpsertPlayerStandingParams{
		UserID:      user.ID,
		PowerRating: 1500,
		IsEligible:  true,
	}))
	return user
}

// game is one pairing of a round to write: white plays black, and
// challenger, when set, sent a challenge the other must accept.
type game struct {
	white, black gen.User
	challenger   *gen.User
}

func byHand(white, black gen.User) game { return game{white: white, black: black} }

func challenge(from, to gen.User) game {
	return game{white: from, black: to, challenger: &from}
}

// round writes a round published at publishedAt with these pairings,
// as create-games leaves them: a challenge pairing holds its challenge
// id as its game id and is still pending.
func (w world) round(t *testing.T, publishedAt time.Time, games ...game) (gen.Round, []gen.Pairing) {
	t.Helper()
	ctx := context.Background()
	number, err := w.q.NextRoundNumber(ctx)
	require.NoError(t, err)
	used, err := json.Marshal(rounds.Snapshot{DaysPerMove: 2, Rated: true})
	require.NoError(t, err)
	at := pgtype.Timestamptz{Time: publishedAt, Valid: true}
	round, err := w.q.CreateGeneratedRound(ctx, gen.CreateGeneratedRoundParams{
		Number:       number,
		State:        gen.RoundStatePublished,
		PublishAt:    at,
		PublishedAt:  at,
		PairAt:       at,
		GeneratedBy:  gen.RoundSourceSchedule,
		SettingsUsed: used,
	})
	require.NoError(t, err)

	var pairings []gen.Pairing
	for i, g := range games {
		position := int32(i + 1)
		p, err := w.q.InsertGeneratedPairing(ctx, gen.InsertGeneratedPairingParams{
			RoundID:     round.ID,
			WhiteUserID: g.white.ID,
			BlackUserID: g.black.ID,
			Position:    &position,
		})
		require.NoError(t, err)
		if g.challenger != nil {
			id := "chal" + p.ID.String()[:8]
			n, err := w.q.RecordChallenge(ctx, gen.RecordChallengeParams{
				ID:               p.ID,
				ChallengerUserID: g.challenger.ID,
				LichessGameID:    &id,
			})
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
		}
		pairings = append(pairings, w.pairing(t, p))
	}
	return round, pairings
}

func (w world) pairing(t *testing.T, p gen.Pairing) gen.Pairing {
	t.Helper()
	got, err := w.q.GetPairingByID(context.Background(), gen.GetPairingByIDParams{ID: p.ID, RoundID: p.RoundID})
	require.NoError(t, err)
	return got
}

func (w world) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	_, err := w.tx.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

// startedGame stores a game for a pairing, started at startedAt: the
// way sync-games records a game once it begins.
func (w world) startedGame(t *testing.T, p gen.Pairing, startedAt time.Time) {
	t.Helper()
	round, err := w.q.GetRoundByID(context.Background(), p.RoundID)
	require.NoError(t, err)
	w.exec(t, `
		INSERT INTO games (lichess_game_id, pairing_id, round_number, white_user_id, black_user_id,
		                   status, lichess_status, started_at, last_move_at, raw_payload)
		VALUES ($1, $2, $3, $4, $5, 'in_progress', 'started', $6, $6, '{}')`,
		"game"+p.ID.String()[:8], p.ID, round.Number, p.WhiteUserID, p.BlackUserID, startedAt)
	w.exec(t, `UPDATE pairings SET status = 'in_progress' WHERE id = $1`, p.ID)
}

func (w world) evaluator() activity.Evaluator {
	return activity.Evaluator{Lichess: w.fake, Scheduler: w.sched}
}

func (w world) evaluate(t *testing.T, at time.Time) activity.Report {
	t.Helper()
	report, err := w.evaluator().Evaluate(context.Background(), w.tx, w.cfg, at)
	require.NoError(t, err)
	return report
}

func (w world) missedStarts(t *testing.T, user gen.User) []gen.ListMissedStartsForUserRow {
	t.Helper()
	rows, err := w.q.ListMissedStartsForUser(context.Background(), user.ID)
	require.NoError(t, err)
	return rows
}

func (w world) profile(t *testing.T, user gen.User) gen.PlayerProfile {
	t.Helper()
	profile, err := w.q.GetPlayerProfile(context.Background(), user.ID)
	require.NoError(t, err)
	return profile
}

func (w world) notices(t *testing.T, user gen.User) []gen.Notification {
	t.Helper()
	notices, err := w.q.ListNotificationsForUser(context.Background(), gen.ListNotificationsForUserParams{
		UserID: user.ID,
		Limit:  100,
	})
	require.NoError(t, err)
	return notices
}

func keysOf(notices []gen.Notification) []string {
	var keys []string
	for _, n := range notices {
		if n.DedupeKey != nil {
			keys = append(keys, *n.DedupeKey)
		}
	}
	return keys
}

func fmtInt(n int32) string { return strconv.Itoa(int(n)) }

const week = 7 * 24 * time.Hour

// --- evaluation ----------------------------------------------------------

func TestEvaluate_RetiresEveryUnstartedPairing(t *testing.T) {
	// Spec §6.4: at the next generation every still-pending pairing of a
	// published round is marked failed, whatever its kind, so it stops
	// counting towards caps. Only a challenge is a missed start, and
	// only for the player who had to accept it.
	w := newWorld(t)
	alice, bob := w.player(t, "alice"), w.player(t, "bob")
	carol, dave := w.player(t, "carol"), w.player(t, "dave")
	erin, frank := w.player(t, "erin"), w.player(t, "frank")
	round, pairings := w.round(t, w.now.Add(-week), byHand(alice, bob), challenge(carol, dave), byHand(erin, frank))
	handStarted, challenged, playing := pairings[0], pairings[1], pairings[2]
	w.startedGame(t, playing, w.now.Add(-week+time.Hour))

	report := w.evaluate(t, w.now)

	assert.Equal(t, gen.PairingStatusFailed, w.pairing(t, handStarted).Status)
	assert.Equal(t, gen.PairingStatusFailed, w.pairing(t, challenged).Status)
	assert.Equal(t, gen.PairingStatusInProgress, w.pairing(t, playing).Status, "a game in progress is no business of this")
	assert.Equal(t, 2, report.Retired)

	missed := w.missedStarts(t, dave)
	require.Len(t, missed, 1, "dave had to accept carol's challenge")
	assert.Equal(t, round.Number, missed[0].RoundNumber)
	for _, nobody := range []gen.User{alice, bob, carol, erin, frank} {
		assert.Empty(t, w.missedStarts(t, nobody), "%s missed nothing anyone can tell", nobody.LichessUsername)
	}
	assert.Equal(t, []pgtype.UUID{challenged.ID}, w.sched.pairings, "carol's challenge is withdrawn")

	// The default threshold is 2: dave is warned, not paused.
	assert.Equal(t, []string{"dave"}, report.Warned)
	assert.Empty(t, report.Paused)
	assert.False(t, w.profile(t, dave).AutoPausedAt.Valid)
	notices := w.notices(t, dave)
	require.Len(t, notices, 1)
	assert.Equal(t, string(notify.MissedStart), notices[0].Category)
	assert.Equal(t, "missed_start:"+fmtInt(round.Number), *notices[0].DedupeKey)
	assert.Contains(t, notices[0].Body, "carol's challenge in round "+fmtInt(round.Number)+" was never accepted")
	assert.Empty(t, w.notices(t, carol), "the challenger is told nothing")
	assert.Empty(t, w.notices(t, alice), "a pairing retired by hand is no one's fault")
}

func TestEvaluate_LeavesAloneWhatIsNotUnstarted(t *testing.T) {
	// A pairing gets 48 hours from its round's publication; a round whose
	// games are still being created belongs to its create-games job; an
	// ambiguous pairing has two games, for an admin to choose from; and a
	// challenge accepted since sync-games last looked is a game.
	w := newWorld(t)
	a, b, c, d := w.player(t, "a1"), w.player(t, "b1"), w.player(t, "c1"), w.player(t, "d1")
	e, f, g, h := w.player(t, "e1"), w.player(t, "f1"), w.player(t, "g1"), w.player(t, "h1")

	_, recent := w.round(t, w.now.Add(-activity.StartGrace+time.Minute), byHand(a, b))
	creating, beingCreated := w.round(t, w.now.Add(-week), byHand(c, d))
	w.exec(t, `UPDATE rounds SET game_creation = 'creating' WHERE id = $1`, creating.ID)
	_, older := w.round(t, w.now.Add(-week), byHand(e, f), challenge(g, h))
	ambiguous, accepted := older[0], older[1]
	w.exec(t, `UPDATE pairings SET match_ambiguous = true WHERE id = $1`, ambiguous.ID)
	w.fake.Games[*accepted.LichessGameID] = lichess.Game{ID: *accepted.LichessGameID, Status: "started"}

	report := w.evaluate(t, w.now)

	for _, p := range []gen.Pairing{recent[0], beingCreated[0], ambiguous, accepted} {
		assert.Equal(t, gen.PairingStatusPending, w.pairing(t, p).Status)
	}
	assert.Zero(t, report.Retired)
	assert.Equal(t, 1, report.Accepted)
	assert.Empty(t, w.missedStarts(t, h), "h accepted")
	assert.Empty(t, w.sched.pairings)
}

func TestEvaluate_ExactlyAtTheGraceIsUnstarted(t *testing.T) {
	w := newWorld(t)
	a, b := w.player(t, "a2"), w.player(t, "b2")
	_, pairings := w.round(t, w.now.Add(-activity.StartGrace), byHand(a, b))

	w.evaluate(t, w.now)

	assert.Equal(t, gen.PairingStatusFailed, w.pairing(t, pairings[0]).Status)
}

func TestEvaluate_LichessDownChangesNothing(t *testing.T) {
	// A challenge Lichess could not be asked about may be a game in
	// progress: the evaluation fails, and retires nothing else either.
	w := newWorld(t)
	a, b, c, d := w.player(t, "a3"), w.player(t, "b3"), w.player(t, "c3"), w.player(t, "d3")
	_, pairings := w.round(t, w.now.Add(-week), byHand(a, b), challenge(c, d))
	w.fake.Err = errors.New("lichess: 503")

	_, err := w.evaluator().Evaluate(context.Background(), w.tx, w.cfg, w.now)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")

	for _, p := range pairings {
		assert.Equal(t, gen.PairingStatusPending, w.pairing(t, p).Status)
	}
	assert.Empty(t, w.missedStarts(t, d))
	assert.Empty(t, w.sched.pairings)
}

func TestEvaluate_WithoutChallengesLichessIsNotAsked(t *testing.T) {
	// Under pairing.game_creation = manual there is no challenge: the
	// evaluation needs neither Lichess nor a job queue.
	w := newWorld(t)
	a, b := w.player(t, "a4"), w.player(t, "b4")
	_, pairings := w.round(t, w.now.Add(-week), byHand(a, b))
	w.fake.Err = errors.New("lichess: 503")

	report, err := activity.Evaluator{Lichess: w.fake}.Evaluate(context.Background(), w.tx, w.cfg, w.now)
	require.NoError(t, err)

	assert.Equal(t, 1, report.Retired)
	assert.Equal(t, gen.PairingStatusFailed, w.pairing(t, pairings[0]).Status)
}

func TestEvaluate_AChallengeNeedsAJobQueueToBeWithdrawn(t *testing.T) {
	w := newWorld(t)
	c, d := w.player(t, "c5"), w.player(t, "d5")
	_, pairings := w.round(t, w.now.Add(-week), challenge(c, d))

	_, err := activity.Evaluator{Lichess: w.fake}.Evaluate(context.Background(), w.tx, w.cfg, w.now)
	require.Error(t, err)

	assert.Equal(t, gen.PairingStatusPending, w.pairing(t, pairings[0]).Status)
	assert.Empty(t, w.fake.Calls)
}

func TestEvaluate_TheSecondMissInARowPauses(t *testing.T) {
	// Spec §6.4 with the default threshold: warned at the first missed
	// start, paused at the second, and out of the round generated next.
	w := newWorld(t)
	carol, dave := w.player(t, "carol6"), w.player(t, "dave6")
	first, _ := w.round(t, w.now.Add(-2*week), challenge(carol, dave))
	w.evaluate(t, w.now.Add(-week))
	second, _ := w.round(t, w.now.Add(-week), challenge(carol, dave))

	report := w.evaluate(t, w.now)

	assert.Equal(t, []string{"dave6"}, report.Paused)
	assert.Empty(t, report.Warned)
	profile := w.profile(t, dave)
	require.True(t, profile.AutoPausedAt.Valid)
	assert.WithinDuration(t, w.now, profile.AutoPausedAt.Time, time.Millisecond)
	standing, err := w.q.GetPlayerStanding(context.Background(), dave.ID)
	require.NoError(t, err)
	assert.False(t, standing.IsEligible, "the standing follows the pause")

	assert.Equal(t,
		[]string{"auto_pause:" + fmtInt(second.Number), "missed_start:" + fmtInt(first.Number)},
		keysOf(w.notices(t, dave)), "warned once, then paused once, newest first")
	paused := w.notices(t, dave)[0]
	assert.Equal(t, string(notify.AutoPause), paused.Category)
	assert.Contains(t, paused.Body, "2 challenges in a row went unanswered, the last from carol6 in round "+fmtInt(second.Number))
	assert.Equal(t, "/account", *paused.LinkUrl)

	outcome, err := rounds.Generate(context.Background(), w.tx, w.cfg, w.now, gen.RoundSourceSchedule, pgtype.UUID{}, nil)
	require.NoError(t, err)
	exclusion, err := w.q.GetExclusionForUserInRound(context.Background(), gen.GetExclusionForUserInRoundParams{
		RoundID: outcome.Round.ID,
		UserID:  dave.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, gen.ExclusionReasonAutoPaused, exclusion.Reason)
}

func TestEvaluate_AStartedGameEndsTheRun(t *testing.T) {
	w := newWorld(t)
	carol, dave, erin := w.player(t, "carol7"), w.player(t, "dave7"), w.player(t, "erin7")
	w.round(t, w.now.Add(-2*week), challenge(carol, dave))
	w.evaluate(t, w.now.Add(-week))
	// Dave plays a game after that first miss: the double game's other
	// half, say, or a game the league created once he re-authorised.
	_, played := w.round(t, w.now.Add(-week), byHand(dave, erin), challenge(carol, dave))
	w.startedGame(t, played[0], w.now.Add(-week+time.Hour))

	report := w.evaluate(t, w.now)

	assert.Equal(t, []string{"dave7"}, report.Warned, "one miss since his game: a warning again, no pause")
	assert.Empty(t, report.Paused)
	assert.False(t, w.profile(t, dave).AutoPausedAt.Valid)
}

func TestEvaluate_ResumingEndsTheRun(t *testing.T) {
	w := newWorld(t)
	carol, dave := w.player(t, "carol8"), w.player(t, "dave8")
	w.round(t, w.now.Add(-2*week), challenge(carol, dave))
	w.evaluate(t, w.now.Add(-week))
	// Paused by an admin's hand-rolled rule, say: what counts is that he
	// resumed after the first miss.
	w.exec(t, `UPDATE player_profiles SET resumed_at = $2 WHERE user_id = $1`, dave.ID, w.now.Add(-week+time.Hour))
	w.round(t, w.now.Add(-week), challenge(carol, dave))

	report := w.evaluate(t, w.now)

	assert.Equal(t, []string{"dave8"}, report.Warned)
	assert.Empty(t, report.Paused)
}

func TestEvaluate_TheVolunteerMissesARoundNotTwo(t *testing.T) {
	// The double-game volunteer who leaves both of a round's challenges
	// unanswered has one missed start: the rule counts rounds.
	w := newWorld(t)
	carol, dave, erin := w.player(t, "carol9"), w.player(t, "dave9"), w.player(t, "erin9")
	_, pairings := w.round(t, w.now.Add(-week), challenge(carol, dave), challenge(erin, dave))

	report := w.evaluate(t, w.now)

	assert.Equal(t, 2, report.Retired)
	assert.Len(t, w.missedStarts(t, dave), 1)
	assert.Equal(t, []string{"dave9"}, report.Warned, "warned, not paused")
	assert.ElementsMatch(t, []pgtype.UUID{pairings[0].ID, pairings[1].ID}, w.sched.pairings, "both challenges withdrawn")
}

func TestEvaluate_RunningAgainChangesNothing(t *testing.T) {
	w := newWorld(t)
	carol, dave := w.player(t, "carol10"), w.player(t, "dave10")
	w.round(t, w.now.Add(-week), challenge(carol, dave))
	w.evaluate(t, w.now)

	report := w.evaluate(t, w.now.Add(time.Minute))

	assert.Zero(t, report.Retired)
	assert.Empty(t, report.MissedStarts)
	assert.Len(t, w.missedStarts(t, dave), 1)
	assert.Len(t, w.notices(t, dave), 1)
	assert.Len(t, w.sched.pairings, 1)
}

func TestEvaluate_SkippedWhileADraftWaits(t *testing.T) {
	// A draft was paired after the last evaluation: a player paused now
	// would still be in it.
	w := newWorld(t)
	carol, dave := w.player(t, "carol11"), w.player(t, "dave11")
	_, pairings := w.round(t, w.now.Add(-week), challenge(carol, dave))
	_, err := rounds.Generate(context.Background(), w.tx, w.cfg, w.now, gen.RoundSourceManual, pgtype.UUID{}, nil)
	require.NoError(t, err)

	report := w.evaluate(t, w.now)

	assert.NotEmpty(t, report.Skipped)
	assert.Equal(t, gen.PairingStatusPending, w.pairing(t, pairings[0]).Status)
}

func TestEvaluate_AnAdminsMarkFailedMeanwhileWins(t *testing.T) {
	// The retirement is guarded like every pairing status change: a
	// pairing that is no longer pending is left as it is, and nobody
	// misses a start for it.
	w := newWorld(t)
	carol, dave := w.player(t, "carol12"), w.player(t, "dave12")
	_, pairings := w.round(t, w.now.Add(-week), challenge(carol, dave))
	api := markingFailedMidCall{Fake: w.fake, t: t, w: w, pairing: pairings[0]}

	report, err := activity.Evaluator{Lichess: api, Scheduler: w.sched}.Evaluate(context.Background(), w.tx, w.cfg, w.now)
	require.NoError(t, err)

	assert.Zero(t, report.Retired)
	assert.Empty(t, w.missedStarts(t, dave))
	assert.Empty(t, w.sched.pairings, "Mark failed withdraws its own challenge")
}

// markingFailedMidCall stands for an admin marking a pairing failed
// while the evaluation asks Lichess about the challenges.
type markingFailedMidCall struct {
	*lichess.Fake
	t       *testing.T
	w       world
	pairing gen.Pairing
}

func (m markingFailedMidCall) GamesByID(ctx context.Context, ids []string) (*lichess.GameStream, error) {
	m.w.exec(m.t, `UPDATE pairings SET status = 'failed' WHERE id = $1`, m.pairing.ID)
	return m.Fake.GamesByID(ctx, ids)
}

func TestRun_RecordsItsOutcome(t *testing.T) {
	w := newWorld(t)
	a, b := w.player(t, "a13"), w.player(t, "b13")
	w.round(t, time.Now().Add(-week), byHand(a, b))
	riverJobID := time.Now().UnixNano()

	require.NoError(t, w.evaluator().Run(context.Background(), w.tx, &riverJobID))

	run := runByRiverID(t, w.q, riverJobID)
	assert.Equal(t, "evaluate-activity", run.JobName)
	assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
	assert.EqualValues(t, 1, run.ItemsProcessed)
	var report activity.Report
	require.NoError(t, json.Unmarshal(run.Detail, &report))
	assert.Equal(t, 1, report.Retired)
}

func TestRun_AFailureIsRecorded(t *testing.T) {
	w := newWorld(t)
	w.exec(t, `INSERT INTO settings (key, value) VALUES ('activity.missed_starts_to_pause', '0')
	           ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	riverJobID := time.Now().UnixNano()

	require.Error(t, w.evaluator().Run(context.Background(), w.tx, &riverJobID))

	run := runByRiverID(t, w.q, riverJobID)
	assert.Equal(t, gen.JobRunStatusFailed, run.Status)
	require.NotNil(t, run.Error)
	assert.Contains(t, *run.Error, "activity.missed_starts_to_pause")
}

func runByRiverID(t *testing.T, q *gen.Queries, riverJobID int64) gen.JobRun {
	t.Helper()
	runs, err := q.ListRecentJobRuns(context.Background(), 200)
	require.NoError(t, err)
	for _, run := range runs {
		if run.RiverJobID != nil && *run.RiverJobID == riverJobID {
			return run
		}
	}
	t.Fatalf("no job run for river job %d", riverJobID)
	return gen.JobRun{}
}

// --- the 48-hour reminder ------------------------------------------------

func TestRemind_TellsWhoCanStartTheGameOnce(t *testing.T) {
	w := newWorld(t)
	alice, bob := w.player(t, "alice14"), w.player(t, "bob14")
	carol, dave := w.player(t, "carol14"), w.player(t, "dave14")
	round, pairings := w.round(t, w.now.Add(-activity.StartGrace-time.Minute), byHand(alice, bob), challenge(carol, dave))

	n, err := activity.Remind(context.Background(), w.q, w.now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	_, err = activity.Remind(context.Background(), w.q, w.now.Add(time.Hour))
	require.NoError(t, err)

	key := func(p gen.Pairing) string { return "round:" + fmtInt(round.Number) + ":unstarted:" + p.ID.String() }

	// By hand: both players, each told the game to start.
	for _, player := range []struct {
		user   gen.User
		colour string
		vs     string
	}{{alice, "white", "bob14"}, {bob, "black", "alice14"}} {
		notices := w.notices(t, player.user)
		require.Len(t, notices, 1, player.user.LichessUsername)
		assert.Equal(t, string(notify.Unstarted), notices[0].Category)
		assert.Equal(t, key(pairings[0]), *notices[0].DedupeKey)
		assert.Contains(t, notices[0].Title, "your game against "+player.vs+" hasn't started")
		assert.Contains(t, notices[0].Body, "a rated correspondence game at 2 days per move, you with "+player.colour)
	}

	// A challenge: only the player who must accept it, with its link.
	assert.Empty(t, w.notices(t, carol))
	notices := w.notices(t, dave)
	require.Len(t, notices, 1)
	assert.Equal(t, key(pairings[1]), *notices[0].DedupeKey)
	assert.Contains(t, notices[0].Title, "carol14's challenge is waiting")
	assert.Equal(t, rounds.GameURL(*pairings[1].LichessGameID), *notices[0].LinkUrl)
}

func TestRemind_NotBeforeTheGrace(t *testing.T) {
	w := newWorld(t)
	alice, bob := w.player(t, "alice15"), w.player(t, "bob15")
	w.round(t, w.now.Add(-activity.StartGrace+time.Minute), byHand(alice, bob))

	n, err := activity.Remind(context.Background(), w.q, w.now)
	require.NoError(t, err)

	assert.Zero(t, n)
	assert.Empty(t, w.notices(t, alice))
}
