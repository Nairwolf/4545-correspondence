package pairing

import "github.com/nairwolf/4545-correspondence/internal/scoring"

// chooseVolunteer is §6.2 step 6a: who absorbs the extra game when the
// pool is odd. It runs BEFORE the matching so the volunteer's second
// slot is part of it, and it selects on rotation alone — never on cost,
// rating or who happens to pair well — so the choice is explainable and
// the engine never has to "try each candidate".
//
// A pool of one cannot support a double game (the volunteer's two
// opponents must be distinct), so it falls through to the bye.
func chooseVolunteer(pool []Player, cfg Config) (index int, ok bool) {
	if cfg.OddPool == ByeOnly || len(pool) < 3 {
		return 0, false
	}
	best := -1
	for i, p := range pool {
		if !p.AcceptsDouble {
			continue
		}
		// §6.2 6a: ongoing + 2 <= cap, i.e. the cap must still hold
		// once both games exist. That is CapacityAllows with one of the
		// two games already counted; nil (unlimited) passes as always.
		if !scoring.CapacityAllows(p.MaxConcurrent, p.InFlight+1) {
			continue
		}
		if best == -1 || moreDueDouble(p, pool[best]) {
			best = i
		}
	}
	if best == -1 {
		return 0, false
	}
	return best, true
}

// chooseBye is §6.2 step 6b. Rating, level, XP and results are
// deliberately absent: a bye must never look like punishment for being
// weak. A player who has never had one has waited forever and so comes
// first.
func chooseBye(pool []Player) int {
	best := 0
	for i, p := range pool[1:] {
		if moreDueBye(p, pool[best]) {
			best = i + 1
		}
	}
	return best
}

// moreDueDouble reports whether a is ahead of b in the double-game
// rotation: longest since their last one, then fewest in total, then
// the lower id.
func moreDueDouble(a, b Player) bool {
	if c := compareWaited(a.LastDoubleRound, b.LastDoubleRound); c != 0 {
		return c < 0
	}
	if a.DoubleCount != b.DoubleCount {
		return a.DoubleCount < b.DoubleCount
	}
	return a.ID < b.ID
}

// moreDueBye is the same rotation over byes.
func moreDueBye(a, b Player) bool {
	if c := compareWaited(a.LastByeRound, b.LastByeRound); c != 0 {
		return c < 0
	}
	if a.ByeCount != b.ByeCount {
		return a.ByeCount < b.ByeCount
	}
	return a.ID < b.ID
}

// compareWaited orders two "round they last had one" values by how long
// ago that was, longest first. nil is never, which beats every round
// number.
func compareWaited(a, b *int) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	case *a != *b:
		if *a < *b {
			return -1
		}
		return 1
	}
	return 0
}
