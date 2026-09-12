package web

import (
	"math/big"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

func i32(v int32) *int32 { return &v }

func tenths(v int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(v), Exp: -1, Valid: true}
}

// feed is three players: a leader, a mid-tabler, and one who has never
// had a standing computed (all metric pointers nil) — in the order
// GetStandings returns them (power rating desc, then username).
func feed() []gen.GetStandingsRow {
	return []gen.GetStandingsRow{
		{
			LichessUsername: "Alice", IsActive: true,
			Rating: i32(2100), GamesPlayed: i32(10), Wins: i32(6), Draws: i32(2), Losses: i32(2),
			Ongoing: i32(1), LastKScore: tenths(35), LastKPerfRating: i32(2150), PowerRating: i32(2140),
			Xp: i32(26), Level: i32(5),
		},
		{
			LichessUsername: "bob", IsActive: false,
			Rating: i32(1800), GamesPlayed: i32(8), Wins: i32(3), Draws: i32(1), Losses: i32(4),
			Ongoing: i32(0), LastKScore: tenths(20), LastKPerfRating: i32(1790), PowerRating: i32(1795),
			Xp: i32(14), Level: i32(3),
		},
		{
			LichessUsername: "Zoe", IsActive: true, // no standing row yet
		},
	}
}

func names(rows []gen.GetStandingsRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.LichessUsername
	}
	return out
}

func TestArrangeStandings(t *testing.T) {
	tests := []struct {
		name string
		p    standingsParams
		want []string
	}{
		{"default order is preserved", standingsParams{}, []string{"Alice", "bob", "Zoe"}},
		{"active only", standingsParams{Active: "1"}, []string{"Alice", "Zoe"}},
		{"inactive only", standingsParams{Active: "0"}, []string{"bob"}},
		{"search is case-insensitive substring", standingsParams{Q: "OB"}, []string{"bob"}},
		{"sort by player ascending", standingsParams{Sort: "player", Dir: "asc"}, []string{"Alice", "bob", "Zoe"}},
		{"sort by rating descending", standingsParams{Sort: "rating", Dir: "desc"}, []string{"Alice", "bob", "Zoe"}},
		{"sort by rating ascending puts the unrated player first", standingsParams{Sort: "rating", Dir: "asc"}, []string{"Zoe", "bob", "Alice"}},
		{"sort by last-5 score ascending", standingsParams{Sort: "last5", Dir: "asc"}, []string{"Zoe", "bob", "Alice"}},
		{"sort by wins descending", standingsParams{Sort: "wins", Dir: "desc"}, []string{"Alice", "bob", "Zoe"}},
		{"filter then sort", standingsParams{Active: "1", Sort: "player", Dir: "desc"}, []string{"Zoe", "Alice"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := arrangeStandings(feed(), tc.p)
			assert.Equal(t, tc.want, names(got))
		})
	}
}

func TestArrangeStandingsDoesNotMutateInput(t *testing.T) {
	in := feed()
	_ = arrangeStandings(in, standingsParams{Sort: "rating", Dir: "asc"})
	assert.Equal(t, []string{"Alice", "bob", "Zoe"}, names(in), "input slice order must be untouched")
}

func TestLineChart(t *testing.T) {
	t.Run("fewer than two points yields no chart", func(t *testing.T) {
		assert.Empty(t, lineChart(nil).Points)
		assert.Empty(t, lineChart([]float64{5}).Points)
	})

	t.Run("scales into the viewbox with y inverted", func(t *testing.T) {
		c := lineChart([]float64{1500, 1600, 1550})
		require.NotEmpty(t, c.Points)
		assert.Equal(t, 1500.0, c.Min)
		assert.Equal(t, 1600.0, c.Max)
		assert.Equal(t, 1550.0, c.Last)
		// first point: x=0, y=h (min value sits at the bottom);
		// second point: x=600, y=0 (max value sits at the top).
		assert.Equal(t, "0.0,160.0 300.0,0.0 600.0,80.0", c.Points)
	})

	t.Run("a flat series does not divide by zero", func(t *testing.T) {
		c := lineChart([]float64{7, 7, 7})
		assert.Equal(t, "0.0,160.0 300.0,160.0 600.0,160.0", c.Points)
	})
}
