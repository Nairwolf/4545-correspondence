package pairing

import (
	"math/rand"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bruteForce is the reference solver: it enumerates every perfect
// matching and keeps the cheapest, which is what the blossom algorithm
// would compute (§6.2 step 4). Far too slow for a real pool — it exists
// to measure how far greedy is from optimal on the small pools where
// greedy's short-sightedness would show up first.
type bruteForce struct{}

func (bruteForce) Match(g *Graph) [][2]int {
	n := g.N()
	taken := make([]bool, n)
	var best [][2]int
	bestCost := -1

	var walk func(acc [][2]int, cost int)
	walk = func(acc [][2]int, cost int) {
		next := -1
		for i := range n {
			if !taken[i] {
				next = i
				break
			}
		}
		if next == -1 {
			if bestCost == -1 || cost < bestCost {
				best, bestCost = slices.Clone(acc), cost
			}
			return
		}
		taken[next] = true
		for j := next + 1; j < n; j++ {
			if taken[j] || !g.Allowed(next, j) {
				continue
			}
			taken[j] = true
			walk(append(acc, [2]int{next, j}), cost+g.Cost(next, j))
			taken[j] = false
		}
		taken[next] = false
	}
	walk(nil, 0)
	return best
}

func TestGreedy_AgainstBruteForceOnSmallPools(t *testing.T) {
	// A fixed seed, so the fixtures are the same on every run: this is
	// a corpus, not a fuzz test.
	rng := rand.New(rand.NewSource(1))
	cfg := defaultConfig()

	var fixtures, optimal, forcedRepeats int
	for range 200 {
		for size := 2; size <= 6; size++ {
			for _, withVolunteer := range []bool{false, true} {
				g, ok := corpusGraph(rng, size, withVolunteer, cfg)
				if !ok {
					continue
				}

				got := Greedy{}.Match(g)
				want := bruteForce{}.Match(g)
				assertValidMatching(t, g, got)
				assertValidMatching(t, g, want)

				excess := matchingCost(g, got) - matchingCost(g, want)
				require.GreaterOrEqual(t, excess, 0, "brute force must be optimal")
				fixtures++
				switch {
				case excess == 0:
					optimal++
				case excess >= cfg.RepeatPenalty:
					// Greedy pairing the top of the list well can leave
					// two players who recently met as the only pair
					// left — the failure mode the blossom solver would
					// remove.
					forcedRepeats++
				}
			}
		}
	}

	// These are measurements, not aspirations: they pin how far the
	// greedy solver is from optimal on the small pools where its
	// short-sightedness shows up first, which is what decision 1 defers
	// the blossom port on. Loosening them silently is a behaviour
	// change; on real pools of ~100 the repeat graph is far sparser
	// than these fixtures and forced repeats should be rarer still —
	// the round diagnostics count them, so the first live rounds
	// answer it.
	t.Logf("greedy optimal on %d of %d pools; %d forced a repeat the optimum avoided",
		optimal, fixtures, forcedRepeats)
	assert.Greater(t, optimal*100/fixtures, 60, "greedy was optimal on %d of %d pools", optimal, fixtures)
	assert.Less(t, forcedRepeats*100/fixtures, 10, "greedy forced a repeat on %d of %d pools", forcedRepeats, fixtures)
}

func TestGreedy_PairsTheVolunteersSlotsFirst(t *testing.T) {
	// Without the special case, the scan pairs a with b — their costs
	// are far lower than anything involving v — and strands v's two
	// slots, which have no edge between them.
	pool := []Player{active("a", 2000), active("b", 1990), active("v", 1500)}
	slots := []Slot{{Pool: 0, ID: "a"}, {Pool: 1, ID: "b"}, {Pool: 2, ID: "v"}, {Pool: 2, ID: "v"}}
	g := buildGraph(pool, slots, History{}, defaultConfig())

	matching := Greedy{}.Match(g)

	assertValidMatching(t, g, matching)
}

// randomPool builds a plausible pool: ratings in the league's range,
// colour scores that are sometimes badly out, and a chance that any two
// players met recently.
func randomPool(rng *rand.Rand, size int) ([]Player, History) {
	pool := make([]Player, size)
	for i := range pool {
		pool[i] = Player{
			ID:          string(rune('a' + i)),
			Status:      StatusActive,
			PowerRating: 1200 + rng.Intn(1200),
			ColorScore:  rng.Intn(9) - 4,
		}
	}
	history := History{}
	for i := range pool {
		for j := i + 1; j < size; j++ {
			if rng.Intn(4) == 0 {
				history.Record(pool[i].ID, pool[j].ID, 6+rng.Intn(4))
			}
		}
	}
	return pool, history
}

// corpusGraph builds one fixture of the brute-force corpus: a random
// pool of size players, plus a second slot for the first player when
// withVolunteer. ok is false when that leaves an odd number of slots.
// The pool is drawn before the parity check, so a skipped fixture
// consumes the same random numbers as a kept one and the corpus stays
// the same whichever solver reads it.
func corpusGraph(rng *rand.Rand, size int, withVolunteer bool, cfg Config) (g *Graph, ok bool) {
	pool, history := randomPool(rng, size)
	slots := make([]Slot, 0, size+1)
	for i, p := range pool {
		slots = append(slots, Slot{Pool: i, ID: p.ID})
	}
	if withVolunteer {
		slots = append(slots, Slot{Pool: 0, ID: pool[0].ID})
	}
	if len(slots)%2 == 1 {
		return nil, false
	}
	return buildGraph(pool, slots, history, cfg), true
}

func assertValidMatching(t *testing.T, g *Graph, matching [][2]int) {
	t.Helper()

	require.Len(t, matching, g.N()/2, "a matching must cover every slot")
	seen := make([]bool, g.N())
	for _, m := range matching {
		assert.True(t, g.Allowed(m[0], m[1]), "slots %d and %d have no edge", m[0], m[1])
		for _, slot := range m {
			assert.False(t, seen[slot], "slot %d was matched twice", slot)
			seen[slot] = true
		}
	}
}

func matchingCost(g *Graph, matching [][2]int) int {
	var total int
	for _, m := range matching {
		total += g.Cost(m[0], m[1])
	}
	return total
}
