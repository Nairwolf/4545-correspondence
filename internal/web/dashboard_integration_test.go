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
	assert.False(t, standing.IsEligible, "Recompute ran: is_eligible follows is_active")
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
