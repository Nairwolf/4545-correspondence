//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// profileOf re-reads a player's profile row.
func profileOf(t *testing.T, q *gen.Queries, u gen.User) gen.PlayerProfile {
	t.Helper()
	p, err := q.GetPlayerProfile(context.Background(), u.ID)
	require.NoError(t, err)
	return p
}

// addGame writes a round, a pairing and a games row between two
// players, finished (with result) or in progress (result nil).
func addGame(t *testing.T, q *gen.Queries, round int32, white, black gen.User, id string, result *gen.GameResult, lastMove time.Time) {
	t.Helper()
	ctx := context.Background()
	ts := pgtype.Timestamptz{Time: lastMove, Valid: true}
	r, err := q.GetRoundByNumber(ctx, round)
	if err != nil {
		r, err = q.CreateRound(ctx, gen.CreateRoundParams{
			Number: round, State: gen.RoundStatePublished,
			PublishAt: ts, PublishedAt: ts, PairAt: ts, GeneratedBy: gen.RoundSourceImported,
		})
		require.NoError(t, err)
	}
	pairing, err := q.UpsertManualPairing(ctx, gen.UpsertManualPairingParams{
		RoundID: r.ID, WhiteUserID: white.ID, BlackUserID: black.ID, CreationMethod: gen.PairingMethodManualExternal,
	})
	require.NoError(t, err)
	params := gen.UpsertGameParams{
		LichessGameID: id, PairingID: pairing.ID, RoundNumber: round,
		WhiteUserID: white.ID, BlackUserID: black.ID,
		Status: gen.GameStatusInProgress, LichessStatus: "started",
		StartedAt: pgtype.Timestamptz{Time: lastMove.Add(-72 * time.Hour), Valid: true}, LastMoveAt: ts,
		RawPayload: []byte(`{}`),
	}
	pairingStatus := gen.PairingStatusInProgress
	if result != nil {
		termination := gen.GameTerminationResign
		params.Status, params.LichessStatus, params.FinishedAt = gen.GameStatusFinished, "resign", ts
		params.Result, params.Termination = result, &termination
		pairingStatus = gen.PairingStatusCompleted
	}
	_, err = q.UpsertGame(ctx, params)
	require.NoError(t, err)

	// As sync-games does: a pairing left pending with no game id counts
	// as a game in flight (spec §5.8, decided 2026-09-17), so the
	// fixture would otherwise show every game twice.
	require.NoError(t, q.AttachGameToPairing(ctx, gen.AttachGameToPairingParams{
		ID:            pairing.ID,
		LichessGameID: &id,
		Status:        pairingStatus,
	}))
}

func TestDashboard_Guards(t *testing.T) {
	srv, q, tx := testServer(t)

	t.Run("anonymous is sent to sign in", func(t *testing.T) {
		for _, path := range []string{"/account", "/account/activity", "/account/capacity", "/account/double-games", "/account/resume"} {
			rec := postAs(t, srv, nil, path, url.Values{})
			if path == "/account" {
				rec = getAs(t, srv, nil, path)
			}
			assert.Equal(t, http.StatusFound, rec.Code, path)
			assert.Equal(t, "/login", rec.Header().Get("Location"), path)
		}
	})

	t.Run("rejected applicant sees status only and cannot configure", func(t *testing.T) {
		u := applicant(t, q, "nope", "Nope", account("nope", "Nope"))
		reason := "duplicate account"
		_, err := q.RejectUser(context.Background(), gen.RejectUserParams{ID: u.ID, RejectionReason: &reason})
		require.NoError(t, err)
		u.Status = gen.UserStatusRejected
		session := sessionFor(t, srv, u)

		rec := getAs(t, srv, session, "/account")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "duplicate account")
		assert.NotContains(t, rec.Body.String(), "/account/activity")

		rec = postAs(t, srv, session, "/account/activity", url.Values{})
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.True(t, profileOf(t, q, u).IsActive, "nothing written")
	})

	t.Run("pending applicant can set preferences", func(t *testing.T) {
		u := applicant(t, q, "newbie", "Newbie", account("newbie", "Newbie"))
		session := sessionFor(t, srv, u)
		rec := postAs(t, srv, session, "/account/double-games", url.Values{}) // unticked
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.False(t, profileOf(t, q, u).AcceptsDoubleGame)
	})
	_ = tx
}

func TestDashboard_Activity(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Player", true, 1800, 10, 1800, 1800, "20")
	session := sessionFor(t, srv, u)

	rec := postAs(t, srv, session, "/account/activity", url.Values{}) // "Pause my quest": no active field
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/account?saved=activity", rec.Header().Get("Location"))
	assert.False(t, profileOf(t, q, u).IsActive)

	standing, err := q.GetPlayerStanding(context.Background(), u.ID)
	require.NoError(t, err)
	assert.False(t, standing.IsEligible, "Recompute ran: is_active is an input to is_eligible")
	assert.Equal(t, []string{"profile.activity"}, auditActions(t, tx, u.ID))

	// Same value again: nothing to change, nothing to audit.
	rec = postAs(t, srv, session, "/account/activity", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, []string{"profile.activity"}, auditActions(t, tx, u.ID))

	rec = postAs(t, srv, session, "/account/activity", url.Values{"active": {"on"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.True(t, profileOf(t, q, u).IsActive)
	assert.Len(t, auditActions(t, tx, u.ID), 2)

	body := getAs(t, srv, session, "/account?saved=activity").Body.String()
	assert.Contains(t, body, "Activity updated")
	assert.Contains(t, body, "Pause my quest")
}

func TestDashboard_Capacity(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Capped", true, 1800, 10, 1800, 1800, "20")
	session := sessionFor(t, srv, u)

	t.Run("set a cap", func(t *testing.T) {
		rec := postAs(t, srv, session, "/account/capacity", url.Values{"limit": {"on"}, "cap": {"4"}})
		require.Equal(t, http.StatusSeeOther, rec.Code)
		p := profileOf(t, q, u)
		require.NotNil(t, p.MaxConcurrentGames)
		assert.Equal(t, int32(4), *p.MaxConcurrentGames)
		assert.Equal(t, []string{"profile.capacity"}, auditActions(t, tx, u.ID))
	})

	t.Run("invalid values are refused and change nothing", func(t *testing.T) {
		for _, cap := range []string{"0", "21", "abc", ""} {
			rec := postAs(t, srv, session, "/account/capacity", url.Values{"limit": {"on"}, "cap": {cap}})
			assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, "cap=%q", cap)
			assert.Contains(t, rec.Body.String(), "between 1 and 20")
			p := profileOf(t, q, u)
			require.NotNil(t, p.MaxConcurrentGames)
			assert.Equal(t, int32(4), *p.MaxConcurrentGames)
		}
		assert.Equal(t, []string{"profile.capacity"}, auditActions(t, tx, u.ID), "no audit rows for refused values")
	})

	t.Run("the ceiling comes from settings", func(t *testing.T) {
		srv.cfg.MaxConcurrentCeiling = 30
		defer func() { srv.cfg.MaxConcurrentCeiling = 20 }()
		rec := postAs(t, srv, session, "/account/capacity", url.Values{"limit": {"on"}, "cap": {"25"}})
		require.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, int32(25), *profileOf(t, q, u).MaxConcurrentGames)
	})

	t.Run("limit off is NULL, never 0", func(t *testing.T) {
		rec := postAs(t, srv, session, "/account/capacity", url.Values{"cap": {"4"}}) // checkbox unticked
		require.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Nil(t, profileOf(t, q, u).MaxConcurrentGames)
		var isNull bool
		require.NoError(t, tx.QueryRow(context.Background(),
			"SELECT max_concurrent_games IS NULL FROM player_profiles WHERE user_id = $1", u.ID).Scan(&isNull))
		assert.True(t, isNull)
	})
}

func TestDashboard_CapacitySentences(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Busy", true, 1800, 10, 1800, 1800, "20")
	opp := addPlayer(t, q, tx, "Opp", true, 1800, 10, 1800, 1800, "20")
	session := sessionFor(t, srv, u)
	now := time.Now()
	addGame(t, q, 1, u, opp, "g1", nil, now.Add(-49*time.Hour))
	addGame(t, q, 2, opp, u, "g2", nil, now)

	body := getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, "2 games in progress — no limit set")
	assert.Contains(t, body, "2 days since last move")
	assert.Contains(t, body, "moved today")
	assert.Contains(t, body, "In progress (2)")

	// html/template escapes the apostrophe in the dynamic sentence.
	set := func(cap string) string {
		rec := postAs(t, srv, session, "/account/capacity", url.Values{"limit": {"on"}, "cap": {cap}})
		require.Equal(t, http.StatusSeeOther, rec.Code)
		return getAs(t, srv, session, "/account").Body.String()
	}
	assert.Contains(t, set("3"), "2 of 3 games in progress — you&#39;ll be paired this week.")
	body = set("2")
	assert.Contains(t, body, "2 of 2 games in progress — you&#39;ll be skipped until one finishes.")
	assert.NotContains(t, body, "above your new limit")
	body = set("1")
	assert.Contains(t, body, "2 of 1 games in progress — you&#39;ll be skipped until one finishes.")
	assert.Contains(t, body, "above your new limit")
}

func TestDashboard_DoubleGamesAndResume(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Vol", true, 1800, 10, 1800, 1800, "20")
	session := sessionFor(t, srv, u)

	rec := postAs(t, srv, session, "/account/double-games", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.False(t, profileOf(t, q, u).AcceptsDoubleGame)
	rec = postAs(t, srv, session, "/account/double-games", url.Values{"accept": {"on"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.True(t, profileOf(t, q, u).AcceptsDoubleGame)
	assert.Equal(t, []string{"profile.double_games", "profile.double_games"}, auditActions(t, tx, u.ID))

	// Resume when not auto-paused: no-op, no audit, no button.
	body := getAs(t, srv, session, "/account").Body.String()
	assert.NotContains(t, body, "Resume quest")
	rec = postAs(t, srv, session, "/account/resume", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Len(t, auditActions(t, tx, u.ID), 2)

	_, err := tx.Exec(context.Background(), "UPDATE player_profiles SET auto_paused_at = now() WHERE user_id = $1", u.ID)
	require.NoError(t, err)
	body = getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, "Your quest is paused")
	assert.Contains(t, body, "Resume quest")

	rec = postAs(t, srv, session, "/account/resume", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.False(t, profileOf(t, q, u).AutoPausedAt.Valid)
	assert.Equal(t, "profile.resume", auditActions(t, tx, u.ID)[2])

	_, err = tx.Exec(context.Background(), "UPDATE player_profiles SET paused_by_admin = true, paused_reason = 'unanswered messages' WHERE user_id = $1", u.ID)
	require.NoError(t, err)
	body = getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, "An admin has paused your quest")
	assert.Contains(t, body, "unanswered messages")
	assert.NotContains(t, body, "Resume quest")
}

func TestDashboard_AuthorisationAndGames(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Vet", true, 1800, 10, 1800, 1800, "20")
	opp := addPlayer(t, q, tx, "Rival", true, 1800, 10, 1800, 1800, "20")
	session := sessionFor(t, srv, u)

	body := getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, "Games can't be created for you automatically", "seeded member without a token")
	assert.Contains(t, body, "You have not authorised the league yet")

	// A stored, valid token.
	sealed, err := srv.tokenKey.Seal("lio_secret_plaintext")
	require.NoError(t, err)
	require.NoError(t, q.UpsertOAuthToken(context.Background(), gen.UpsertOAuthTokenParams{
		UserID: u.ID, AccessToken: sealed, Scopes: []string{"challenge:write"},
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(300 * 24 * time.Hour), Valid: true},
	}))
	body = getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, "the league can create your games")
	assert.NotContains(t, body, "Games can't be created")
	assert.NotContains(t, body, "lio_secret_plaintext")

	for _, col := range []string{"revoked_at = now()", "expires_at = now() - interval '1 day'"} {
		_, err := tx.Exec(context.Background(), "UPDATE oauth_tokens SET "+col+" WHERE user_id = $1", u.ID)
		require.NoError(t, err)
		body = getAs(t, srv, session, "/account").Body.String()
		assert.Contains(t, body, "Games can't be created for you automatically", col)
		assert.Contains(t, body, "expired or was revoked", col)
	}

	// Games: one in progress, twelve finished → ten shown plus a pointer.
	now := time.Now()
	addGame(t, q, 1, u, opp, "live", nil, now)
	win := gen.GameResultWhiteWin
	for i := 0; i < 12; i++ {
		addGame(t, q, int32(10+i), u, opp, "fin"+string(rune('a'+i)), &win, now.Add(-time.Duration(i+1)*24*time.Hour))
	}
	body = getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, "In progress (1)")
	assert.Contains(t, body, `href="https://lichess.org/live"`)
	assert.Contains(t, body, "Only the last 10 are shown")
	assert.Contains(t, body, "https://lichess.org/fina")    // most recent finished
	assert.NotContains(t, body, "https://lichess.org/finl") // the twelfth is not
}

// thisWeekRound writes a round numbered far past any real one, so it is
// the latest whatever the test database already holds.
func thisWeekRound(t *testing.T, q *gen.Queries, number int32, state gen.RoundState, settingsUsed string) gen.Round {
	t.Helper()
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	params := gen.CreateGeneratedRoundParams{
		Number: number, State: state, PublishAt: now, PairAt: now,
		GeneratedBy: gen.RoundSourceSchedule,
	}
	if state == gen.RoundStatePublished {
		params.PublishedAt = now
	}
	if settingsUsed != "" {
		params.SettingsUsed = []byte(settingsUsed)
	}
	r, err := q.CreateGeneratedRound(context.Background(), params)
	require.NoError(t, err)
	return r
}

func pairIn(t *testing.T, q *gen.Queries, r gen.Round, white, black gen.User) gen.Pairing {
	t.Helper()
	p, err := q.InsertGeneratedPairing(context.Background(), gen.InsertGeneratedPairingParams{
		RoundID: r.ID, WhiteUserID: white.ID, BlackUserID: black.ID,
	})
	require.NoError(t, err)
	return p
}

func excludeIn(t *testing.T, q *gen.Queries, r gen.Round, u gen.User, reason gen.ExclusionReason, ongoing, cap *int32) {
	t.Helper()
	_, err := q.InsertRoundExclusion(context.Background(), gen.InsertRoundExclusionParams{
		RoundID: r.ID, UserID: u.ID, Reason: reason, OngoingGames: ongoing, MaxConcurrentGames: cap,
	})
	require.NoError(t, err)
}

func TestDashboard_ThisWeek(t *testing.T) {
	ctx := context.Background()

	t.Run("no published round: no section", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Fresh", true, 1800, 10, 1800, 1800, "20")
		var published int
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM rounds WHERE state = 'published'").Scan(&published))
		if published > 0 {
			t.Skip("the test database already holds published rounds")
		}
		rec := getAs(t, srv, sessionFor(t, srv, u), "/account")
		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, rec.Body.String(), "This week")
	})

	t.Run("paired: opponent, colour, challenge link, then the game", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Walter", true, 1800, 10, 1800, 1800, "20")
		opp := addPlayer(t, q, tx, "Brenda", true, 1800, 10, 1800, 1800, "20")
		r := thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")
		p := pairIn(t, q, r, u, opp)
		session := sessionFor(t, srv, u)

		body := getAs(t, srv, session, "/account").Body.String()
		assert.Contains(t, body, "This week · round 9000")
		assert.Contains(t, body, ">Brenda</a> with white.")
		assert.Contains(t, body, `href="https://lichess.org/@/Brenda">challenge them on Lichess`)
		assert.Contains(t, body, "Start the game on Lichess as you do today")

		oppBody := getAs(t, srv, sessionFor(t, srv, opp), "/account").Body.String()
		assert.Contains(t, oppBody, ">Walter</a> with black.")

		id := "thisweek1"
		require.NoError(t, q.AttachGameToPairing(ctx, gen.AttachGameToPairingParams{
			ID: p.ID, LichessGameID: &id, Status: gen.PairingStatusInProgress,
		}))
		body = getAs(t, srv, session, "/account").Body.String()
		assert.Contains(t, body, `href="https://lichess.org/thisweek1">open on Lichess`)
		assert.NotContains(t, body, "challenge them on Lichess")
		assert.NotContains(t, body, "Start the game on Lichess as you do today")
	})

	t.Run("double game: both opponents, one white one black", func(t *testing.T) {
		srv, q, tx := testServer(t)
		vol := addPlayer(t, q, tx, "Volunteer", true, 1800, 10, 1800, 1800, "20")
		a := addPlayer(t, q, tx, "Alpha", true, 1800, 10, 1800, 1800, "20")
		b := addPlayer(t, q, tx, "Bravo", true, 1800, 10, 1800, 1800, "20")
		r := thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")
		pairIn(t, q, r, vol, a)
		pairIn(t, q, r, b, vol)
		_, err := q.InsertDoubleGame(ctx, gen.InsertDoubleGameParams{RoundID: r.ID, UserID: vol.ID})
		require.NoError(t, err)

		body := getAs(t, srv, sessionFor(t, srv, vol), "/account").Body.String()
		assert.Contains(t, body, ">Alpha</a> with white.")
		assert.Contains(t, body, ">Bravo</a> with black.")
		assert.Contains(t, body, "you're playing two games") // static template text: not escaped
		assert.Contains(t, body, "You&#39;ve played 1 double game so far.")
	})

	t.Run("pairing marked failed", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Ghosted", true, 1800, 10, 1800, 1800, "20")
		opp := addPlayer(t, q, tx, "Absent", true, 1800, 10, 1800, 1800, "20")
		r := thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")
		p := pairIn(t, q, r, u, opp)
		require.NoError(t, q.MarkPairingFailed(ctx, p.ID))

		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.Contains(t, body, ">Absent</a> with white. <span class=\"text-zinc-500\">Marked as not played.</span>")
		assert.NotContains(t, body, "challenge them on Lichess")
	})

	t.Run("bye, and the bye history", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Resting", true, 1800, 10, 1800, 1800, "20")
		for _, n := range []int32{9000, 9003} {
			r := thisWeekRound(t, q, n, gen.RoundStatePublished, `{"odd_pool_strategy":"double_then_bye"}`)
			_, err := q.InsertBye(ctx, gen.InsertByeParams{RoundID: r.ID, UserID: u.ID})
			require.NoError(t, err)
			excludeIn(t, q, r, u, gen.ExclusionReasonBye, nil, nil)
		}

		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.Contains(t, body, "This week · round 9003")
		assert.Contains(t, body, "Odd number of players this week and nobody was free for a double game, so you sat out. You&#39;re first in line to avoid the next one.")
		assert.Contains(t, body, "Byes: 2 byes so far, most recently in round 9003.")
	})

	t.Run("at capacity, with the numbers", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Busy", true, 1800, 10, 1800, 1800, "20")
		r := thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")
		four := int32(4)
		excludeIn(t, q, r, u, gen.ExclusionReasonAtCapacity, &four, &four)

		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.Contains(t, body, "You had 4 games in progress and your limit is 4, so you sat out round 9000.")
		assert.NotContains(t, body, "Byes:", "no byes, no history line")
	})

	t.Run("removed by an admin", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Dropped", true, 1800, 10, 1800, 1800, "20")
		r := thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")
		excludeIn(t, q, r, u, gen.ExclusionReasonRemovedByAdmin, nil, nil)

		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.Contains(t, body, "An admin removed your pairing for round 9000.")
	})

	t.Run("approved after the round was paired", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Newcomer", true, 1800, 10, 1800, 1800, "20")
		thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")

		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.Contains(t, body, "Round 9000 was paired before you joined — you&#39;ll be in the next one.")
	})

	t.Run("a newer draft is never shown", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Patient", true, 1800, 10, 1800, 1800, "20")
		opp := addPlayer(t, q, tx, "Future", true, 1800, 10, 1800, 1800, "20")
		thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")
		draft := thisWeekRound(t, q, 9001, gen.RoundStateDraft, "")
		pairIn(t, q, draft, u, opp)

		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.Contains(t, body, "This week · round 9000")
		assert.NotContains(t, body, "Future")
	})

	t.Run("pending applicants see no section", func(t *testing.T) {
		srv, q, _ := testServer(t)
		u := applicant(t, q, "waiting", "Waiting", account("waiting", "Waiting"))
		thisWeekRound(t, q, 9000, gen.RoundStatePublished, "")

		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.NotContains(t, body, "This week")
	})
}

func TestDashboard_Eligibility(t *testing.T) {
	ctx := context.Background()
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Eligible", true, 1800, 10, 1800, 1800, "20")
	eligible := func() bool {
		t.Helper()
		_, err := standings.Recompute(ctx, q, u.ID, srv.cfg)
		require.NoError(t, err)
		s, err := q.GetPlayerStanding(ctx, u.ID)
		require.NoError(t, err)
		return s.IsEligible
	}
	set := func(sql string) {
		t.Helper()
		_, err := tx.Exec(ctx, sql, u.ID)
		require.NoError(t, err)
	}

	assert.True(t, eligible(), "approved and active")

	set("UPDATE player_profiles SET paused_by_admin = true WHERE user_id = $1")
	assert.False(t, eligible(), "an admin pause alone makes the player ineligible")
	set("UPDATE player_profiles SET paused_by_admin = false WHERE user_id = $1")
	assert.True(t, eligible())

	set("UPDATE player_profiles SET auto_paused_at = now() WHERE user_id = $1")
	assert.False(t, eligible(), "an auto-pause alone makes the player ineligible")
	assert.True(t, profileOf(t, q, u).IsActive, "while still active")

	// Resume recomputes in its own transaction: no nightly wait.
	rec := postAs(t, srv, sessionFor(t, srv, u), "/account/resume", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	s, err := q.GetPlayerStanding(ctx, u.ID)
	require.NoError(t, err)
	assert.True(t, s.IsEligible, "eligible again straight after resuming")
}
