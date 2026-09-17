// Package pairing implements the spec §6.2 pairing engine: it turns one
// round's eligible players and their recent-opponent history into that
// round's pairings, exclusions, and either a double game or a bye when
// the pool is odd. Like internal/scoring and internal/matching it is
// pure — no database, no HTTP, no clock, no randomness — which is what
// makes the engine deterministic (§6.2: the same inputs must produce the
// same output, so an admin who regenerates a round during the review
// window sees the round they reviewed) and exhaustively testable without
// a database.
package pairing

import (
	"cmp"
	"slices"
)

// Status is a player's §5.7 eligibility state, already resolved by the
// caller from users.status and the player_profiles flags. Only Active
// players enter the pool; the rest become exclusion rows so the
// dashboard can answer "why didn't I get a game this week?".
type Status int

const (
	// StatusActive is an approved, active, unpaused player.
	StatusActive Status = iota
	// StatusInactive is a player who has declared themselves inactive.
	StatusInactive
	// StatusPaused is a player paused by an admin.
	StatusPaused
	// StatusAutoPaused is a player paused by the missed-start rule.
	StatusAutoPaused
)

// Player is one candidate for the round. Ratings are integers because
// power_rating is an integer column; nothing here is a float or a time,
// so a fixture is a plain struct literal.
type Player struct {
	// ID is the users.id, and the engine's only tie-break key.
	ID string
	// Status decides pool membership before capacity is considered.
	Status Status
	// PowerRating is the §5.4 value the engine sorts and matches on.
	PowerRating int
	// ColorScore is §5.5: games as white minus games as black.
	// Positive means the player has had white too often.
	ColorScore int
	// InFlight is the §5.8 in-flight game count: in-progress games plus
	// pending pairings of published rounds with no game yet.
	InFlight int
	// MaxConcurrent is the player's cap. nil means UNLIMITED — the
	// default for every player — and is never coerced to zero.
	MaxConcurrent *int
	// AcceptsDouble is the §8.3 opt-in to absorbing a double game.
	AcceptsDouble bool
	// LastDoubleRound and LastByeRound are the numbers of the rounds in
	// which the player last took a double game or a bye; nil means
	// never, which counts as having waited the longest.
	LastDoubleRound *int
	LastByeRound    *int
	// DoubleCount and ByeCount are their totals, the second tie-break
	// of each rotation.
	DoubleCount int
	ByeCount    int
}

// OddPoolStrategy is the pairing.odd_pool_strategy setting (§4.2).
type OddPoolStrategy string

const (
	// DoubleThenBye prefers a double-game volunteer (§6.2 step 6a) and
	// falls back to a bye.
	DoubleThenBye OddPoolStrategy = "double_then_bye"
	// ByeOnly always gives a bye, skipping step 6a.
	ByeOnly OddPoolStrategy = "bye_only"
)

// Config is the §4.2 pairing settings as they apply to one generation,
// plus the number of the round being generated (the repeat window is
// measured back from it).
type Config struct {
	RoundNumber       int
	AvoidRecentRounds int
	ColorWeight       int
	RepeatPenalty     int
	OddPool           OddPoolStrategy
}

// Pairing is one generated game, with the diagnostics the admin round
// view shows (§8.5). The diagnostics are stored rather than recomputed
// because standings move after generation.
type Pairing struct {
	White, Black string
	// RatingGap is |power(white) − power(black)| at generation.
	RatingGap int
	// ColorPenalty is the leftover colour imbalance this game actually
	// leaves behind, under the colours assigned.
	ColorPenalty int
	// RepeatOfRound is the round in which these two last met, if that
	// was within the avoid_recent_rounds window; nil otherwise.
	RepeatOfRound *int
}

// Exclusion explains one player's absence from the round. InFlight and
// Cap are meaningful only for ReasonAtCapacity, where the dashboard and
// the admin view quote the numbers.
type Exclusion struct {
	ID       string
	Reason   Reason
	InFlight int
	Cap      int
}

// Result is one round as the engine sees it, before any of it is
// written. PoolSize is the size of the pool after the §5.8 capacity
// filter and before the bye, so it always equals the number of paired
// players plus the bye.
type Result struct {
	Pairings   []Pairing
	Exclusions []Exclusion
	// DoubleGame and Bye are the odd-pool outcome: the id of the player
	// who took two games, or the id of the player who sat out. At most
	// one is set, and neither is for an even pool.
	DoubleGame *string
	Bye        *string

	PoolSize       int
	RepeatPairings int
}

// Generate runs the whole of §6.2 for one round. players is every
// approved member with their §5.7 status (pending applicants are not
// loaded at all), history is who met whom and when, and solve is the
// matching solver — Greedy today, a minimum-weight perfect matching
// later, with no other change to this function.
func Generate(players []Player, history History, cfg Config, solve Solver) Result {
	// One sort up front. The same total order — power rating
	// descending, then id — is the pool's scan order, the order the
	// solver sees, and the order exclusions and pairings come out in,
	// so no result can depend on the caller's slice order.
	ordered := make([]Player, len(players))
	copy(ordered, players)
	slices.SortFunc(ordered, func(a, b Player) int {
		if c := cmp.Compare(b.PowerRating, a.PowerRating); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})

	pool, exclusions := buildPool(ordered)
	res := Result{Exclusions: exclusions, PoolSize: len(pool)}

	slots := make([]Slot, len(pool))
	for i, p := range pool {
		slots[i] = Slot{Pool: i, ID: p.ID}
	}

	if len(pool)%2 == 1 {
		if v, ok := chooseVolunteer(pool, cfg); ok {
			id := pool[v].ID
			res.DoubleGame = &id
			slots = append(slots, Slot{Pool: v, ID: id})
		} else {
			b := chooseBye(pool)
			id := pool[b].ID
			res.Bye = &id
			res.Exclusions = append(res.Exclusions, Exclusion{ID: id, Reason: ReasonBye})
			slots = slices.Delete(slots, b, b+1)
		}
	}
	if len(slots) == 0 {
		return res
	}

	g := buildGraph(pool, slots, history, cfg)
	matched := solve.Match(g)

	// Sort the matching into a display order that does not depend on
	// which solver produced it: strongest player first, by their
	// position in the ordered pool.
	slices.SortFunc(matched, func(x, y [2]int) int {
		xa, xb := minMaxPool(g, x)
		ya, yb := minMaxPool(g, y)
		if c := cmp.Compare(xa, ya); c != 0 {
			return c
		}
		return cmp.Compare(xb, yb)
	})

	res.Pairings = make([]Pairing, 0, len(matched))
	for _, m := range matched {
		a, b := pool[g.Slots[m[0]].Pool], pool[g.Slots[m[1]].Pool]
		white, black := assignColours(a, b)
		res.Pairings = append(res.Pairings, newPairing(white, black, history, cfg))
	}
	if res.DoubleGame != nil {
		byID := make(map[string]Player, len(pool))
		for _, p := range pool {
			byID[p.ID] = p
		}
		assignDoubleColours(res.Pairings, byID, *res.DoubleGame)
	}
	for _, p := range res.Pairings {
		if p.RepeatOfRound != nil {
			res.RepeatPairings++
		}
	}
	return res
}

// newPairing records the two players in the colours they were given,
// with the §8.5 diagnostics for those colours.
func newPairing(white, black Player, history History, cfg Config) Pairing {
	p := Pairing{
		White:        white.ID,
		Black:        black.ID,
		RatingGap:    abs(white.PowerRating - black.PowerRating),
		ColorPenalty: leftoverImbalance(white, black),
	}
	if round, repeat := history.repeatOf(white.ID, black.ID, cfg); repeat {
		p.RepeatOfRound = &round
	}
	return p
}

func minMaxPool(g *Graph, m [2]int) (lo, hi int) {
	a, b := g.Slots[m[0]].Pool, g.Slots[m[1]].Pool
	return min(a, b), max(a, b)
}
