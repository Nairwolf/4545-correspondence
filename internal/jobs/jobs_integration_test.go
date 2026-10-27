//go:build integration

package jobs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

func TestScheduler_OneCreateGamesJobPerRoundAtATime(t *testing.T) {
	// Spec §6.3: create-games is unique per round while it waits or
	// runs, whoever queued it, and has the longer retry budget. Once it
	// has finished, an admin's retry can queue another.
	pool, tx := testPoolAndTx(t)
	ctx := context.Background()

	client, err := NewInsertClient(pool)
	require.NoError(t, err)
	sched := NewScheduler(client)

	const roundID = 900043
	queued, err := sched.ScheduleCreateGames(ctx, tx, roundID, false)
	require.NoError(t, err)
	assert.True(t, queued)

	queued, err = sched.ScheduleCreateGames(ctx, tx, roundID, true)
	require.NoError(t, err)
	assert.False(t, queued, "a retry while the publication's job waits adds nothing")

	var count, maxAttempts int
	require.NoError(t, tx.QueryRow(ctx, `
		SELECT count(*), max(max_attempts) FROM river_job
		WHERE kind = 'create-games' AND (args->>'round_id')::int = $1`, roundID).Scan(&count, &maxAttempts))
	assert.Equal(t, 1, count)
	assert.Equal(t, 10, maxAttempts)

	_, err = tx.Exec(ctx, `
		UPDATE river_job SET state = 'completed', finalized_at = now()
		WHERE kind = 'create-games' AND (args->>'round_id')::int = $1`, roundID)
	require.NoError(t, err)
	queued, err = sched.ScheduleCreateGames(ctx, tx, roundID, true)
	require.NoError(t, err)
	assert.True(t, queued, "once it has finished, a retry is queued")

	var retry bool
	require.NoError(t, tx.QueryRow(ctx, `
		SELECT (args->>'retry')::bool FROM river_job
		WHERE kind = 'create-games' AND (args->>'round_id')::int = $1 AND state = 'available'`, roundID).Scan(&retry))
	assert.True(t, retry)
}

func TestScheduler_QueuesASyncGamesRun(t *testing.T) {
	pool, tx := testPoolAndTx(t)
	ctx := context.Background()

	client, err := NewInsertClient(pool)
	require.NoError(t, err)

	var before, after int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'sync-games'`).Scan(&before))
	require.NoError(t, NewScheduler(client).ScheduleSyncGames(ctx, tx))
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM river_job WHERE kind = 'sync-games'`).Scan(&after))
	assert.Equal(t, before+1, after)
}

func TestScheduler_WithdrawsAPairingsChallengeOnce(t *testing.T) {
	// Spec §6.4: a pairing's challenge is withdrawn once. Asking again —
	// a second evaluation, an admin's Mark failed after it — adds
	// nothing, even after the first job has finished.
	pool, tx := testPoolAndTx(t)
	ctx := context.Background()

	client, err := NewInsertClient(pool)
	require.NoError(t, err)
	sched := NewScheduler(client)
	var pairing pgtype.UUID
	require.NoError(t, pairing.Scan("6f1c1c6e-0000-4000-8000-000000000042"))

	require.NoError(t, sched.ScheduleCancelChallenge(ctx, tx, pairing))
	_, err = tx.Exec(ctx, `
		UPDATE river_job SET state = 'completed', finalized_at = now()
		WHERE kind = 'cancel-challenge' AND args->>'pairing_id' = $1`, pairing.String())
	require.NoError(t, err)
	require.NoError(t, sched.ScheduleCancelChallenge(ctx, tx, pairing))

	var count int
	require.NoError(t, tx.QueryRow(ctx, `
		SELECT count(*) FROM river_job
		WHERE kind = 'cancel-challenge' AND args->>'pairing_id' = $1`, pairing.String()).Scan(&count))
	assert.Equal(t, 1, count)
}
