package pairing

import "github.com/nairwolf/4545-correspondence/internal/scoring"

// Reason is why a player is not in this round's pairings. The values
// are the exclusion_reason enum's, so the rounds service can store one
// without a translation table. pending_approval and no_valid_token
// exist in the database for Phase 5 and are never produced here:
// applicants are not loaded into the pool at all, and until Lichess
// game creation exists every game is started by hand, so a token
// cannot gate the pool (§5.7 item 6).
type Reason string

const (
	ReasonAtCapacity Reason = "at_capacity"
	ReasonInactive   Reason = "inactive"
	ReasonPaused     Reason = "paused"
	ReasonAutoPaused Reason = "auto_paused"
	ReasonBye        Reason = "bye"
)

// buildPool is §6.2 step 1: the §5.7 status filter and then the §5.8
// capacity rule. ordered is the whole approved membership in the
// engine's total order; every player leaves as exactly one pool member
// or exactly one exclusion, so the two lists together always account
// for the input.
func buildPool(ordered []Player) (pool []Player, exclusions []Exclusion) {
	for _, p := range ordered {
		switch p.Status {
		case StatusInactive:
			exclusions = append(exclusions, Exclusion{ID: p.ID, Reason: ReasonInactive})
			continue
		case StatusPaused:
			exclusions = append(exclusions, Exclusion{ID: p.ID, Reason: ReasonPaused})
			continue
		case StatusAutoPaused:
			exclusions = append(exclusions, Exclusion{ID: p.ID, Reason: ReasonAutoPaused})
			continue
		}
		if !scoring.CapacityAllows(p.MaxConcurrent, p.InFlight) {
			// Not nil: CapacityAllows is unconditionally true for the
			// unlimited default, which is the whole point of §5.8.
			exclusions = append(exclusions, Exclusion{
				ID:       p.ID,
				Reason:   ReasonAtCapacity,
				InFlight: p.InFlight,
				Cap:      *p.MaxConcurrent,
			})
			continue
		}
		pool = append(pool, p)
	}
	return pool, exclusions
}
