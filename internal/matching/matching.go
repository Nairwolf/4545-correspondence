// Package matching implements spec §7.3: finding the one Lichess game
// that belongs to a pairing which doesn't have a game id yet. It is
// pure — no I/O, no Lichess client, no database — so it can be tested
// exhaustively without either. The caller (sync-games) is responsible
// for fetching the candidate games (via lichess.API.UserGames) and for
// knowing which game ids are already attached to some other pairing.
package matching

import "time"

// Pairing is the subset of a pairings/rounds row needed to match it
// against a candidate game.
type Pairing struct {
	WhiteLichessID string
	BlackLichessID string
	PairAt         time.Time
	DaysPerMove    int
}

// Candidate is the subset of a lichess.Game needed to evaluate it
// against a Pairing. Kept separate from lichess.Game itself so this
// package has no dependency on internal/lichess and stays trivially
// testable with plain struct literals.
type Candidate struct {
	ID             string
	Variant        string
	Rated          bool
	DaysPerTurn    int
	WhiteLichessID string // "" if the game has no human white player
	BlackLichessID string
	CreatedAt      time.Time
}

// Result is the outcome of matching one pairing against a set of
// candidates.
type Result struct {
	// Game is the matched candidate's id, non-empty iff exactly one
	// candidate was eligible.
	Game string
	// Ambiguous is true iff more than one candidate was eligible —
	// nothing is matched, and the pairing must be flagged for an admin
	// rather than guessed at.
	Ambiguous bool
}

// Match implements spec §7.3's exactly-one rule. A candidate is
// eligible only if ALL of:
//   - variant is standard and the game is rated (a casual game, or a
//     game in some other variant, between the same two players is not
//     this league's pairing)
//   - its days-per-turn matches the pairing's exactly
//   - its white and black players match the pairing's IN THAT COLOUR
//     ORDER — a game with the colours reversed is a different pairing,
//     not this one played the other way round
//   - it was created no earlier than pairAt minus one day (a small
//     margin for clock skew and a delayed job run, not an invitation to
//     match old games)
//   - its id is not already attached to a different pairing
//
// Exactly one eligible candidate is a match. Zero leaves the pairing
// pending — the game may simply not exist yet. More than one is
// Ambiguous, and nothing is attached: never guess which one is real.
func Match(pairing Pairing, candidates []Candidate, takenGameIDs map[string]bool) Result {
	cutoff := pairing.PairAt.Add(-24 * time.Hour)

	var eligible []Candidate
	for _, c := range candidates {
		if c.Variant != "standard" || !c.Rated {
			continue
		}
		if c.DaysPerTurn != pairing.DaysPerMove {
			continue
		}
		if c.WhiteLichessID != pairing.WhiteLichessID || c.BlackLichessID != pairing.BlackLichessID {
			continue
		}
		if c.CreatedAt.Before(cutoff) {
			continue
		}
		if takenGameIDs[c.ID] {
			continue
		}
		eligible = append(eligible, c)
	}

	switch len(eligible) {
	case 0:
		return Result{}
	case 1:
		return Result{Game: eligible[0].ID}
	default:
		return Result{Ambiguous: true}
	}
}
