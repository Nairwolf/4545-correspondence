package pairing

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// defaultConfig is the §4.2 default settings for round 10, so a
// fixture's history can reach back into rounds 5–9 and, just outside
// the window, round 4.
func defaultConfig() Config {
	return Config{
		RoundNumber:       10,
		AvoidRecentRounds: 5,
		ColorWeight:       100,
		RepeatPenalty:     1_000_000,
		OddPool:           DoubleThenBye,
	}
}

func ptr[T any](v T) *T { return &v }

// active is a player with the league's defaults: unlimited capacity, no
// double games, no history.
func active(id string, rating int) Player {
	return Player{ID: id, Status: StatusActive, PowerRating: rating}
}

// assertPostConditions checks the §6.2 invariants that must hold for
// every result the engine ever produces, whatever the fixture.
func assertPostConditions(t *testing.T, players []Player, res Result) {
	t.Helper()

	excluded := map[string]int{}
	for _, e := range res.Exclusions {
		excluded[e.ID]++
		assert.Equal(t, 1, excluded[e.ID], "%s excluded more than once", e.ID)
	}

	appearances := map[string]int{}
	seenPair := map[[2]string]bool{}
	for _, p := range res.Pairings {
		assert.NotEqual(t, p.White, p.Black, "a player was paired with themselves")
		appearances[p.White]++
		appearances[p.Black]++
		assert.NotContains(t, excluded, p.White, "an excluded player was paired")
		assert.NotContains(t, excluded, p.Black, "an excluded player was paired")

		key := historyKey(p.White, p.Black)
		assert.False(t, seenPair[key], "%s and %s were paired twice", p.White, p.Black)
		seenPair[key] = true
	}

	for _, pl := range players {
		want := 1
		switch {
		case excluded[pl.ID] > 0:
			want = 0
		case res.DoubleGame != nil && *res.DoubleGame == pl.ID:
			want = 2
		}
		assert.Equal(t, want, appearances[pl.ID], "appearances of %s", pl.ID)
	}

	if res.DoubleGame != nil {
		var whites int
		for _, p := range res.Pairings {
			if p.White == *res.DoubleGame {
				whites++
			}
		}
		assert.Equal(t, 1, whites, "the double-game volunteer must have one white and one black")
	}
	assert.False(t, res.Bye != nil && res.DoubleGame != nil, "a round is a double game or a bye, never both")

	byes := 0
	if res.Bye != nil {
		byes = 1
	}
	// appearances is keyed by player, so the volunteer counts once.
	assert.Equal(t, res.PoolSize, len(appearances)+byes, "pool size must be the paired players plus the bye")
}

func generate(t *testing.T, players []Player, history History, cfg Config) Result {
	t.Helper()
	if history == nil {
		history = History{}
	}
	res := Generate(players, history, cfg, Greedy{})
	assertPostConditions(t, players, res)
	return res
}

func TestBuildPool(t *testing.T) {
	tests := []struct {
		name           string
		player         Player
		wantInPool     bool
		wantReason     Reason
		wantInFlight   int
		wantCapReading int
	}{
		{
			name:       "active player with the default unlimited cap",
			player:     Player{ID: "a", Status: StatusActive},
			wantInPool: true,
		},
		{
			name:       "unlimited cap with forty games in flight is still paired",
			player:     Player{ID: "a", Status: StatusActive, InFlight: 40},
			wantInPool: true,
		},
		{
			name:       "under the cap",
			player:     Player{ID: "a", Status: StatusActive, InFlight: 3, MaxConcurrent: ptr(4)},
			wantInPool: true,
		},
		{
			name:           "at the cap",
			player:         Player{ID: "a", Status: StatusActive, InFlight: 4, MaxConcurrent: ptr(4)},
			wantReason:     ReasonAtCapacity,
			wantInFlight:   4,
			wantCapReading: 4,
		},
		{
			name:           "over the cap, after lowering it",
			player:         Player{ID: "a", Status: StatusActive, InFlight: 9, MaxConcurrent: ptr(2)},
			wantReason:     ReasonAtCapacity,
			wantInFlight:   9,
			wantCapReading: 2,
		},
		{
			name:       "inactive",
			player:     Player{ID: "a", Status: StatusInactive},
			wantReason: ReasonInactive,
		},
		{
			name:       "paused by an admin",
			player:     Player{ID: "a", Status: StatusPaused},
			wantReason: ReasonPaused,
		},
		{
			name:       "auto-paused",
			player:     Player{ID: "a", Status: StatusAutoPaused},
			wantReason: ReasonAutoPaused,
		},
		{
			name:       "a cap of zero excludes, and is not the same as no cap",
			player:     Player{ID: "a", Status: StatusActive, MaxConcurrent: ptr(0)},
			wantReason: ReasonAtCapacity,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pool, exclusions := buildPool([]Player{tc.player})

			if tc.wantInPool {
				require.Len(t, pool, 1)
				assert.Empty(t, exclusions)
				return
			}
			assert.Empty(t, pool)
			require.Len(t, exclusions, 1)
			assert.Equal(t, tc.wantReason, exclusions[0].Reason)
			assert.Equal(t, tc.wantInFlight, exclusions[0].InFlight)
			assert.Equal(t, tc.wantCapReading, exclusions[0].Cap)
		})
	}
}

func TestColourPenalty(t *testing.T) {
	tests := []struct {
		name        string
		a, b        Player
		wantPenalty int
		wantAWhite  bool
	}{
		{
			// §6.2 worked example 1: both assignments leave 3.
			name:        "tie is broken by rating, the lower-rated player takes white",
			a:           Player{ID: "a", PowerRating: 2000, ColorScore: 2},
			b:           Player{ID: "b", PowerRating: 1800, ColorScore: 1},
			wantPenalty: 3,
			wantAWhite:  false,
		},
		{
			// §6.2 worked example 2: giving each their needed colour
			// fixes both at once.
			name:        "oppositely imbalanced players, the one due black takes black",
			a:           Player{ID: "a", PowerRating: 2000, ColorScore: 2},
			b:           Player{ID: "b", PowerRating: 1800, ColorScore: -2},
			wantPenalty: 2,
			wantAWhite:  false,
		},
		{
			name:        "the player due white takes white",
			a:           Player{ID: "a", PowerRating: 2000, ColorScore: -2},
			b:           Player{ID: "b", PowerRating: 1800, ColorScore: 2},
			wantPenalty: 2,
			wantAWhite:  true,
		},
		{
			name:        "equal scores and equal ratings fall through to the id",
			a:           Player{ID: "a", PowerRating: 1800},
			b:           Player{ID: "b", PowerRating: 1800},
			wantPenalty: 2,
			wantAWhite:  true,
		},
		{
			name:        "balanced players leave one unit either way",
			a:           Player{ID: "b", PowerRating: 1900},
			b:           Player{ID: "a", PowerRating: 1800},
			wantPenalty: 2,
			wantAWhite:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			penalty, aWhite := colourPenalty(tc.a, tc.b)
			assert.Equal(t, tc.wantPenalty, penalty)
			assert.Equal(t, tc.wantAWhite, aWhite)

			// The argmin and the penalty must agree: step 5 assigns the
			// colours step 3 priced.
			white, black := assignColours(tc.a, tc.b)
			assert.Equal(t, penalty, leftoverImbalance(white, black))
		})
	}
}

func TestGenerate_AssignsTheColoursPlayersAreDue(t *testing.T) {
	players := []Player{
		{ID: "a", Status: StatusActive, PowerRating: 2000, ColorScore: 3},
		{ID: "b", Status: StatusActive, PowerRating: 1990, ColorScore: -3},
	}

	res := generate(t, players, nil, defaultConfig())

	require.Len(t, res.Pairings, 1)
	assert.Equal(t, "b", res.Pairings[0].White, "the player owed white takes white")
	assert.Equal(t, "a", res.Pairings[0].Black)
	assert.Equal(t, 4, res.Pairings[0].ColorPenalty)
	assert.Equal(t, 10, res.Pairings[0].RatingGap)
}

func TestGenerate_RepeatAvoidance(t *testing.T) {
	// a and b are the closest pair by rating by far, so only the repeat
	// penalty can separate them.
	players := []Player{
		active("a", 2000),
		active("b", 1995),
		active("c", 1700),
		active("d", 1695),
	}

	t.Run("inside the window they are paired elsewhere", func(t *testing.T) {
		history := History{}
		history.Record("a", "b", 9)

		res := generate(t, players, history, defaultConfig())

		assert.Zero(t, res.RepeatPairings)
		for _, p := range res.Pairings {
			assert.NotEqual(t, historyKey("a", "b"), historyKey(p.White, p.Black))
		}
	})

	t.Run("just outside the window the rating gap wins again", func(t *testing.T) {
		history := History{}
		history.Record("a", "b", 4) // round 10 − 5 = 5 is the oldest round that counts

		res := generate(t, players, history, defaultConfig())

		assert.Zero(t, res.RepeatPairings)
		assert.Contains(t, pairKeys(res), historyKey("a", "b"))
	})

	t.Run("a window of zero avoids nothing", func(t *testing.T) {
		cfg := defaultConfig()
		cfg.AvoidRecentRounds = 0
		history := History{}
		history.Record("a", "b", 9)

		res := generate(t, players, history, cfg)

		assert.Zero(t, res.RepeatPairings)
		assert.Contains(t, pairKeys(res), historyKey("a", "b"))
	})
}

func TestGenerate_RecordsARepeatItCannotAvoid(t *testing.T) {
	// Two players who just met are still paired: the penalty is large
	// but finite, so the round happens and the admin is told why.
	players := []Player{active("a", 2000), active("b", 1900)}
	history := History{}
	history.Record("a", "b", 9)

	res := generate(t, players, history, defaultConfig())

	require.Len(t, res.Pairings, 1)
	require.NotNil(t, res.Pairings[0].RepeatOfRound)
	assert.Equal(t, 9, *res.Pairings[0].RepeatOfRound)
	assert.Equal(t, 1, res.RepeatPairings)
}

func TestGenerate_UnlimitedCapIsPairedEveryRound(t *testing.T) {
	// The NULL-cap regression test §11 requires: a player with no cap
	// and a large backlog must be paired every single round, and must
	// never appear in an exclusion. A NULL-to-zero coercion would make
	// the engine look successful while pairing nobody.
	unlimited := Player{ID: "unlimited", Status: StatusActive, PowerRating: 1800, InFlight: 40}
	others := []Player{active("a", 2000), active("b", 1900), active("c", 1700)}
	history := History{}

	for round := 1; round <= 20; round++ {
		cfg := defaultConfig()
		cfg.RoundNumber = round

		res := generate(t, append([]Player{unlimited}, others...), history, cfg)

		var found bool
		for _, p := range res.Pairings {
			if p.White == unlimited.ID || p.Black == unlimited.ID {
				found = true
			}
			history.Record(p.White, p.Black, round)
		}
		assert.True(t, found, "round %d did not pair the unlimited player", round)
		for _, e := range res.Exclusions {
			assert.NotEqual(t, unlimited.ID, e.ID, "round %d excluded the unlimited player", round)
		}
	}
}

func TestGenerate_CapacityWorkedExample(t *testing.T) {
	// The §5.8 table for a player who chose max_concurrent_games = 4.
	tests := []struct {
		round      int
		inFlight   int
		wantPaired bool
	}{
		{round: 1, inFlight: 0, wantPaired: true},
		{round: 2, inFlight: 1, wantPaired: true},
		{round: 3, inFlight: 2, wantPaired: true},
		{round: 4, inFlight: 3, wantPaired: true},
		{round: 5, inFlight: 4, wantPaired: false},
		{round: 6, inFlight: 4, wantPaired: false},
		{round: 7, inFlight: 3, wantPaired: true},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("round %d with %d in flight", tc.round, tc.inFlight), func(t *testing.T) {
			capped := Player{
				ID: "capped", Status: StatusActive, PowerRating: 1800,
				InFlight: tc.inFlight, MaxConcurrent: ptr(4),
			}
			players := []Player{capped, active("a", 1810), active("b", 1790), active("c", 1780)}
			cfg := defaultConfig()
			cfg.RoundNumber = tc.round

			res := generate(t, players, nil, cfg)

			if tc.wantPaired {
				assert.Empty(t, res.Exclusions)
				assert.Contains(t, pairedIDs(res), capped.ID)
				return
			}
			assert.Contains(t, res.Exclusions, Exclusion{
				ID: capped.ID, Reason: ReasonAtCapacity, InFlight: tc.inFlight, Cap: 4,
			})
			assert.NotContains(t, pairedIDs(res), capped.ID)
		})
	}
}

func TestGenerate_DoubleGame(t *testing.T) {
	volunteer := func(id string, rating int) Player {
		p := active(id, rating)
		p.AcceptsDouble = true
		return p
	}

	t.Run("the volunteer plays two distinct opponents, one white and one black", func(t *testing.T) {
		players := []Player{
			active("a", 2000),
			active("b", 1900),
			active("c", 1800),
			active("d", 1700),
			volunteer("v", 1600),
		}

		res := generate(t, players, nil, defaultConfig())

		require.NotNil(t, res.DoubleGame)
		assert.Equal(t, "v", *res.DoubleGame)
		assert.Nil(t, res.Bye)
		assert.Empty(t, res.Exclusions)
		require.Len(t, res.Pairings, 3)
		assert.Equal(t, 5, res.PoolSize)

		var opponents []string
		for _, p := range res.Pairings {
			if p.White == "v" {
				opponents = append(opponents, p.Black)
			} else if p.Black == "v" {
				opponents = append(opponents, p.White)
			}
		}
		require.Len(t, opponents, 2)
		assert.NotEqual(t, opponents[0], opponents[1], "the two opponents must be distinct")
	})

	t.Run("rotation picks the volunteer, never the rating", func(t *testing.T) {
		never := volunteer("never", 1500)
		recent := volunteer("recent", 2100)
		recent.LastDoubleRound, recent.DoubleCount = ptr(9), 1
		older := volunteer("older", 2050)
		older.LastDoubleRound, older.DoubleCount = ptr(6), 1

		players := []Player{active("a", 2000), active("b", 1900), never, recent, older}

		res := generate(t, players, nil, defaultConfig())

		require.NotNil(t, res.DoubleGame)
		assert.Equal(t, "never", *res.DoubleGame, "a player who never had one has waited longest")
	})

	t.Run("fewest doubles then id break the rotation ties", func(t *testing.T) {
		twice := volunteer("twice", 2000)
		twice.LastDoubleRound, twice.DoubleCount = ptr(7), 2
		once := volunteer("once", 1900)
		once.LastDoubleRound, once.DoubleCount = ptr(7), 1
		alsoOnce := volunteer("also-once", 1800)
		alsoOnce.LastDoubleRound, alsoOnce.DoubleCount = ptr(7), 1

		res := generate(t, []Player{twice, once, alsoOnce}, nil, defaultConfig())

		require.NotNil(t, res.DoubleGame)
		assert.Equal(t, "also-once", *res.DoubleGame)
	})

	t.Run("opting out is respected, and the round falls back to a bye", func(t *testing.T) {
		players := []Player{active("a", 2000), active("b", 1900), active("c", 1800)}

		res := generate(t, players, nil, defaultConfig())

		assert.Nil(t, res.DoubleGame)
		require.NotNil(t, res.Bye)
	})

	t.Run("the cap must still hold after both games", func(t *testing.T) {
		// ongoing + 2 <= cap (§6.2 6a): 2 + 2 <= 4 qualifies, 3 + 2 > 4
		// does not, even though 3 < 4 keeps them in the pool.
		fits := volunteer("fits", 1900)
		fits.InFlight, fits.MaxConcurrent = 2, ptr(4)
		tooFull := volunteer("too-full", 2000)
		tooFull.InFlight, tooFull.MaxConcurrent = 3, ptr(4)

		res := generate(t, []Player{active("a", 2100), fits, tooFull}, nil, defaultConfig())

		require.NotNil(t, res.DoubleGame)
		assert.Equal(t, "fits", *res.DoubleGame)
	})

	t.Run("bye_only skips the double game entirely", func(t *testing.T) {
		cfg := defaultConfig()
		cfg.OddPool = ByeOnly
		players := []Player{active("a", 2000), active("b", 1900), volunteer("v", 1800)}

		res := generate(t, players, nil, cfg)

		assert.Nil(t, res.DoubleGame)
		require.NotNil(t, res.Bye)
	})

	t.Run("a pool of three where the volunteer is nobody's cheapest partner", func(t *testing.T) {
		// a and b are far closer to each other than to v, so a plain
		// greedy scan would pair a with b and strand v's two slots.
		players := []Player{active("a", 2000), active("b", 1990), volunteer("v", 1500)}

		res := generate(t, players, nil, defaultConfig())

		require.NotNil(t, res.DoubleGame)
		assert.Len(t, res.Pairings, 2)
	})

	t.Run("the volunteer's colours balance their two opponents", func(t *testing.T) {
		v := volunteer("v", 1800)
		owedWhite := active("owed-white", 1810)
		owedWhite.ColorScore = -3
		owedBlack := active("owed-black", 1790)
		owedBlack.ColorScore = 3

		res := generate(t, []Player{v, owedWhite, owedBlack}, nil, defaultConfig())

		require.NotNil(t, res.DoubleGame)
		colours := map[string]string{}
		for _, p := range res.Pairings {
			colours[p.White] = "white"
			colours[p.Black] = "black"
		}
		assert.Equal(t, "white", colours["owed-white"])
		assert.Equal(t, "black", colours["owed-black"])
	})
}

func TestChooseBye_IgnoresRating(t *testing.T) {
	// Byes are a rotation, never a judgement: the same player is chosen
	// whatever the ratings say.
	base := []Player{
		{ID: "a", Status: StatusActive, ByeCount: 1, LastByeRound: ptr(8)},
		{ID: "b", Status: StatusActive, ByeCount: 1, LastByeRound: ptr(4)},
		{ID: "c", Status: StatusActive, ByeCount: 2, LastByeRound: ptr(4)},
	}

	ratings := [][3]int{{2400, 1200, 1800}, {1200, 2400, 1800}, {1800, 1800, 1800}}
	for i, r := range ratings {
		t.Run(fmt.Sprintf("ratings %d", i), func(t *testing.T) {
			players := make([]Player, len(base))
			copy(players, base)
			for j := range players {
				players[j].PowerRating = r[j]
			}

			res := generate(t, players, nil, defaultConfig())

			require.NotNil(t, res.Bye)
			assert.Equal(t, "b", *res.Bye, "longest since their last bye, whatever they are rated")
			assert.Contains(t, res.Exclusions, Exclusion{ID: "b", Reason: ReasonBye})
		})
	}
}

func TestGenerate_ByeRotationIsFair(t *testing.T) {
	// Over many rounds with an odd pool and nobody taking double games,
	// the byes must spread evenly rather than settling on one player.
	players := []Player{
		active("a", 2000), active("b", 1900), active("c", 1800),
		active("d", 1700), active("e", 1600),
	}
	history := History{}

	for round := 1; round <= 200; round++ {
		cfg := defaultConfig()
		cfg.RoundNumber = round

		res := Generate(players, history, cfg, Greedy{})

		require.NotNil(t, res.Bye)
		for i := range players {
			if players[i].ID == *res.Bye {
				players[i].ByeCount++
				players[i].LastByeRound = ptr(round)
			}
		}
		for _, p := range res.Pairings {
			history.Record(p.White, p.Black, round)
		}
	}

	lowest, highest := players[0].ByeCount, players[0].ByeCount
	for _, p := range players {
		lowest = min(lowest, p.ByeCount)
		highest = max(highest, p.ByeCount)
	}
	assert.LessOrEqual(t, highest-lowest, 1, "byes must be spread evenly: %v", byeCounts(players))
}

func TestGenerate_SmallPools(t *testing.T) {
	t.Run("a pool of one is a bye and no pairings", func(t *testing.T) {
		players := []Player{active("a", 1800), {ID: "b", Status: StatusInactive}}

		res := generate(t, players, nil, defaultConfig())

		assert.Empty(t, res.Pairings)
		require.NotNil(t, res.Bye)
		assert.Equal(t, "a", *res.Bye)
		assert.Equal(t, 1, res.PoolSize)
	})

	t.Run("a pool of one never takes a double game", func(t *testing.T) {
		lone := active("a", 1800)
		lone.AcceptsDouble = true

		res := generate(t, []Player{lone}, nil, defaultConfig())

		assert.Nil(t, res.DoubleGame)
		require.NotNil(t, res.Bye)
	})

	t.Run("an empty pool is an empty round", func(t *testing.T) {
		players := []Player{{ID: "a", Status: StatusPaused}}

		res := generate(t, players, nil, defaultConfig())

		assert.Empty(t, res.Pairings)
		assert.Nil(t, res.Bye)
		assert.Nil(t, res.DoubleGame)
		assert.Zero(t, res.PoolSize)
		assert.Len(t, res.Exclusions, 1)
	})

	t.Run("no players at all", func(t *testing.T) {
		res := generate(t, nil, nil, defaultConfig())

		assert.Empty(t, res.Pairings)
		assert.Empty(t, res.Exclusions)
		assert.Zero(t, res.PoolSize)
	})
}

func TestGenerate_IsDeterministic(t *testing.T) {
	// Same inputs, same output — whatever order the caller's slice is
	// in. This is what makes the review window meaningful: an admin who
	// regenerates sees the round they reviewed.
	players := []Player{
		{ID: "a", Status: StatusActive, PowerRating: 2000, ColorScore: 2, AcceptsDouble: true},
		{ID: "b", Status: StatusActive, PowerRating: 1900, ColorScore: -1},
		{ID: "c", Status: StatusActive, PowerRating: 1900, ColorScore: 0, AcceptsDouble: true},
		{ID: "d", Status: StatusActive, PowerRating: 1750, ColorScore: 3},
		{ID: "e", Status: StatusActive, PowerRating: 1600, InFlight: 2, MaxConcurrent: ptr(2)},
		{ID: "f", Status: StatusInactive, PowerRating: 1500},
		{ID: "g", Status: StatusActive, PowerRating: 1400, ColorScore: -2},
	}
	history := History{}
	history.Record("a", "c", 9)
	history.Record("b", "d", 7)

	orders := [][]Player{
		players,
		{players[6], players[5], players[4], players[3], players[2], players[1], players[0]},
		{players[3], players[0], players[6], players[1], players[4], players[2], players[5]},
	}

	want := generate(t, orders[0], history, defaultConfig())
	for i, order := range orders[1:] {
		got := generate(t, order, history, defaultConfig())
		assert.Equal(t, want, got, "order %d produced a different round", i+1)
	}
}

func pairKeys(res Result) [][2]string {
	keys := make([][2]string, 0, len(res.Pairings))
	for _, p := range res.Pairings {
		keys = append(keys, historyKey(p.White, p.Black))
	}
	return keys
}

func pairedIDs(res Result) []string {
	ids := make([]string, 0, 2*len(res.Pairings))
	for _, p := range res.Pairings {
		ids = append(ids, p.White, p.Black)
	}
	return ids
}

func byeCounts(players []Player) map[string]int {
	counts := map[string]int{}
	for _, p := range players {
		counts[p.ID] = p.ByeCount
	}
	return counts
}
