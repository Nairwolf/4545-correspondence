//go:build integration

package jobs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPoolAndTx returns a pool on TEST_DATABASE_URL and a transaction
// on it that is rolled back when the test ends.
func testPoolAndTx(t *testing.T) (*pgxpool.Pool, pgx.Tx) {
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
	return pool, tx
}

type queuedPublish struct {
	state       string
	scheduledAt time.Time
}

func queuedPublishes(t *testing.T, tx pgx.Tx, roundID int32) []queuedPublish {
	t.Helper()
	rows, err := tx.Query(context.Background(), `
		SELECT state::text, scheduled_at FROM river_job
		WHERE kind = 'publish-round' AND (args->>'round_id')::int = $1
		ORDER BY scheduled_at`, roundID)
	require.NoError(t, err)
	defer rows.Close()
	var out []queuedPublish
	for rows.Next() {
		var q queuedPublish
		require.NoError(t, rows.Scan(&q.state, &q.scheduledAt))
		out = append(out, q)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestInsertClient_QueuesOnePublishJobPerWindow(t *testing.T) {
	// The one-shot `ic generate-round` has no job runner: its draft's
	// publish-round job goes into river's table for serve to work when
	// the window ends (spec §8.5). The job is committed with the draft,
	// in the caller's transaction, and unique per round and window.
	pool, tx := testPoolAndTx(t)
	ctx := context.Background()

	client, err := NewInsertClient(pool)
	require.NoError(t, err)
	sched := NewScheduler(client)

	const roundID = 900042 // far from any id a sequence hands out
	window := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)

	require.NoError(t, sched.SchedulePublish(ctx, tx, roundID, window))
	require.NoError(t, sched.SchedulePublish(ctx, tx, roundID, window), "a regenerated draft enqueues again")
	queued := queuedPublishes(t, tx, roundID)
	require.Len(t, queued, 1, "the same round and window is one job")
	assert.Equal(t, "scheduled", queued[0].state)
	assert.True(t, window.Equal(queued[0].scheduledAt), "runs when the window ends: %s", queued[0].scheduledAt)

	later := window.Add(time.Hour)
	require.NoError(t, sched.SchedulePublish(ctx, tx, roundID, later))
	assert.Len(t, queuedPublishes(t, tx, roundID), 2, "a moved window is a new job")
}
