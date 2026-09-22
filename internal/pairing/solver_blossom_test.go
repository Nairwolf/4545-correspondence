package pairing

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBlossom_AgainstBruteForceOnSmallPools(t *testing.T) {
	// A fixed seed, like the greedy measurement: a corpus, not a fuzz
	// test. Up to 10 slots, where brute force still checks all 945
	// possible pairings in no time.
	rng := rand.New(rand.NewSource(2))
	cfg := defaultConfig()

	var fixtures int
	for range 200 {
		for size := 2; size <= 10; size++ {
			for _, withVolunteer := range []bool{false, true} {
				g, ok := corpusGraph(rng, size, withVolunteer, cfg)
				if !ok || g.N() > 10 {
					continue
				}

				got := Blossom{}.Match(g)
				want := bruteForce{}.Match(g)
				assertValidMatching(t, g, got)
				// Two different pairings can tie; the cost is what
				// must match.
				require.Equal(t, matchingCost(g, want), matchingCost(g, got),
					"fixture %d (%d slots): blossom is not optimal", fixtures, g.N())
				requireOptimumCertificate(t, g)
				fixtures++
			}
		}
	}
	t.Logf("blossom optimal on all %d pools", fixtures)
}

func TestBlossom_AvoidsTheRematchGreedyForces(t *testing.T) {
	// Greedy pairs A with B, the closest rating, and so leaves C and D,
	// who just met, as the only pair left. Pairing A–C and B–D, or A–D
	// and B–C, costs about 800 rating points and no rematch.
	players := []Player{active("A", 2000), active("B", 1990), active("C", 1600), active("D", 1590)}
	history := History{}
	history.Record("C", "D", 9)

	greedy := Generate(players, history, defaultConfig(), Greedy{})
	blossom := Generate(players, history, defaultConfig(), Blossom{})
	assertPostConditions(t, players, greedy)
	assertPostConditions(t, players, blossom)

	assert.Equal(t, 1, greedy.RepeatPairings)
	assert.Zero(t, blossom.RepeatPairings)
	assert.NotContains(t, pairKeys(blossom), historyKey("C", "D"))
}

func TestBlossom_LargePools(t *testing.T) {
	// Far beyond brute force: the dual certificate proves optimality,
	// and greedy's result bounds it from above.
	cfg := defaultConfig()
	for _, size := range []int{51, 120, 199, 200} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(size)))
			players, history := largePool(rng, size)

			greedy := Generate(players, history, cfg, Greedy{})
			blossom := Generate(players, history, cfg, Blossom{})
			assertPostConditions(t, players, blossom)
			assert.LessOrEqual(t, blossom.RepeatPairings, greedy.RepeatPairings)

			g := poolGraph(players, history, cfg)
			got := Blossom{}.Match(g)
			assertValidMatching(t, g, got)
			assert.LessOrEqual(t, matchingCost(g, got), matchingCost(g, Greedy{}.Match(g)))
			requireOptimumCertificate(t, g)
		})
	}
}

func TestBlossom_IsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	players, history := largePool(rng, 99)

	want := Generate(players, history, defaultConfig(), Blossom{})
	for range 3 {
		shuffled := append([]Player(nil), players...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		assert.Equal(t, want, Generate(shuffled, history, defaultConfig(), Blossom{}))
	}
}

func BenchmarkBlossom_200Players(b *testing.B) {
	rng := rand.New(rand.NewSource(200))
	players, history := largePool(rng, 200)
	g := poolGraph(players, history, defaultConfig())
	for b.Loop() {
		Blossom{}.Match(g)
	}
}

// requireOptimumCertificate reruns the solver on g and checks the
// linear-programming certificate it leaves behind.
func requireOptimumCertificate(t *testing.T, g *Graph) {
	t.Helper()
	m := newMatcher(g)
	m.run()
	require.NoError(t, m.verifyOptimum())
}

// largePool is a league-sized pool: ratings across the league's range,
// uneven colour scores, everyone willing to play a double game, and
// each player having met about five others in the recent window.
func largePool(rng *rand.Rand, size int) ([]Player, History) {
	pool := make([]Player, size)
	for i := range pool {
		pool[i] = Player{
			ID:            fmt.Sprintf("p%03d", i),
			Status:        StatusActive,
			PowerRating:   1200 + rng.Intn(1200),
			ColorScore:    rng.Intn(9) - 4,
			AcceptsDouble: true,
		}
	}
	history := History{}
	for i := range pool {
		for range 5 {
			if j := rng.Intn(size); j != i {
				history.Record(pool[i].ID, pool[j].ID, 6+rng.Intn(4))
			}
		}
	}
	return pool, history
}

// poolGraph is the graph the engine would build for an even pool of
// active players with no volunteer: one slot each.
func poolGraph(players []Player, history History, cfg Config) *Graph {
	if len(players)%2 == 1 {
		players = players[:len(players)-1]
	}
	slots := make([]Slot, len(players))
	for i, p := range players {
		slots[i] = Slot{Pool: i, ID: p.ID}
	}
	return buildGraph(players, slots, history, cfg)
}
