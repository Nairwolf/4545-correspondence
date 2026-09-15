//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// account builds a Fake /api/account profile for a Lichess user.
func account(id, username string) lichess.Account {
	a := lichess.Account{
		User:      lichess.User{ID: id, Username: username},
		CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		Verified:  true,
	}
	a.Perfs.Correspondence = &lichess.Perf{Games: 40, Rating: 1700, RD: 60}
	a.Count.Rated = 120
	return a
}

// grant registers a code → token → account triple on the Fake.
func grant(srv *Server, code string, token tokencrypt.Secret, a lichess.Account) {
	f := testFake(srv)
	f.Codes[code] = token
	f.Accounts[token] = a
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// startFlow performs the first leg (POST /join with the agreement, or
// GET /login) and returns the state Lichess would echo back plus the
// ic_oauth cookie the browser would carry.
func startFlow(t *testing.T, srv *Server, intent string) (string, *http.Cookie) {
	t.Helper()
	var req *http.Request
	if intent == intentJoin {
		req = httptest.NewRequest(http.MethodPost, "/join", strings.NewReader("agree=on"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(http.MethodGet, "/login", nil)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())

	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "challenge:write", loc.Query().Get("scope"))
	state := loc.Query().Get("state")
	require.NotEmpty(t, state)

	c := cookieNamed(rec, oauthCookieName)
	require.NotNil(t, c, "ic_oauth cookie must be set")
	assert.True(t, c.HttpOnly)
	return state, c
}

// callback performs the second leg with the given query and cookie.
func callback(t *testing.T, srv *Server, query url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/lichess/callback?"+query.Encode(), nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// signIn runs the whole flow for a code the Fake knows.
func signIn(t *testing.T, srv *Server, intent, code string) *httptest.ResponseRecorder {
	t.Helper()
	state, cookie := startFlow(t, srv, intent)
	return callback(t, srv, url.Values{"code": {code}, "state": {state}}, cookie)
}

// getAs performs a GET carrying the session cookie.
func getAs(t *testing.T, srv *Server, session *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if session != nil {
		req.AddCookie(session)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func countUsers(t *testing.T, tx pgx.Tx) int {
	t.Helper()
	var n int
	require.NoError(t, tx.QueryRow(context.Background(), "SELECT count(*) FROM users").Scan(&n))
	return n
}

func auditActions(t *testing.T, tx pgx.Tx, userID pgtype.UUID) []string {
	t.Helper()
	rows, err := tx.Query(context.Background(), "SELECT action FROM audit_log WHERE entity_id = $1 ORDER BY id", userID.String())
	require.NoError(t, err)
	defer rows.Close()
	var actions []string
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		actions = append(actions, a)
	}
	return actions
}

func TestJoin_CreatesPendingApplicant(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	grant(srv, "code-1", "lio_newbie", account("newbie", "Newbie"))

	rec := signIn(t, srv, intentJoin, "code-1")
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	assert.Equal(t, "/account", rec.Header().Get("Location"))
	session := cookieNamed(rec, "ic_session")
	require.NotNil(t, session)
	assert.True(t, session.HttpOnly)
	oauth := cookieNamed(rec, oauthCookieName)
	require.NotNil(t, oauth)
	assert.Less(t, oauth.MaxAge, 0, "ic_oauth cookie is cleared")

	user, err := q.GetUserByLichessUserID(ctx, "newbie")
	require.NoError(t, err)
	assert.Equal(t, "Newbie", user.LichessUsername)
	assert.Equal(t, gen.UserStatusPending, user.Status)
	assert.Equal(t, gen.UserRolePlayer, user.Role)
	assert.True(t, user.FairPlayAgreedAt.Valid)
	assert.Contains(t, string(user.LichessProfile), `"newbie"`)

	_, err = q.GetPlayerProfile(ctx, user.ID)
	require.NoError(t, err, "player_profiles row")
	snap, err := q.GetLatestRatingSnapshot(ctx, user.ID)
	require.NoError(t, err, "rating_snapshots row")
	assert.Equal(t, int32(1700), *snap.CorrespondenceRating)

	tok, err := q.GetOAuthToken(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"challenge:write"}, tok.Scopes)
	assert.NotContains(t, string(tok.AccessToken), "lio_newbie", "stored bytes are ciphertext")
	plain, err := srv.tokenKey.Open(tok.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, tokencrypt.Secret("lio_newbie"), plain)

	assert.Equal(t, []string{"registration.apply"}, auditActions(t, tx, user.ID))

	t.Run("account page shows pending and never the token", func(t *testing.T) {
		rec := getAs(t, srv, session, "/account")
		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		assert.Contains(t, body, "waiting for review")
		assert.Contains(t, body, "Newbie")
		assert.NotContains(t, body, "lio_newbie")
	})

	t.Run("signed-in nav", func(t *testing.T) {
		body := getAs(t, srv, session, "/").Body.String()
		assert.Contains(t, body, `href="/account"`)
		assert.NotContains(t, body, "Join the league")
	})
}

func TestJoin_RequiresAgreement(t *testing.T) {
	srv, _, tx := testServer(t)
	before := countUsers(t, tx)
	req := httptest.NewRequest(http.MethodPost, "/join", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "agree to the fair-play rules")
	assert.Nil(t, cookieNamed(rec, oauthCookieName))
	assert.Equal(t, before, countUsers(t, tx))
}

func TestLogin_ExistingMember(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	u := addPlayer(t, q, tx, "Solo", true, 1850, 15, 1850, 1860, "20")
	before := countUsers(t, tx)
	grant(srv, "code-1", "lio_solo", account("solo", "Solo"))

	rec := signIn(t, srv, intentLogin, "code-1")
	require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
	assert.Equal(t, before, countUsers(t, tx), "no new row")

	after, err := q.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.UserStatusApproved, after.Status)
	assert.True(t, after.LichessProfileFetchedAt.Valid)
	_, err = q.GetOAuthToken(ctx, u.ID)
	require.NoError(t, err, "token stored")
	assert.Equal(t, []string{"auth.sign_in"}, auditActions(t, tx, u.ID))

	t.Run("second sign-in is a re-authorisation", func(t *testing.T) {
		grant(srv, "code-2", "lio_solo2", account("solo", "Solo"))
		rec := signIn(t, srv, intentLogin, "code-2")
		require.Equal(t, http.StatusFound, rec.Code)
		tok, err := q.GetOAuthToken(ctx, u.ID)
		require.NoError(t, err)
		plain, err := srv.tokenKey.Open(tok.AccessToken)
		require.NoError(t, err)
		assert.Equal(t, tokencrypt.Secret("lio_solo2"), plain, "newest token wins")
		assert.Equal(t, []string{"auth.sign_in", "auth.reauthorise"}, auditActions(t, tx, u.ID))
	})

	t.Run("account page reads approved with an active authorisation", func(t *testing.T) {
		body := getAs(t, srv, cookieNamed(rec, "ic_session"), "/account").Body.String()
		assert.Contains(t, body, "approved member")
		assert.Contains(t, body, "Active")
	})
}

func TestLogin_NonMemberIsTurnedAwayAndTokenRevoked(t *testing.T) {
	srv, _, tx := testServer(t)
	before := countUsers(t, tx)
	grant(srv, "code-1", "lio_stranger", account("stranger", "Stranger"))

	rec := signIn(t, srv, intentLogin, "code-1")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "not a member yet")
	assert.Nil(t, cookieNamed(rec, "ic_session"))
	assert.Equal(t, before, countUsers(t, tx))
	assert.Equal(t, []tokencrypt.Secret{"lio_stranger"}, testFake(srv).Revoked)
}

func TestLogin_BannedGetsNoSession(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Cheater", true, 1500, 0, 1500, 1500, "0")
	_, err := tx.Exec(context.Background(), "UPDATE users SET status = 'banned' WHERE id = $1", u.ID)
	require.NoError(t, err)
	grant(srv, "code-1", "lio_cheater", account("cheater", "Cheater"))

	rec := signIn(t, srv, intentLogin, "code-1")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Nil(t, cookieNamed(rec, "ic_session"))
	assert.Equal(t, []tokencrypt.Secret{"lio_cheater"}, testFake(srv).Revoked)
	_, err = q.GetOAuthToken(context.Background(), u.ID)
	assert.ErrorIs(t, err, pgx.ErrNoRows, "nothing written")
}

func TestLogin_FollowsLichessRename(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Solo", true, 1850, 15, 1850, 1860, "20")
	grant(srv, "code-1", "lio_solo", account("solo", "SoLo"))

	rec := signIn(t, srv, intentLogin, "code-1")
	require.Equal(t, http.StatusFound, rec.Code)
	after, err := q.GetUserByID(context.Background(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, "SoLo", after.LichessUsername)
	assert.Equal(t, []string{"user.rename", "auth.sign_in"}, auditActions(t, tx, u.ID))
}

func TestBootstrapAdmin(t *testing.T) {
	t.Run("existing member is promoted", func(t *testing.T) {
		srv, q, tx := testServer(t)
		u := addPlayer(t, q, tx, "Boss", true, 2000, 0, 2000, 2000, "0")
		grant(srv, "code-1", "lio_boss", account("boss", "Boss"))

		rec := signIn(t, srv, intentLogin, "code-1")
		require.Equal(t, http.StatusFound, rec.Code)
		after, err := q.GetUserByID(context.Background(), u.ID)
		require.NoError(t, err)
		assert.Equal(t, gen.UserRoleAdmin, after.Role)
		assert.Contains(t, auditActions(t, tx, u.ID), "auth.bootstrap_admin")

		body := getAs(t, srv, cookieNamed(rec, "ic_session"), "/account").Body.String()
		assert.Contains(t, body, "admin")
	})

	t.Run("new applicant is created approved and admin", func(t *testing.T) {
		srv, q, tx := testServer(t)
		grant(srv, "code-1", "lio_boss", account("boss", "Boss"))

		rec := signIn(t, srv, intentJoin, "code-1")
		require.Equal(t, http.StatusFound, rec.Code)
		u, err := q.GetUserByLichessUserID(context.Background(), "boss")
		require.NoError(t, err)
		assert.Equal(t, gen.UserStatusApproved, u.Status)
		assert.Equal(t, gen.UserRoleAdmin, u.Role)
		assert.True(t, u.ApprovedAt.Valid)
		assert.False(t, u.ApprovedBy.Valid, "approved by the system, not a person")
		assert.Equal(t, []string{"registration.apply", "auth.bootstrap_admin"}, auditActions(t, tx, u.ID))
	})

	t.Run("listed name with no row cannot log in, only join", func(t *testing.T) {
		srv, _, tx := testServer(t)
		before := countUsers(t, tx)
		grant(srv, "code-1", "lio_boss", account("boss", "Boss"))
		rec := signIn(t, srv, intentLogin, "code-1")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "not a member yet")
		assert.Equal(t, before, countUsers(t, tx))
	})
}

func TestCallback_RejectsUnverifiableState(t *testing.T) {
	srv, _, tx := testServer(t)
	before := countUsers(t, tx)
	grant(srv, "code-1", "lio_newbie", account("newbie", "Newbie"))

	t.Run("state mismatch", func(t *testing.T) {
		_, cookie := startFlow(t, srv, intentJoin)
		rec := callback(t, srv, url.Values{"code": {"code-1"}, "state": {"forged"}}, cookie)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "could not be verified")
	})

	t.Run("missing cookie", func(t *testing.T) {
		state, _ := startFlow(t, srv, intentJoin)
		rec := callback(t, srv, url.Values{"code": {"code-1"}, "state": {state}}, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("expired cookie", func(t *testing.T) {
		state, cookie := startFlow(t, srv, intentJoin)
		stale, err := signState(srv.stateSecret, oauthState{State: state, Verifier: "v", Intent: intentJoin, Expires: time.Now().Add(-time.Minute).Unix()})
		require.NoError(t, err)
		cookie.Value = stale
		rec := callback(t, srv, url.Values{"code": {"code-1"}, "state": {state}}, cookie)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("cancelled on lichess", func(t *testing.T) {
		state, cookie := startFlow(t, srv, intentJoin)
		rec := callback(t, srv, url.Values{"error": {"access_denied"}, "state": {state}}, cookie)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "cancelled")
		assert.Nil(t, cookieNamed(rec, "ic_session"))
	})

	t.Run("unknown code", func(t *testing.T) {
		state, cookie := startFlow(t, srv, intentJoin)
		rec := callback(t, srv, url.Values{"code": {"nope"}, "state": {state}}, cookie)
		assert.Equal(t, http.StatusBadGateway, rec.Code)
	})

	assert.Equal(t, before, countUsers(t, tx), "no path above writes anything")
}

func TestAccount_RequiresSignIn(t *testing.T) {
	srv, _, _ := testServer(t)
	rec := getAs(t, srv, nil, "/account")
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/login", rec.Header().Get("Location"))
}

// sessionsOf counts the sessions of one Lichess account. Scoped to the
// user on purpose: the integration database is the dev database, which
// holds the maintainer's own live sessions outside this transaction.
func sessionsOf(t *testing.T, tx pgx.Tx, lichessID string) int {
	t.Helper()
	var n int
	require.NoError(t, tx.QueryRow(context.Background(),
		"SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.lichess_user_id = $1", lichessID).Scan(&n))
	return n
}

func TestLogout(t *testing.T) {
	srv, _, tx := testServer(t)
	grant(srv, "code-1", "lio_newbie", account("newbie", "Newbie"))
	session := cookieNamed(signIn(t, srv, intentJoin, "code-1"), "ic_session")
	require.NotNil(t, session)

	post := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/logout", nil)
		req.AddCookie(session)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("cross-site post is refused", func(t *testing.T) {
		rec := post(map[string]string{"Sec-Fetch-Site": "cross-site"})
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, http.StatusOK, getAs(t, srv, session, "/account").Code, "still signed in")
	})

	t.Run("same-site post signs out", func(t *testing.T) {
		rec := post(map[string]string{"Sec-Fetch-Site": "same-origin"})
		assert.Equal(t, http.StatusFound, rec.Code)
		cleared := cookieNamed(rec, "ic_session")
		require.NotNil(t, cleared)
		assert.Less(t, cleared.MaxAge, 0)

		assert.Equal(t, 0, sessionsOf(t, tx, "newbie"), "session row gone")
		assert.Equal(t, http.StatusFound, getAs(t, srv, session, "/account").Code, "old cookie no longer works")
	})
}

func TestSession_ExpiryAndTouch(t *testing.T) {
	srv, _, tx := testServer(t)
	ctx := context.Background()
	grant(srv, "code-1", "lio_newbie", account("newbie", "Newbie"))
	session := cookieNamed(signIn(t, srv, intentJoin, "code-1"), "ic_session")
	require.NotNil(t, session)

	// Every query below is scoped to this test's user (see sessionsOf).
	const mine = "user_id = (SELECT id FROM users WHERE lichess_user_id = 'newbie')"
	lastSeen := func() time.Time {
		var ts time.Time
		require.NoError(t, tx.QueryRow(ctx, "SELECT last_seen_at FROM sessions WHERE "+mine).Scan(&ts))
		return ts
	}

	t.Run("a fresh session is not touched on every request", func(t *testing.T) {
		before := lastSeen()
		require.Equal(t, http.StatusOK, getAs(t, srv, session, "/account").Code)
		assert.Equal(t, before, lastSeen())
	})

	t.Run("an hour-old last_seen_at is refreshed", func(t *testing.T) {
		_, err := tx.Exec(ctx, "UPDATE sessions SET last_seen_at = now() - interval '2 hours' WHERE "+mine)
		require.NoError(t, err)
		before := lastSeen()
		require.Equal(t, http.StatusOK, getAs(t, srv, session, "/account").Code)
		assert.True(t, lastSeen().After(before))
	})

	t.Run("an expired session is not loaded", func(t *testing.T) {
		_, err := tx.Exec(ctx, "UPDATE sessions SET expires_at = now() - interval '1 hour' WHERE "+mine)
		require.NoError(t, err)
		assert.Equal(t, http.StatusFound, getAs(t, srv, session, "/account").Code)
	})
}

func TestBannedMidSessionIsSignedOut(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	grant(srv, "code-1", "lio_newbie", account("newbie", "Newbie"))
	session := cookieNamed(signIn(t, srv, intentJoin, "code-1"), "ic_session")
	u, err := q.GetUserByLichessUserID(ctx, "newbie")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "UPDATE users SET status = 'banned' WHERE id = $1", u.ID)
	require.NoError(t, err)

	rec := getAs(t, srv, session, "/account")
	assert.Equal(t, http.StatusFound, rec.Code, "treated as anonymous")
	assert.Equal(t, 0, sessionsOf(t, tx, "newbie"), "session destroyed")
}
