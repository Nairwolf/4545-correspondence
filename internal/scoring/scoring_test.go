package scoring

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBaseRating(t *testing.T) {
	established := func(v int) *Rating { return &Rating{Value: v, Provisional: false} }
	provisional := func(v int) *Rating { return &Rating{Value: v, Provisional: true} }

	tests := []struct {
		name           string
		correspondence *Rating
		classical      *Rating
		wantValue      int
		wantUnrated    bool
	}{
		{
			name:           "established correspondence wins regardless of classical",
			correspondence: established(1800),
			classical:      established(2200), // higher, but must NOT win — corrects the sheet's MAX()
			wantValue:      1800,
		},
		{
			name:           "provisional correspondence wins over an established classical rating",
			correspondence: provisional(1200),
			classical:      established(1900),
			wantValue:      1200,
		},
		{
			name:           "provisional correspondence, no classical uses the provisional value anyway",
			correspondence: provisional(1200),
			classical:      nil,
			wantValue:      1200,
		},
		{
			name:           "both provisional prefers correspondence, still not unrated",
			correspondence: provisional(1200),
			classical:      provisional(1400),
			wantValue:      1200,
		},
		{
			name:           "classical only, established",
			correspondence: nil,
			classical:      established(2000),
			wantValue:      2000,
		},
		{
			name:           "classical only, provisional",
			correspondence: nil,
			classical:      provisional(1600),
			wantValue:      1600,
		},
		{
			name:           "neither rating exists means unrated",
			correspondence: nil,
			classical:      nil,
			wantValue:      0,
			wantUnrated:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, unrated := BaseRating(tt.correspondence, tt.classical)
			assert.Equal(t, tt.wantValue, value)
			assert.Equal(t, tt.wantUnrated, unrated)
		})
	}
}

func TestPerfDelta_FIDETableExactAtK5(t *testing.T) {
	// The spec's worked table: at k=5, every half-point score lands
	// exactly on a 10% step of the FIDE table.
	tests := []struct {
		score float64
		want  float64
	}{
		{0.0, -800},
		{0.5, -366},
		{1.0, -240},
		{1.5, -149},
		{2.0, -72},
		{2.5, 0},
		{3.0, 72},
		{3.5, 149},
		{4.0, 240},
		{4.5, 366},
		{5.0, 800},
	}
	for _, tt := range tests {
		got := PerfDelta(tt.score, 5)
		assert.InDelta(t, tt.want, got, 1e-9, "score=%v/5", tt.score)
	}
}

func TestPerfDelta_InterpolatesBetweenSteps(t *testing.T) {
	// 2.25/5 = 45%, halfway between the 40% step (-72) and the 50% step (0).
	got := PerfDelta(2.25, 5)
	assert.InDelta(t, -36, got, 1e-9)
}

func TestPerfDelta_AnyWindowSize(t *testing.T) {
	// k=3: percentage steps don't line up with the table's own points
	// except at a few exact fractions — check those.
	assert.InDelta(t, -800, PerfDelta(0, 3), 1e-9)
	assert.InDelta(t, 0, PerfDelta(1.5, 3), 1e-9) // 50%
	assert.InDelta(t, 800, PerfDelta(3, 3), 1e-9)

	// k=10: exact 10% steps.
	assert.InDelta(t, -240, PerfDelta(2, 10), 1e-9) // 20%
	assert.InDelta(t, 0, PerfDelta(5, 10), 1e-9)    // 50%
	assert.InDelta(t, 800, PerfDelta(10, 10), 1e-9) // 100%
}

func TestPerfDelta_ClampsOutOfRangeScores(t *testing.T) {
	// Should not happen for a well-formed score, but must not extrapolate
	// past the FIDE table's own ±800 cap if it does.
	assert.InDelta(t, -800, PerfDelta(-1, 5), 1e-9)
	assert.InDelta(t, 800, PerfDelta(6, 5), 1e-9)
}

func TestPerfDelta_PanicsOnNonPositiveWindow(t *testing.T) {
	assert.Panics(t, func() { PerfDelta(1, 0) })
	assert.Panics(t, func() { PerfDelta(1, -1) })
}

func game(daysAgo int, playedWhite bool, result Result, opponentRating int) FinishedGame {
	return FinishedGame{
		OpponentID:           "opponent",
		OpponentRatingAtGame: opponentRating,
		PlayedWhite:          playedWhite,
		Result:               result,
		FinishedAt:           time.Now().Add(-time.Duration(daysAgo) * 24 * time.Hour),
	}
}

func TestLastK_OrdersByFinishedAtDescendingAndCaps(t *testing.T) {
	games := []FinishedGame{
		game(10, true, Win, 1500),
		game(1, true, Win, 1500), // most recent
		game(5, true, Win, 1500),
	}
	window := LastK(games, 2)
	require.Len(t, window, 2)
	assert.Equal(t, 1, daysAgoOf(t, window[0]))
	assert.Equal(t, 5, daysAgoOf(t, window[1]))
}

func daysAgoOf(t *testing.T, g FinishedGame) int {
	t.Helper()
	return int(time.Since(g.FinishedAt).Hours() / 24)
}

func TestLastK_ShorterThanKForNewPlayers(t *testing.T) {
	games := []FinishedGame{game(1, true, Win, 1500), game(2, false, Draw, 1500)}
	assert.Len(t, LastK(games, 5), 2)
	assert.Len(t, LastK(nil, 5), 0)
	assert.Nil(t, LastK(games, 0))
}

func TestPerfRating_EmptyWindowIsNotOK(t *testing.T) {
	perf, ok := PerfRating(nil, 5)
	assert.False(t, ok)
	assert.Zero(t, perf)
}

func TestPerfRating_AveragesOpponentRatingAndAppliesDelta(t *testing.T) {
	games := []FinishedGame{
		game(1, true, Win, 1600),
		game(2, false, Win, 1400),
	}
	// score = 2/2 = 100% -> delta +800; avg opponent rating = 1500.
	perf, ok := PerfRating(games, 5)
	require.True(t, ok)
	assert.InDelta(t, 2300, perf, 1e-9)
}

func TestPowerRating(t *testing.T) {
	tests := []struct {
		name            string
		perf            float64
		perfOK          bool
		base            int
		gamesPlayed     int
		minGamesForPerf int
		want            int
	}{
		{
			name: "below min games falls back to base even with a valid perf rating",
			perf: 1900, perfOK: true, base: 1500, gamesPlayed: 4, minGamesForPerf: 5,
			want: 1500,
		},
		{
			name: "at min games uses the rounded performance rating",
			perf: 1900.4, perfOK: true, base: 1500, gamesPlayed: 5, minGamesForPerf: 5,
			want: 1900,
		},
		{
			name: "above min games rounds half up",
			perf: 1900.6, perfOK: true, base: 1500, gamesPlayed: 10, minGamesForPerf: 5,
			want: 1901,
		},
		{
			name: "no computable perf rating always falls back to base",
			perf: 0, perfOK: false, base: 1500, gamesPlayed: 10, minGamesForPerf: 5,
			want: 1500,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PowerRating(tt.perf, tt.perfOK, tt.base, tt.gamesPlayed, tt.minGamesForPerf)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestColorScore(t *testing.T) {
	games := []FinishedGame{
		game(1, true, Win, 1500),
		game(2, true, Loss, 1500),
		game(3, false, Draw, 1500),
	}
	// two whites, one black -> +1
	assert.Equal(t, 1, ColorScore(games))
	assert.Zero(t, ColorScore(nil))
}

func TestXP_DefaultWeights(t *testing.T) {
	games := []FinishedGame{
		game(1, true, Win, 1500),
		game(2, true, Draw, 1500),
		game(3, true, Loss, 1500),
	}
	// win(3) + draw(2) + loss(1) = 6, and a loss still earns XP.
	assert.Equal(t, 6, XP(games, DefaultXPWeights))
}

func TestXP_CustomWeights(t *testing.T) {
	games := []FinishedGame{game(1, true, Win, 1500)}
	got := XP(games, XPWeights{Win: 10, Draw: 0, Loss: 0})
	assert.Equal(t, 10, got)
}

func TestLevel_Boundaries(t *testing.T) {
	tests := []struct {
		xp         int
		wantLevel  int
		wantToNext int
	}{
		{0, 0, 1},
		{1, 1, 3},
		{3, 1, 1},
		{4, 2, 5},
		{8, 2, 1},
		{9, 3, 7},
		{15, 3, 1},
		{16, 4, 9},
	}
	for _, tt := range tests {
		level, toNext := Level(tt.xp)
		assert.Equal(t, tt.wantLevel, level, "xp=%d level", tt.xp)
		assert.Equal(t, tt.wantToNext, toNext, "xp=%d xpToNextLevel", tt.xp)
	}
}

func TestLevel_PerfectSquaresStayExact(t *testing.T) {
	// Regression guard for Level's integer-arithmetic correction: every
	// perfect square must report the exact level, not one below it.
	for n := 1; n <= 500; n++ {
		xp := n * n
		level, toNext := Level(xp)
		require.Equal(t, n, level, "xp=%d", xp)
		require.Equal(t, (n+1)*(n+1)-xp, toNext, "xp=%d", xp)
	}
}

func TestCapacityAllows(t *testing.T) {
	unlimited := (*int)(nil)
	cap4 := func() *int { v := 4; return &v }()

	// This is the case spec §11 calls out explicitly: nil must mean
	// unlimited, never zero, no matter how many games are ongoing.
	assert.True(t, CapacityAllows(unlimited, 999))

	assert.True(t, CapacityAllows(cap4, 3))
	assert.False(t, CapacityAllows(cap4, 4))
	assert.False(t, CapacityAllows(cap4, 7))
}
