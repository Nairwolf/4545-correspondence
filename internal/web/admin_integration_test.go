//go:build integration

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
)

// sessionFor signs userID in directly and returns the cookie.
func sessionFor(t *testing.T, srv *Server, u gen.User) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	require.NoError(t, srv.sessions.Create(context.Background(), rec, u.ID))
	c := cookieNamed(rec, "ic_session")
	require.NotNil(t, c)
	return c
}

// addAdmin seeds an approved admin and returns their session.
func addAdmin(t *testing.T, srv *Server, q *gen.Queries, tx pgx.Tx) (gen.User, *http.Cookie) {
	t.Helper()
	u := addPlayer(t, q, tx, "Boss", true, 2000, 0, 2000, 2000, "0")
	_, err := tx.Exec(context.Background(), "UPDATE users SET role = 'admin' WHERE id = $1", u.ID)
	require.NoError(t, err)
	u.Role = gen.UserRoleAdmin
	return u, sessionFor(t, srv, u)
}

// applicant creates a pending user the way the join flow would, with a
// stored Lichess profile.
func applicant(t *testing.T, q *gen.Queries, id, username string, a lichess.Account) gen.User {
	t.Helper()
	raw, err := json.Marshal(a)
	require.NoError(t, err)
	u, err := q.CreatePendingUser(context.Background(), gen.CreatePendingUserParams{
		LichessUsername: username, LichessUserID: id, LichessProfile: raw,
	})
	require.NoError(t, err)
	_, err = q.CreatePlayerProfile(context.Background(), u.ID)
	require.NoError(t, err)
	return u
}

func postAs(t *testing.T, srv *Server, session *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if session != nil {
		req.AddCookie(session)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAdmin_RoleChecks(t *testing.T) {
	srv, q, tx := testServer(t)
	player := addPlayer(t, q, tx, "Pleb", true, 1500, 0, 1500, 1500, "0")
	playerSession := sessionFor(t, srv, player)

	for _, path := range []string{"/admin", "/admin/registrations", "/admin/jobs"} {
		rec := getAs(t, srv, nil, path)
		assert.Equal(t, http.StatusFound, rec.Code, "anonymous %s", path)
		assert.Equal(t, "/login", rec.Header().Get("Location"))

		assert.Equal(t, http.StatusForbidden, getAs(t, srv, playerSession, path).Code, "player %s", path)
	}
	rec := postAs(t, srv, playerSession, "/admin/registrations/approve", url.Values{"ids": {player.ID.String()}})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	assert.Equal(t, http.StatusNotFound, getAs(t, srv, nil, "/jobs").Code, "moved under /admin")
}

func TestAdmin_QueueShowsSignals(t *testing.T) {
	srv, q, tx := testServer(t)
	_, admin := addAdmin(t, srv, q, tx)
	fresh := account("m1lsbees", "M1lsBees")
	fresh.CreatedAt = time.Now().Add(-10 * 24 * time.Hour).UnixMilli()
	fresh.Count.Rated = 7
	fresh.TOSViolation = true
	fresh.Perfs.Classical = &lichess.Perf{Games: 2, Rating: 1450, RD: 300, Provisional: true}
	applicant(t, q, "m1lsbees", "M1lsBees", fresh)

	rec := getAs(t, srv, admin, "/admin/registrations")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "M1lsBees")
	assert.Contains(t, body, "10 days old")
	assert.Contains(t, body, "Marked for TOS violation")
	assert.Contains(t, body, ">7<")
	assert.Contains(t, body, "1700")
	assert.Contains(t, body, `1450<span class="text-zinc-500" title="provisional">?</span>`)
	assert.Contains(t, body, "Pending <span class=\"text-zinc-500\">(1)</span>")

	t.Run("admin nav is visible", func(t *testing.T) {
		assert.Contains(t, body, `href="/admin/registrations"`)
		assert.Contains(t, body, `href="/admin/jobs"`)
		assert.Equal(t, http.StatusOK, getAs(t, srv, admin, "/admin/jobs").Code)
	})
}

func TestAdmin_Approve(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	adminUser, admin := addAdmin(t, srv, q, tx)
	u := applicant(t, q, "newbie", "Newbie", account("newbie", "Newbie"))

	rec := postAs(t, srv, admin, "/admin/registrations/approve", url.Values{"ids": {u.ID.String()}, "tab": {"pending"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/admin/registrations", rec.Header().Get("Location"))

	after, err := q.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.UserStatusApproved, after.Status)
	assert.True(t, after.ApprovedAt.Valid)
	assert.Equal(t, adminUser.ID, after.ApprovedBy)
	assert.Equal(t, []string{"registration.approve"}, auditActions(t, tx, u.ID))

	_, err = q.GetPlayerStanding(ctx, u.ID)
	require.NoError(t, err, "standings row computed on approval")
	assert.Contains(t, get2(t, srv, "/standings"), `/players/Newbie">Newbie`)

	t.Run("approving again is stale", func(t *testing.T) {
		rec := postAs(t, srv, admin, "/admin/registrations/approve", url.Values{"ids": {u.ID.String()}})
		assert.Equal(t, "/admin/registrations?error=stale", rec.Header().Get("Location"))
	})
}

// get2 is get's body only, for one-line assertions.
func get2(t *testing.T, srv *Server, path string) string {
	t.Helper()
	_, body := get(t, srv, path)
	return body
}

func TestAdmin_BulkApproveIsAtomic(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	_, admin := addAdmin(t, srv, q, tx)
	a := applicant(t, q, "aa", "Aa", account("aa", "Aa"))
	b := applicant(t, q, "bb", "Bb", account("bb", "Bb"))
	_, err := tx.Exec(ctx, "UPDATE users SET status = 'banned' WHERE id = $1", b.ID)
	require.NoError(t, err)

	rec := postAs(t, srv, admin, "/admin/registrations/approve", url.Values{"ids": {a.ID.String(), b.ID.String()}})
	assert.Equal(t, "/admin/registrations?error=stale", rec.Header().Get("Location"))

	after, err := q.GetUserByID(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.UserStatusPending, after.Status, "the good one was rolled back too")
	assert.Empty(t, auditActions(t, tx, a.ID))

	t.Run("both valid approves both", func(t *testing.T) {
		_, err := tx.Exec(ctx, "UPDATE users SET status = 'pending' WHERE id = $1", b.ID)
		require.NoError(t, err)
		rec := postAs(t, srv, admin, "/admin/registrations/approve", url.Values{"ids": {a.ID.String(), b.ID.String()}})
		assert.Equal(t, "/admin/registrations", rec.Header().Get("Location"))
		for _, id := range []gen.User{a, b} {
			u, err := q.GetUserByID(ctx, id.ID)
			require.NoError(t, err)
			assert.Equal(t, gen.UserStatusApproved, u.Status)
		}
	})

	t.Run("garbage id", func(t *testing.T) {
		rec := postAs(t, srv, admin, "/admin/registrations/approve", url.Values{"ids": {"not-a-uuid"}})
		assert.Equal(t, "/admin/registrations?error=bad-id", rec.Header().Get("Location"))
	})
}

func TestAdmin_Reject(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	_, admin := addAdmin(t, srv, q, tx)
	u := applicant(t, q, "newbie", "Newbie", account("newbie", "Newbie"))
	path := "/admin/registrations/" + u.ID.String() + "/reject"

	t.Run("reason required", func(t *testing.T) {
		rec := postAs(t, srv, admin, path, url.Values{"reason": {"   "}})
		assert.Equal(t, "/admin/registrations?error=reason", rec.Header().Get("Location"))
		after, err := q.GetUserByID(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, gen.UserStatusPending, after.Status)
		body := getAs(t, srv, admin, "/admin/registrations?error=reason").Body.String()
		assert.Contains(t, body, "A reason is required")
	})

	t.Run("rejects with reason", func(t *testing.T) {
		rec := postAs(t, srv, admin, path, url.Values{"reason": {"Account too new; apply again in a month."}})
		assert.Equal(t, "/admin/registrations", rec.Header().Get("Location"))
		after, err := q.GetUserByID(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, gen.UserStatusRejected, after.Status)
		assert.Equal(t, "Account too new; apply again in a month.", *after.RejectionReason)
		assert.Equal(t, []string{"registration.reject"}, auditActions(t, tx, u.ID))
	})

	t.Run("applicant sees the reason", func(t *testing.T) {
		body := getAs(t, srv, sessionFor(t, srv, u), "/account").Body.String()
		assert.Contains(t, body, "not accepted")
		assert.Contains(t, body, "Account too new")
	})

	t.Run("rejected tab lists them and can approve", func(t *testing.T) {
		body := getAs(t, srv, admin, "/admin/registrations?tab=rejected").Body.String()
		assert.Contains(t, body, "Newbie")
		assert.Contains(t, body, "Rejected: Account too new")

		rec := postAs(t, srv, admin, "/admin/registrations/approve", url.Values{"ids": {u.ID.String()}, "tab": {"rejected"}})
		assert.Equal(t, "/admin/registrations?tab=rejected", rec.Header().Get("Location"))
		after, err := q.GetUserByID(ctx, u.ID)
		require.NoError(t, err)
		assert.Equal(t, gen.UserStatusApproved, after.Status)
		assert.Nil(t, after.RejectionReason, "cleared on approval")
	})

	t.Run("rejecting an approved user is stale", func(t *testing.T) {
		rec := postAs(t, srv, admin, path, url.Values{"reason": {"x"}})
		assert.Equal(t, "/admin/registrations?error=stale", rec.Header().Get("Location"))
	})
}
