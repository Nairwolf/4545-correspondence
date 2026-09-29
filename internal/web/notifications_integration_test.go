//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/notify"
)

func send(t *testing.T, q *gen.Queries, u gen.User, c notify.Category, title string) {
	t.Helper()
	require.NoError(t, notify.Send(context.Background(), q, notify.Notice{
		User: u.ID, Category: c, Title: title, Body: title + " body", Link: "/account",
	}))
}

func noticesFor(t *testing.T, q *gen.Queries, u gen.User) []gen.Notification {
	t.Helper()
	list, err := q.ListNotificationsForUser(context.Background(), gen.ListNotificationsForUserParams{
		UserID: u.ID,
		Limit:  100,
	})
	require.NoError(t, err)
	return list
}

func unreadOf(t *testing.T, q *gen.Queries, u gen.User) int64 {
	t.Helper()
	n, err := q.CountUnreadNotifications(context.Background(), u.ID)
	require.NoError(t, err)
	return n
}

func TestNotifications_RequireSignIn(t *testing.T) {
	srv, _, _ := testServer(t)
	rec := getAs(t, srv, nil, "/account/notifications")
	assert.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "/login", rec.Header().Get("Location"))
}

func TestNotifications_BellListAndMarkRead(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Reader", true, 1800, 0, 1800, 1800, "0")
	session := sessionFor(t, srv, u)

	body := getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, `aria-label="Notifications"`, "no count while there is nothing unread")

	send(t, q, u, notify.Round, "Round 3: you play Rival with white")
	send(t, q, u, notify.LevelUp, "You reached level 2")

	body = getAs(t, srv, session, "/standings").Body.String()
	assert.Contains(t, body, `aria-label="Notifications, 2 unread"`, "the bell is on every page")

	rec := getAs(t, srv, session, "/account/notifications")
	require.Equal(t, http.StatusOK, rec.Code)
	body = rec.Body.String()
	assert.Contains(t, body, "Round 3: you play Rival with white")
	assert.Contains(t, body, "You reached level 2")
	assert.Less(t, strings.Index(body, "You reached level 2"), strings.Index(body, "Round 3"), "newest first")
	assert.Contains(t, body, "Mark all as read")
	assert.Contains(t, body, `href="/account"`)

	list := noticesFor(t, q, u)
	rec = postAs(t, srv, session, "/account/notifications/"+strconv.FormatInt(list[0].ID, 10)+"/read", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/account/notifications", rec.Header().Get("Location"))
	assert.Equal(t, int64(1), unreadOf(t, q, u))

	rec = postAs(t, srv, session, "/account/notifications/read", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Zero(t, unreadOf(t, q, u))
	body = getAs(t, srv, session, "/account/notifications").Body.String()
	assert.NotContains(t, body, "Mark all as read")
	assert.Contains(t, body, "You reached level 2", "read notices stay listed")
}

func TestNotifications_OnlyYourOwn(t *testing.T) {
	srv, q, tx := testServer(t)
	owner := addPlayer(t, q, tx, "Owner", true, 1800, 0, 1800, 1800, "0")
	other := addPlayer(t, q, tx, "Other", true, 1800, 0, 1800, 1800, "0")
	send(t, q, owner, notify.Round, "Owner's notice")
	id := strconv.FormatInt(noticesFor(t, q, owner)[0].ID, 10)

	session := sessionFor(t, srv, other)
	assert.NotContains(t, getAs(t, srv, session, "/account/notifications").Body.String(), "Owner's notice")
	postAs(t, srv, session, "/account/notifications/"+id+"/read", url.Values{})
	postAs(t, srv, session, "/account/notifications/read", url.Values{})
	assert.Equal(t, int64(1), unreadOf(t, q, owner))

	rec := postAs(t, srv, session, "/account/notifications/not-a-number/read", url.Values{})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestNotifications_Preferences(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Picky", true, 1800, 0, 1800, 1800, "0")
	session := sessionFor(t, srv, u)

	body := getAs(t, srv, session, "/account").Body.String()
	for _, o := range notify.OptOuts {
		assert.Contains(t, body, `name="send" value="`+string(o.Category)+`" class="mt-1" checked`, o.Category)
	}
	assert.NotContains(t, body, `value="token"`, "account-critical categories have no box")

	// Keep only round notices.
	rec := postAs(t, srv, session, "/account/notification-preferences", url.Values{"send": {"round"}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/account?saved=notifications", rec.Header().Get("Location"))

	muted, err := q.ListMutedNotificationCategories(context.Background(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"challenge", "level_up", "missed_start", "unstarted"}, muted)
	assert.Equal(t, []string{"profile.notifications"}, auditActions(t, tx, u.ID))

	body = getAs(t, srv, session, "/account?saved=notifications").Body.String()
	assert.Contains(t, body, "Notification preferences updated.")
	assert.Contains(t, body, `value="round" class="mt-1" checked`)
	assert.Contains(t, body, `value="level_up" class="mt-1" >`)

	send(t, q, u, notify.LevelUp, "muted")
	send(t, q, u, notify.Round, "kept")
	list := noticesFor(t, q, u)
	require.Len(t, list, 1)
	assert.Equal(t, "kept", list[0].Title)

	// Saving the same choice again changes and audits nothing.
	postAs(t, srv, session, "/account/notification-preferences", url.Values{"send": {"round"}})
	assert.Len(t, auditActions(t, tx, u.ID), 1)

	// Ticking everything back removes every opt-out row.
	all := url.Values{}
	for _, o := range notify.OptOuts {
		all.Add("send", string(o.Category))
	}
	postAs(t, srv, session, "/account/notification-preferences", all)
	muted, err = q.ListMutedNotificationCategories(context.Background(), u.ID)
	require.NoError(t, err)
	assert.Empty(t, muted)
}

func TestNotifications_RejectedApplicantHasNoPreferences(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Turned", true, 1800, 0, 1800, 1800, "0")
	setStatus(t, tx, u, gen.UserStatusRejected)
	session := sessionFor(t, srv, u)

	assert.NotContains(t, getAs(t, srv, session, "/account").Body.String(), "notification-preferences")
	rec := postAs(t, srv, session, "/account/notification-preferences", url.Values{})
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// They can still read what they were sent: the rejection itself.
	send(t, q, u, notify.Registration, "Your application was not accepted")
	body := getAs(t, srv, session, "/account/notifications").Body.String()
	assert.Contains(t, body, "Your application was not accepted")
	assert.NotContains(t, body, "Choose what you're told about")
}

func setStatus(t *testing.T, tx pgx.Tx, u gen.User, status gen.UserStatus) {
	t.Helper()
	_, err := tx.Exec(context.Background(), "UPDATE users SET status = $2 WHERE id = $1", u.ID, status)
	require.NoError(t, err)
}
