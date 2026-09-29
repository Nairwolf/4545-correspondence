//go:build integration

package notify_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/notify"
)

// testQueries runs against TEST_DATABASE_URL inside a transaction that
// is rolled back when the test ends.
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

func addUser(t *testing.T, q *gen.Queries, name string) pgtype.UUID {
	t.Helper()
	u, err := q.CreateApprovedUser(context.Background(), gen.CreateApprovedUserParams{
		LichessUsername: name,
		LichessUserID:   name,
	})
	require.NoError(t, err)
	return u.ID
}

func notices(t *testing.T, q *gen.Queries, user pgtype.UUID) []gen.Notification {
	t.Helper()
	list, err := q.ListNotificationsForUser(context.Background(), gen.ListNotificationsForUserParams{
		UserID: user,
		Limit:  100,
	})
	require.NoError(t, err)
	return list
}

func mute(t *testing.T, q *gen.Queries, user pgtype.UUID, c notify.Category) {
	t.Helper()
	require.NoError(t, q.MuteNotificationCategory(context.Background(), gen.MuteNotificationCategoryParams{
		UserID:   user,
		Category: string(c),
	}))
}

func TestSend_StoresTheNotice(t *testing.T) {
	q := testQueries(t)
	user := addUser(t, q, "notifystore")

	require.NoError(t, notify.Send(context.Background(), q, notify.Notice{
		User:     user,
		Category: notify.LevelUp,
		Title:    "You reached level 2",
		Body:     "Well played.",
		Link:     "/account",
		Key:      "level_up:2",
	}))

	list := notices(t, q, user)
	require.Len(t, list, 1)
	n := list[0]
	assert.Equal(t, "level_up", n.Category)
	assert.Equal(t, "You reached level 2", n.Title)
	assert.Equal(t, "Well played.", n.Body)
	require.NotNil(t, n.LinkUrl)
	assert.Equal(t, "/account", *n.LinkUrl)
	assert.False(t, n.ReadAt.Valid, "a new notice is unread")

	unread, err := q.CountUnreadNotifications(context.Background(), user)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unread)
}

func TestSend_DedupeKeyIsPerPlayer(t *testing.T) {
	// Spec §10: a retried job never notifies twice. Both players of a
	// pairing are told about the same event under the same key, so the
	// key must be unique per player, not across the table.
	q := testQueries(t)
	ctx := context.Background()
	white := addUser(t, q, "notifydedupewhite")
	black := addUser(t, q, "notifydedupeblack")

	for range 2 {
		for _, user := range []pgtype.UUID{white, black} {
			require.NoError(t, notify.Send(ctx, q, notify.Notice{
				User: user, Category: notify.Round, Title: "Round 7", Body: "Paired.", Key: "round:7:paired:p1",
			}))
		}
	}
	assert.Len(t, notices(t, q, white), 1)
	assert.Len(t, notices(t, q, black), 1)
}

func TestSend_NoKeyMeansNoDeduplication(t *testing.T) {
	q := testQueries(t)
	user := addUser(t, q, "notifynokey")
	for range 2 {
		require.NoError(t, notify.Send(context.Background(), q, notify.Notice{
			User: user, Category: notify.Round, Title: "t", Body: "b",
		}))
	}
	list := notices(t, q, user)
	assert.Len(t, list, 2)
	assert.Nil(t, list[0].DedupeKey)
	assert.Nil(t, list[0].LinkUrl)
}

func TestSend_RespectsAnOptOut(t *testing.T) {
	q := testQueries(t)
	user := addUser(t, q, "notifymuted")
	mute(t, q, user, notify.LevelUp)

	require.NoError(t, notify.Send(context.Background(), q, notify.Notice{
		User: user, Category: notify.LevelUp, Title: "t", Body: "b",
	}))
	require.NoError(t, notify.Send(context.Background(), q, notify.Notice{
		User: user, Category: notify.Round, Title: "t", Body: "b",
	}))
	list := notices(t, q, user)
	require.Len(t, list, 1, "only the category still turned on is sent")
	assert.Equal(t, "round", list[0].Category)
}

func TestSend_CriticalCategoriesIgnoreAnOptOut(t *testing.T) {
	// No form can store an opt-out for these, but a row put there by
	// hand must still not silence them (spec §10).
	q := testQueries(t)
	user := addUser(t, q, "notifycritical")
	mute(t, q, user, notify.Registration)

	require.NoError(t, notify.Send(context.Background(), q, notify.Notice{
		User: user, Category: notify.Registration, Title: "t", Body: "b",
	}))
	assert.Len(t, notices(t, q, user), 1)
}

func TestSend_RefusesAnUnknownCategory(t *testing.T) {
	q := testQueries(t)
	user := addUser(t, q, "notifyunknown")

	err := notify.Send(context.Background(), q, notify.Notice{
		User: user, Category: "discord", Title: "t", Body: "b",
	})
	assert.ErrorContains(t, err, `unknown category "discord"`)
	assert.Empty(t, notices(t, q, user))
}

func TestMarkRead_OnlyTheOwnersNotices(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()
	owner := addUser(t, q, "notifyowner")
	other := addUser(t, q, "notifyother")
	for range 2 {
		require.NoError(t, notify.Send(ctx, q, notify.Notice{User: owner, Category: notify.Round, Title: "t", Body: "b"}))
	}
	first := notices(t, q, owner)[0]

	n, err := q.MarkNotificationRead(ctx, gen.MarkNotificationReadParams{ID: first.ID, UserID: other})
	require.NoError(t, err)
	assert.Zero(t, n, "someone else's notice is left alone")

	n, err = q.MarkNotificationRead(ctx, gen.MarkNotificationReadParams{ID: first.ID, UserID: owner})
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	unread, err := q.CountUnreadNotifications(ctx, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(1), unread)

	n, err = q.MarkAllNotificationsRead(ctx, owner)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "only the one still unread")
}
