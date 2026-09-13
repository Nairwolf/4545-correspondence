package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

func TestSyncGamesOutcome(t *testing.T) {
	dbErr := errors.New("upsert game: numeric field overflow")

	tests := []struct {
		name       string
		stats      syncGamesStats
		fatal      error
		wantStatus gen.JobRunStatus
		wantErr    []string // substrings the error text must contain; nil = no error text
	}{
		{
			name:       "nothing to do is a success, not a failure",
			wantStatus: gen.JobRunStatusSucceeded,
		},
		{
			name:       "every call succeeded",
			stats:      syncGamesStats{LichessCallsOK: 3},
			wantStatus: gen.JobRunStatusSucceeded,
		},
		{
			name: "some Lichess calls failed: succeeded, but the failures are recorded",
			stats: syncGamesStats{
				LichessCallsOK:     2,
				LichessCallsFailed: 1,
				LichessErrors:      []string{"fetch games for renamed: lichess: 404: Not found"},
			},
			wantStatus: gen.JobRunStatusSucceeded,
			wantErr:    []string{"1 of 3 Lichess calls failed", "renamed"},
		},
		{
			name: "every Lichess call failed: Lichess is down, the run failed",
			stats: syncGamesStats{
				LichessCallsFailed: 2,
				LichessErrors:      []string{"fetch games by id: boom", "fetch games for a: boom"},
			},
			wantStatus: gen.JobRunStatusFailed,
			wantErr:    []string{"2 of 2 Lichess calls failed"},
		},
		{
			// The original B-2 bug: a database error after one successful
			// Lichess call was recorded as a clean success.
			name:       "a database error fails the run even after successful Lichess calls",
			stats:      syncGamesStats{LichessCallsOK: 1},
			fatal:      dbErr,
			wantStatus: gen.JobRunStatusFailed,
			wantErr:    []string{"numeric field overflow"},
		},
		{
			name: "a database error and Lichess failures are both reported",
			stats: syncGamesStats{
				LichessCallsOK:     1,
				LichessCallsFailed: 1,
				LichessErrors:      []string{"fetch games for a: timeout"},
			},
			fatal:      dbErr,
			wantStatus: gen.JobRunStatusFailed,
			wantErr:    []string{"numeric field overflow", "1 of 2 Lichess calls failed"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, errMsg := syncGamesOutcome(tt.stats, tt.fatal)
			assert.Equal(t, tt.wantStatus, status)
			if tt.wantErr == nil {
				assert.Nil(t, errMsg)
				return
			}
			require.NotNil(t, errMsg)
			for _, want := range tt.wantErr {
				assert.Contains(t, *errMsg, want)
			}
		})
	}
}

func TestLichessFailed_CapsRecordedMessagesButCountsEveryFailure(t *testing.T) {
	var stats syncGamesStats
	for i := 0; i < maxRecordedLichessErrors+5; i++ {
		stats.lichessFailed(fmt.Sprintf("fetch games for p%d", i), errors.New("down"))
	}

	assert.Equal(t, maxRecordedLichessErrors+5, stats.LichessCallsFailed)
	assert.Len(t, stats.LichessErrors, maxRecordedLichessErrors)

	_, errMsg := syncGamesOutcome(stats, nil)
	require.NotNil(t, errMsg)
	assert.Contains(t, *errMsg, "15 of 15 Lichess calls failed")
	assert.Contains(t, *errMsg, "…", "truncation is visible")
}
