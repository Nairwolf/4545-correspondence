package pairing

// Solver turns the cost graph into a perfect matching over its slots
// (§6.2 step 4). It must be deterministic: the same graph must always
// produce the same matching, whatever order its edges are examined in.
// Greedy is what Phase 4 ships; a minimum-weight perfect matching drops
// in here later with no change to the engine around it.
type Solver interface {
	// Match returns every slot paired exactly once, as pairs of slot
	// indices. It never returns a pair for which Allowed is false.
	Match(g *Graph) [][2]int
}

// Greedy walks the pool from the strongest player down, pairing each
// still-unmatched slot with its cheapest still-unmatched partner. This
// is what the spreadsheet effectively did, and §6.2 step 4 accepts it:
// it is not globally optimal — a poor pairing at the top can leave two
// badly matched players at the bottom — but it is simple enough to
// audit by eye against a generated round.
type Greedy struct{}

func (Greedy) Match(g *Graph) [][2]int {
	n := g.N()
	taken := make([]bool, n)
	pairs := make([][2]int, 0, n/2)

	// The double-game volunteer's two slots have no edge between them,
	// so a plain scan can strand them as the last unmatched pair with
	// nothing legal left to do. Give them their two cheapest partners
	// first; being distinct slots, the partners are distinct players.
	if a, b, ok := duplicateSlots(g); ok {
		taken[a], taken[b] = true, true
		pa := cheapestPartner(g, a, taken)
		taken[pa] = true
		pb := cheapestPartner(g, b, taken)
		taken[pb] = true
		pairs = append(pairs, [2]int{a, pa}, [2]int{b, pb})
	}

	for i := range n {
		if taken[i] {
			continue
		}
		taken[i] = true
		j := cheapestPartner(g, i, taken)
		taken[j] = true
		pairs = append(pairs, [2]int{i, j})
	}
	return pairs
}

// duplicateSlots finds the two slots of the double-game volunteer,
// the only player who has more than one.
func duplicateSlots(g *Graph) (a, b int, ok bool) {
	for i := range g.N() {
		for j := i + 1; j < g.N(); j++ {
			if g.Slots[i].Pool == g.Slots[j].Pool {
				return i, j, true
			}
		}
	}
	return 0, 0, false
}

// cheapestPartner picks slot i's partner: lowest cost, ties broken by
// the partner's id (§6.2 step 4). With i fixed, that is the whole of
// the (cost, lower id, higher id) edge order.
func cheapestPartner(g *Graph, i int, taken []bool) int {
	best := -1
	for j := range g.N() {
		if taken[j] || !g.Allowed(i, j) {
			continue
		}
		if best == -1 {
			best = j
			continue
		}
		switch c := g.Cost(i, j); {
		case c < g.Cost(i, best):
			best = j
		case c == g.Cost(i, best) && g.Slots[j].ID < g.Slots[best].ID:
			best = j
		}
	}
	if best == -1 {
		// Unreachable: the repeat penalty is finite, so every pair of
		// distinct players has an edge, and the volunteer's two slots
		// are matched before the scan begins.
		panic("pairing: greedy: no legal partner left")
	}
	return best
}
