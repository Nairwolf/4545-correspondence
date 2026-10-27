package activity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStreak(t *testing.T) {
	week := func(n int) time.Time {
		return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC).AddDate(0, 0, 7*n)
	}
	missed := []MissedStart{{At: week(1), Round: 201}, {At: week(2), Round: 202}, {At: week(3), Round: 203}}

	cases := []struct {
		name     string
		lastGame time.Time
		resumed  time.Time
		want     []int32 // rounds still in the run
	}{
		{"never played, never resumed: every miss counts", time.Time{}, time.Time{}, []int32{201, 202, 203}},
		{"a game started before them all", week(0), time.Time{}, []int32{201, 202, 203}},
		{"a game started between two misses ends the run", week(2).Add(-time.Hour), time.Time{}, []int32{202, 203}},
		{"a game started after the last miss", week(3).Add(time.Hour), time.Time{}, nil},
		{"resuming ends the run", time.Time{}, week(2).Add(time.Hour), []int32{203}},
		{"the later of the two counts: the resume", week(1).Add(time.Hour), week(2).Add(time.Hour), []int32{203}},
		{"the later of the two counts: the game", week(2).Add(time.Hour), week(1).Add(time.Hour), []int32{203}},
		{"a miss recorded at the very moment of the game is not after it", week(2), time.Time{}, []int32{203}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rounds []int32
			for _, m := range Streak(missed, tc.lastGame, tc.resumed) {
				rounds = append(rounds, m.Round)
			}
			assert.Equal(t, tc.want, rounds)
		})
	}

	assert.Empty(t, Streak(nil, time.Time{}, time.Time{}), "no missed start at all")
}

func TestDecide(t *testing.T) {
	cases := []struct {
		streak, pauseAfter int
		want               Action
	}{
		// The default, 2: warned at the first miss, paused at the second.
		{0, 2, None},
		{1, 2, Warn},
		{2, 2, Pause},
		// A threshold lowered below a streak already under way pauses at
		// the next miss rather than never.
		{3, 2, Pause},
		// 1: paused at the first miss, with nothing to warn at before.
		{0, 1, None},
		{1, 1, Pause},
		// 3: the first miss passes quietly, the second warns.
		{1, 3, None},
		{2, 3, Warn},
		{3, 3, Pause},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, Decide(tc.streak, tc.pauseAfter), "streak %d, pause after %d", tc.streak, tc.pauseAfter)
	}
}
