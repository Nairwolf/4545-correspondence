//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// jobRunByRiverID reads back the job_runs row a job wrote, found by the
// river job id the test passed in (unique per test). recordedRun in
// sync_games_integration_test.go is the sync-games-shaped version of
// this; here the whole row matters, not the sync-games detail.
func jobRunByRiverID(t *testing.T, q *gen.Queries, riverJobID int64) gen.JobRun {
	t.Helper()
	runs, err := q.ListRecentJobRuns(context.Background(), 200)
	require.NoError(t, err)
	for _, run := range runs {
		if run.RiverJobID != nil && *run.RiverJobID == riverJobID {
			return run
		}
	}
	t.Fatalf("no job_runs row with river_job_id %d", riverJobID)
	return gen.JobRun{}
}

func TestFailInterruptedJobRuns_MarksLeftoverRunningRowsFailed(t *testing.T) {
	// The startup sweep in runServe: a row left at "running" by a process
	// that died would otherwise be reported as running by /health forever.
	tx := testTx(t)
	q := gen.New(tx)
	ctx := context.Background()

	stuckID := time.Now().UnixNano()
	stuck, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{
		JobName:    "sync-games",
		RiverJobID: &stuckID,
	})
	require.NoError(t, err)
	require.Equal(t, gen.JobRunStatusRunning, stuck.Status)

	doneID := stuckID + 1
	done, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{
		JobName:    "sync-games",
		RiverJobID: &doneID,
	})
	require.NoError(t, err)
	require.NoError(t, q.FinishJobRun(ctx, gen.FinishJobRunParams{
		ID:             done.ID,
		Status:         gen.JobRunStatusSucceeded,
		ItemsProcessed: 1,
		Detail:         []byte(`{}`),
	}))

	msg := "interrupted: the server restarted before this run finished"
	swept, err := q.FailInterruptedJobRuns(ctx, &msg)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, swept, int64(1))

	got := jobRunByRiverID(t, q, stuckID)
	assert.Equal(t, gen.JobRunStatusFailed, got.Status)
	require.NotNil(t, got.Error)
	assert.Equal(t, msg, *got.Error)
	assert.True(t, got.FinishedAt.Valid, "a swept run is finished, not open-ended")

	untouched := jobRunByRiverID(t, q, doneID)
	assert.Equal(t, gen.JobRunStatusSucceeded, untouched.Status)
	assert.Nil(t, untouched.Error)
}

func TestRunRecompute_SettingsLoadFailureIsRecorded(t *testing.T) {
	// Before the B-3 change this early return left the row at "running"
	// (the recompute half of REVIEW S-12).
	tx := testTx(t)
	q := gen.New(tx)
	ctx := context.Background()

	_, err := q.UpsertSetting(ctx, gen.UpsertSettingParams{
		Key:   "pairing.last_k",
		Value: []byte(`"five"`), // a string where an int is expected
	})
	require.NoError(t, err)

	riverJobID := time.Now().UnixNano()
	err = runRecompute(ctx, tx, &riverJobID)
	require.Error(t, err)

	got := jobRunByRiverID(t, q, riverJobID)
	assert.Equal(t, gen.JobRunStatusFailed, got.Status)
	require.NotNil(t, got.Error)
	assert.Contains(t, *got.Error, "load settings")
	assert.True(t, got.FinishedAt.Valid)
}
