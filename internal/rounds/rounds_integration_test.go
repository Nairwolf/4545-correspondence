//go:build integration

package rounds_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// testTx opens a transaction against TEST_DATABASE_URL and rolls it
// back when the test ends, so nothing needs cleaning up and the dev
// database is never changed.
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

	// The pairing pool is "every approved user" and only one draft may
	// exist at a time, so whatever the database already holds would
	// otherwise take part in these tests. Both statements roll back
	// with the transaction.
	_, err = tx.Exec(ctx, `UPDATE users SET status = 'pending' WHERE status = 'approved'`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE rounds SET state = 'cancelled' WHERE state = 'draft'`)
	require.NoError(t, err)

	return tx
}

// addPlayer creates an approved player with a standing, which is what
// every test here needs before it can be paired.
func addPlayer(t *testing.T, tx pgx.Tx, name string, powerRating int32) gen.User {
	t.Helper()
	ctx := context.Background()
	q := gen.New(tx)

	user, err := q.CreateApprovedUser(ctx, gen.CreateApprovedUserParams{
		LichessUsername: name,
		LichessUserID:   name,
	})
	require.NoError(t, err)
	_, err = q.CreatePlayerProfile(ctx, user.ID)
	require.NoError(t, err)
	require.NoError(t, q.UpsertPlayerStanding(ctx, gen.UpsertPlayerStandingParams{
		UserID:      user.ID,
		PowerRating: powerRating,
		IsEligible:  true,
	}))
	return user
}

func setActive(t *testing.T, tx pgx.Tx, user gen.User, active bool) {
	t.Helper()
	_, err := gen.New(tx).SetPlayerActive(context.Background(), gen.SetPlayerActiveParams{
		UserID:   user.ID,
		IsActive: active,
	})
	require.NoError(t, err)
}

func setCapacity(t *testing.T, tx pgx.Tx, user gen.User, cap int32) {
	t.Helper()
	_, err := gen.New(tx).SetPlayerCapacity(context.Background(), gen.SetPlayerCapacityParams{
		UserID:             user.ID,
		MaxConcurrentGames: &cap,
	})
	require.NoError(t, err)
}

func exec(t *testing.T, tx pgx.Tx, sql string, args ...any) {
	t.Helper()
	_, err := tx.Exec(context.Background(), sql, args...)
	require.NoError(t, err)
}

// addPublishedRound writes a finished-with round in the state the
// history queries read: published, with one pending pairing per pair.
func addPublishedRound(t *testing.T, tx pgx.Tx, pairs [][2]gen.User) gen.Round {
	t.Helper()
	return addRound(t, tx, gen.RoundStatePublished, pairs)
}

func addRound(t *testing.T, tx pgx.Tx, state gen.RoundState, pairs [][2]gen.User) gen.Round {
	t.Helper()
	ctx := context.Background()
	q := gen.New(tx)

	number, err := q.NextRoundNumber(ctx)
	require.NoError(t, err)
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}

	round, err := q.CreateRound(ctx, gen.CreateRoundParams{
		Number:      number,
		State:       state,
		PublishAt:   now,
		PublishedAt: now,
		PairAt:      now,
		GeneratedBy: gen.RoundSourceImported,
	})
	require.NoError(t, err)

	for _, pair := range pairs {
		_, err := q.UpsertManualPairing(ctx, gen.UpsertManualPairingParams{
			RoundID:        round.ID,
			WhiteUserID:    pair[0].ID,
			BlackUserID:    pair[1].ID,
			CreationMethod: gen.PairingMethodManualExternal,
		})
		require.NoError(t, err)
	}
	return round
}

// fakeScheduler records what the service asked river to enqueue.
// Testing the enqueue itself would be testing river; what matters here
// is that a draft schedules its own publication exactly once and that
// regeneration does not lose it.
type fakeScheduler struct {
	calls []scheduled
}

type scheduled struct {
	roundID   int32
	publishAt time.Time
}

func (f *fakeScheduler) SchedulePublish(_ context.Context, _ pgx.Tx, roundID int32, publishAt time.Time) error {
	f.calls = append(f.calls, scheduled{roundID: roundID, publishAt: publishAt})
	return nil
}

func generate(t *testing.T, tx pgx.Tx, cfg settings.Settings, sched rounds.Scheduler) rounds.Outcome {
	t.Helper()
	outcome, err := rounds.Generate(
		context.Background(),
		tx,
		cfg,
		time.Now(),
		gen.RoundSourceSchedule,
		pgtype.UUID{},
		sched,
	)
	require.NoError(t, err)
	return outcome
}

func TestGenerate_StoresWhatTheEngineDecided(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	players := []gen.User{
		addPlayer(t, tx, "alpha", 2000),
		addPlayer(t, tx, "bravo", 1900),
		addPlayer(t, tx, "charlie", 1800),
		addPlayer(t, tx, "delta", 1700),
		addPlayer(t, tx, "echo", 1600),
	}

	before := time.Now()
	outcome := generate(t, tx, settings.Defaults(), nil)
	round := outcome.Round

	assert.Equal(t, gen.RoundStateDraft, round.State)
	assert.Equal(t, gen.RoundSourceSchedule, round.GeneratedBy)
	require.NotNil(t, round.PoolSize)
	assert.EqualValues(t, len(players), *round.PoolSize)
	require.NotNil(t, round.RepeatPairings)
	assert.EqualValues(t, 0, *round.RepeatPairings)
	assert.False(t, round.PublishedAt.Valid, "a draft has not been published")

	// The review window is six hours by default, and pair_at holds the
	// planned publication moment until the round actually publishes.
	assert.WithinDuration(t, before.Add(6*time.Hour), round.PublishAt.Time, time.Minute)
	assert.Equal(t, round.PublishAt.Time, round.PairAt.Time)

	var snapshot rounds.Snapshot
	require.NoError(t, json.Unmarshal(round.SettingsUsed, &snapshot))
	assert.Equal(t, settings.PairingModeReviewWindow, snapshot.Mode)
	assert.Equal(t, 100, snapshot.ColorWeight)
	assert.Equal(t, 1_000_000, snapshot.RepeatPenalty)
	assert.Equal(t, 2, snapshot.DaysPerMove, "Phase 5 pairs the games with the settings the round was generated under")

	stored, err := q.ListPairingsForRound(ctx, round.ID)
	require.NoError(t, err)
	require.Len(t, stored, len(outcome.Result.Pairings))
	for i, p := range stored {
		want := outcome.Result.Pairings[i]
		assert.Equal(t, want.White, p.WhiteUserID.String())
		assert.Equal(t, want.Black, p.BlackUserID.String())
		require.NotNil(t, p.Position)
		assert.EqualValues(t, i+1, *p.Position)
		require.NotNil(t, p.RatingGap)
		assert.EqualValues(t, want.RatingGap, *p.RatingGap)
		require.NotNil(t, p.ColorPenalty)
		assert.EqualValues(t, want.ColorPenalty, *p.ColorPenalty)
		assert.Nil(t, p.RepeatOfRound)
		// Phase 4 makes no Lichess call: the pairings are exactly what
		// import-pairings produces, and players start the games by hand.
		assert.Equal(t, gen.PairingMethodManualExternal, p.CreationMethod)
		assert.Equal(t, gen.PairingStatusPending, p.Status)
	}

	// An odd pool: five players means someone takes two games or sits
	// out, and either way it is recorded for the rotation.
	require.NotNil(t, round.OddPool)
	switch *round.OddPool {
	case gen.OddPoolOutcomeDoubleGame:
		doubles, err := q.ListDoubleGamesForRound(ctx, round.ID)
		require.NoError(t, err)
		require.Len(t, doubles, 1)
		require.NotNil(t, outcome.Result.DoubleGame)
		assert.Equal(t, *outcome.Result.DoubleGame, doubles[0].UserID.String())
	case gen.OddPoolOutcomeBye:
		byes, err := q.ListByesForRound(ctx, round.ID)
		require.NoError(t, err)
		require.Len(t, byes, 1)
	default:
		t.Fatalf("a pool of five is odd, got %q", *round.OddPool)
	}

	assert.True(t, audited(t, tx, "round.generate", round.ID), "the generation is in the audit log")
}

func TestGenerate_RunsAndRecordsTheConfiguredSolver(t *testing.T) {
	// Greedy pairs alpha with bravo, the closest rating, and leaves
	// charlie and delta, who met in the last round, to meet again; the
	// blossom solver pairs them apart (PLAN.md, step 6a).
	tx := testTx(t)
	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1990)
	charlie := addPlayer(t, tx, "charlie", 1600)
	delta := addPlayer(t, tx, "delta", 1590)
	addPublishedRound(t, tx, [][2]gen.User{{charlie, delta}})

	solverUsed := func(round gen.Round) settings.Solver {
		t.Helper()
		var snapshot rounds.Snapshot
		require.NoError(t, json.Unmarshal(round.SettingsUsed, &snapshot))
		return snapshot.Solver
	}

	greedy := generate(t, tx, settings.Defaults(), nil)
	assert.Equal(t, settings.SolverGreedy, solverUsed(greedy.Round))
	require.NotNil(t, greedy.Round.RepeatPairings)
	assert.EqualValues(t, 1, *greedy.Round.RepeatPairings)

	cfg := settings.Defaults()
	cfg.Solver = settings.SolverBlossom
	blossom, err := rounds.Regenerate(context.Background(), tx, greedy.Round.ID, cfg, pgtype.UUID{}, nil)
	require.NoError(t, err)
	assert.Equal(t, settings.SolverBlossom, solverUsed(blossom.Round))
	require.NotNil(t, blossom.Round.RepeatPairings)
	assert.EqualValues(t, 0, *blossom.Round.RepeatPairings)
}

func TestGenerate_PairsTheUnlimitedPlayerWithFortyGamesInFlight(t *testing.T) {
	// The NULL-cap rule end to end (spec §5.8): the default player has
	// no cap, and no number of games in flight may keep them out of a
	// round. A NULL-to-zero coercion anywhere between the pool query
	// and the engine would show up here as an at_capacity exclusion.
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	unlimited := addPlayer(t, tx, "unlimited", 1800)
	var pairs [][2]gen.User
	for i := range 40 {
		opponent := addPlayer(t, tx, fmt.Sprintf("opponent-%02d", i), 1500+int32(i))
		pairs = append(pairs, [2]gen.User{unlimited, opponent})
	}
	addPublishedRound(t, tx, pairs)

	inFlight, err := q.CountInFlightGamesForUser(ctx, unlimited.ID)
	require.NoError(t, err)
	require.EqualValues(t, 40, inFlight, "pending pairings of a published round are games in flight")

	outcome := generate(t, tx, settings.Defaults(), nil)

	var paired bool
	for _, p := range outcome.Result.Pairings {
		if p.White == unlimited.ID.String() || p.Black == unlimited.ID.String() {
			paired = true
		}
	}
	assert.True(t, paired, "a player with no cap is paired every round, whatever their backlog")

	exclusions, err := q.ListExclusionsForRound(ctx, outcome.Round.ID)
	require.NoError(t, err)
	for _, e := range exclusions {
		assert.NotEqual(t, unlimited.ID, e.UserID, "the unlimited player was excluded as %q", e.Reason)
	}
}

func TestGenerate_WritesOneExclusionPerAbsentPlayer(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	playing := addPlayer(t, tx, "playing", 2000)
	inactive := addPlayer(t, tx, "inactive", 1900)
	paused := addPlayer(t, tx, "paused", 1850)
	autoPaused := addPlayer(t, tx, "auto-paused", 1800)
	capped := addPlayer(t, tx, "capped", 1750)
	other := addPlayer(t, tx, "other", 1700)

	setActive(t, tx, inactive, false)
	exec(t, tx, `UPDATE player_profiles SET paused_by_admin = true WHERE user_id = $1`, paused.ID)
	exec(t, tx, `UPDATE player_profiles SET auto_paused_at = now() WHERE user_id = $1`, autoPaused.ID)
	setCapacity(t, tx, capped, 1)
	addPublishedRound(t, tx, [][2]gen.User{{capped, other}})

	outcome := generate(t, tx, settings.Defaults(), nil)

	stored, err := q.ListExclusionsForRound(ctx, outcome.Round.ID)
	require.NoError(t, err)

	byUser := map[string]gen.ListExclusionsForRoundRow{}
	for _, e := range stored {
		_, seen := byUser[e.UserID.String()]
		assert.False(t, seen, "%s was excluded twice", e.LichessUsername)
		byUser[e.UserID.String()] = e
	}

	assert.Equal(t, gen.ExclusionReasonInactive, byUser[inactive.ID.String()].Reason)
	assert.Equal(t, gen.ExclusionReasonPaused, byUser[paused.ID.String()].Reason)
	assert.Equal(t, gen.ExclusionReasonAutoPaused, byUser[autoPaused.ID.String()].Reason)
	assert.NotContains(t, byUser, playing.ID.String())

	atCapacity := byUser[capped.ID.String()]
	assert.Equal(t, gen.ExclusionReasonAtCapacity, atCapacity.Reason)
	require.NotNil(t, atCapacity.OngoingGames)
	assert.EqualValues(t, 1, *atCapacity.OngoingGames, "the dashboard quotes these numbers back")
	require.NotNil(t, atCapacity.MaxConcurrentGames)
	assert.EqualValues(t, 1, *atCapacity.MaxConcurrentGames)

	// Only at_capacity carries numbers; the others would be lying.
	assert.Nil(t, byUser[inactive.ID.String()].OngoingGames)
}

func TestGenerate_SchedulesItsOwnPublication(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)

	sched := &fakeScheduler{}
	outcome := generate(t, tx, settings.Defaults(), sched)

	require.Len(t, sched.calls, 1, "a draft schedules exactly one publish-round job")
	assert.Equal(t, outcome.Round.ID, sched.calls[0].roundID)
	assert.Equal(t, outcome.Round.PublishAt.Time, sched.calls[0].publishAt)

	// Regenerating must not lose the publication: the window did not
	// move, so the same job is enqueued again — a no-op for river when
	// it is already queued, and the safety net when it never was.
	again, err := rounds.Regenerate(ctx, tx, outcome.Round.ID, settings.Defaults(), pgtype.UUID{}, sched)
	require.NoError(t, err)
	require.Len(t, sched.calls, 2)
	assert.Equal(t, outcome.Round.PublishAt.Time, sched.calls[1].publishAt)
	assert.Equal(t, outcome.Round.PublishAt.Time, again.Round.PublishAt.Time, "the review window does not restart")
}

func TestGenerate_AutoPublishNeedsNoSecondStep(t *testing.T) {
	tx := testTx(t)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)

	cfg := settings.Defaults()
	cfg.PairingMode = settings.PairingModeAutoPublish
	sched := &fakeScheduler{}

	round := generate(t, tx, cfg, sched).Round

	assert.Equal(t, gen.RoundStatePublished, round.State)
	require.True(t, round.PublishedAt.Valid)
	assert.Equal(t, round.PublishedAt.Time, round.PairAt.Time, "pair_at is the moment the round went live (§6.3)")
	assert.Equal(t, round.PublishedAt.Time, round.PublishAt.Time)
	assert.Empty(t, sched.calls, "there is no draft to publish later")
}

func TestGenerate_RefusesASecondDraft(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)

	generate(t, tx, settings.Defaults(), nil)

	_, err := rounds.Generate(
		ctx,
		tx,
		settings.Defaults(),
		time.Now(),
		gen.RoundSourceManual,
		pgtype.UUID{},
		nil,
	)
	assert.ErrorIs(t, err, rounds.ErrDraftExists)
}

func TestPublish_IsIdempotentAcrossItsThreeCallers(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)
	draft := generate(t, tx, settings.Defaults(), nil).Round

	// The scheduled job, or an admin's "publish now", first.
	now := time.Now()
	round, published, err := rounds.Publish(ctx, tx, draft.ID, now, pgtype.UUID{})
	require.NoError(t, err)
	assert.True(t, published)
	assert.Equal(t, gen.RoundStatePublished, round.State)
	assert.Equal(t, round.PublishedAt.Time, round.PairAt.Time)
	assert.WithinDuration(t, now, round.PublishedAt.Time, time.Second)

	// Whoever gets there second changes nothing.
	_, published, err = rounds.Publish(ctx, tx, draft.ID, time.Now(), pgtype.UUID{})
	require.NoError(t, err)
	assert.False(t, published)

	// And the hourly sweep no longer sees it.
	swept, err := rounds.PublishDue(ctx, tx, time.Now())
	require.NoError(t, err)
	assert.Empty(t, swept)
}

func TestPublishDue_PublishesAnOverdueDraft(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)

	cfg := settings.Defaults()
	cfg.ReviewWindowHours = 0 // the window has already passed
	draft := generate(t, tx, cfg, nil).Round
	require.Equal(t, gen.RoundStateDraft, draft.State)

	published, err := rounds.PublishDue(ctx, tx, time.Now())
	require.NoError(t, err)
	require.Len(t, published, 1)
	assert.Equal(t, draft.Number, published[0].Number)
	assert.Equal(t, gen.RoundStatePublished, published[0].State)
}

func TestRegenerate_ReplacesTheRoundInPlace(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)
	dropped := addPlayer(t, tx, "charlie", 1800)
	addPlayer(t, tx, "delta", 1700)
	addPlayer(t, tx, "echo", 1600)

	// Five is an odd pool, so someone plays twice and there are three
	// pairings; dropping a player makes it four and two, which is what
	// makes "replaced, not added to" visible below.
	first := generate(t, tx, settings.Defaults(), nil)
	require.Len(t, first.Result.Pairings, 3)

	// An admin looks at the draft, then takes a player out of the pool
	// and asks for a new pairing.
	setActive(t, tx, dropped, false)
	second, err := rounds.Regenerate(ctx, tx, first.Round.ID, settings.Defaults(), pgtype.UUID{}, nil)
	require.NoError(t, err)

	assert.Equal(t, first.Round.ID, second.Round.ID)
	assert.Equal(t, first.Round.Number, second.Round.Number, "a regenerated round is the same round")
	assert.Equal(t, first.Round.PublishAt.Time, second.Round.PublishAt.Time)
	require.NotNil(t, second.Round.PoolSize)
	assert.EqualValues(t, 4, *second.Round.PoolSize)
	require.Len(t, second.Result.Pairings, 2)

	stored, err := q.ListPairingsForRound(ctx, first.Round.ID)
	require.NoError(t, err)
	assert.Len(t, stored, 2, "the old pairings are gone, not added to")
	for _, p := range stored {
		assert.NotEqual(t, dropped.ID, p.WhiteUserID)
		assert.NotEqual(t, dropped.ID, p.BlackUserID)
	}

	exclusions, err := q.ListExclusionsForRound(ctx, first.Round.ID)
	require.NoError(t, err)
	var reasons []gen.ExclusionReason
	for _, e := range exclusions {
		if e.UserID == dropped.ID {
			reasons = append(reasons, e.Reason)
		}
	}
	assert.Equal(t, []gen.ExclusionReason{gen.ExclusionReasonInactive}, reasons, "exclusions are replaced, not duplicated")

	assert.True(t, audited(t, tx, "round.regenerate", first.Round.ID))
}

func TestRegenerateAndCancel_RefusePublishedRounds(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)

	cfg := settings.Defaults()
	cfg.PairingMode = settings.PairingModeAutoPublish
	round := generate(t, tx, cfg, nil).Round

	_, err := rounds.Regenerate(ctx, tx, round.ID, settings.Defaults(), pgtype.UUID{}, nil)
	assert.ErrorIs(t, err, rounds.ErrNotDraft)

	_, err = rounds.Cancel(ctx, tx, round.ID, "changed my mind", pgtype.UUID{})
	assert.ErrorIs(t, err, rounds.ErrNotDraft, "a published round may already have games behind it")
}

func TestCancel_GivesTheNumberBack(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	q := gen.New(tx)

	addPlayer(t, tx, "alpha", 2000)
	addPlayer(t, tx, "bravo", 1900)
	draft := generate(t, tx, settings.Defaults(), nil).Round

	cancelled, err := rounds.Cancel(ctx, tx, draft.ID, "pool was wrong", pgtype.UUID{})
	require.NoError(t, err)
	assert.Equal(t, gen.RoundStateCancelled, cancelled.State)
	require.NotNil(t, cancelled.Notes)
	assert.Equal(t, "pool was wrong", *cancelled.Notes)

	stored, err := q.ListPairingsForRound(ctx, draft.ID)
	require.NoError(t, err)
	for _, p := range stored {
		assert.Equal(t, gen.PairingStatusCancelled, p.Status)
	}

	// The sequence has no gaps: the next generation reuses the number.
	next := generate(t, tx, settings.Defaults(), nil).Round
	assert.Equal(t, draft.Number, next.Number)
}

func TestGenerate_HistoryReadsPublishedRoundsOnly(t *testing.T) {
	// alpha and bravo are by far the closest pair by rating, so they
	// are paired unless the repeat penalty separates them — and only a
	// published round counts as having met.
	tests := []struct {
		name          string
		state         gen.RoundState
		wantSeparated bool
	}{
		{name: "published", state: gen.RoundStatePublished, wantSeparated: true},
		{name: "draft", state: gen.RoundStateDraft},
		{name: "cancelled", state: gen.RoundStateCancelled},
	}

	// Only one draft may exist at a time, so the round under test is
	// generated in auto_publish mode: the draft case would otherwise
	// collide with the draft it is meant to ignore.
	cfg := settings.Defaults()
	cfg.PairingMode = settings.PairingModeAutoPublish

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := testTx(t)

			alpha := addPlayer(t, tx, "alpha", 2000)
			bravo := addPlayer(t, tx, "bravo", 1995)
			addPlayer(t, tx, "charlie", 1700)
			addPlayer(t, tx, "delta", 1695)
			addRound(t, tx, tc.state, [][2]gen.User{{alpha, bravo}})

			outcome := generate(t, tx, cfg, nil)

			var met bool
			for _, p := range outcome.Result.Pairings {
				if (p.White == alpha.ID.String() && p.Black == bravo.ID.String()) ||
					(p.White == bravo.ID.String() && p.Black == alpha.ID.String()) {
					met = true
				}
			}
			assert.Equal(t, tc.wantSeparated, !met)
			// Either way this is not a repeat: a published meeting is
			// avoided, and an unpublished one never happened.
			require.NotNil(t, outcome.Round.RepeatPairings)
			assert.EqualValues(t, 0, *outcome.Round.RepeatPairings)
		})
	}
}

// audited reads the audit_log directly: nothing else reads that table
// yet, so there is no query to reuse.
func audited(t *testing.T, tx pgx.Tx, action string, roundID int32) bool {
	t.Helper()
	var count int
	err := tx.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE action = $1 AND entity_type = 'round' AND entity_id = $2`,
		action, fmt.Sprint(roundID),
	).Scan(&count)
	require.NoError(t, err)
	return count > 0
}
