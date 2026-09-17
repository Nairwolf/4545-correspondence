package pairing

// History is who met whom and in which round, keyed by the unordered
// pair of ids. It is only ever looked up, never iterated, so a map
// cannot make the engine order-dependent.
type History map[[2]string]int

// Record notes that a and b met in round number, keeping the most
// recent meeting if they have met more than once.
func (h History) Record(a, b string, number int) {
	k := historyKey(a, b)
	if existing, ok := h[k]; !ok || number > existing {
		h[k] = number
	}
}

// LastMet returns the number of the most recent round in which a and b
// were paired, over whatever history the caller loaded.
func (h History) LastMet(a, b string) (number int, ok bool) {
	number, ok = h[historyKey(a, b)]
	return number, ok
}

// repeatOf answers the §6.2 step 3 question: did these two meet within
// the last avoid_recent_rounds rounds? A window of zero avoids nothing.
func (h History) repeatOf(a, b string, cfg Config) (number int, repeat bool) {
	if cfg.AvoidRecentRounds <= 0 {
		return 0, false
	}
	number, ok := h.LastMet(a, b)
	if !ok || number < cfg.RoundNumber-cfg.AvoidRecentRounds {
		return 0, false
	}
	return number, true
}

func historyKey(a, b string) [2]string {
	if a < b {
		return [2]string{a, b}
	}
	return [2]string{b, a}
}

// Slot is one entry in the matching. Each pool member has one, except
// the double-game volunteer, who has two — which is what makes an odd
// pool matchable.
type Slot struct {
	// Pool is the player's index in the ordered pool.
	Pool int
	// ID is the player's id, the solver's only tie-break key.
	ID string
}

// Graph is the weighted matching problem handed to a Solver: the slots,
// and the §6.2 step 3 cost of every legal edge between them. The
// volunteer's two slots are not an edge at all — that is the one pair
// a solver may not return.
type Graph struct {
	Slots []Slot

	cost    []int
	allowed []bool
}

// N is the number of slots, which is always even.
func (g *Graph) N() int { return len(g.Slots) }

// Cost is the edge weight between two slots; meaningless unless
// Allowed.
func (g *Graph) Cost(i, j int) int { return g.cost[i*g.N()+j] }

// Allowed reports whether i and j may be matched to each other.
func (g *Graph) Allowed(i, j int) bool { return g.allowed[i*g.N()+j] }

func (g *Graph) setEdge(i, j, cost int) {
	n := g.N()
	g.cost[i*n+j], g.cost[j*n+i] = cost, cost
	g.allowed[i*n+j], g.allowed[j*n+i] = true, true
}

func buildGraph(pool []Player, slots []Slot, history History, cfg Config) *Graph {
	n := len(slots)
	g := &Graph{Slots: slots, cost: make([]int, n*n), allowed: make([]bool, n*n)}
	for i := range n {
		for j := i + 1; j < n; j++ {
			if slots[i].Pool == slots[j].Pool {
				continue
			}
			g.setEdge(i, j, pairCost(pool[slots[i].Pool], pool[slots[j].Pool], history, cfg))
		}
	}
	return g
}

// pairCost is §6.2 step 3, entirely in rating points so the three terms
// can be summed meaningfully: the rating gap, the leftover colour
// imbalance priced at color_weight, and a large but FINITE penalty for
// a repeat — finite so that a small pool can still be paired at all,
// expensive so that it only happens when nothing else works.
func pairCost(a, b Player, history History, cfg Config) int {
	penalty, _ := colourPenalty(a, b)
	cost := abs(a.PowerRating-b.PowerRating) + cfg.ColorWeight*penalty
	if _, repeat := history.repeatOf(a.ID, b.ID, cfg); repeat {
		cost += cfg.RepeatPenalty
	}
	return cost
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
