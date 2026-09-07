// Package scoring implements the league's rating, performance and XP/level
// logic from spec §5, as a pure, side-effect-free module: no database, no
// HTTP, no clock reads. Every function takes plain values and returns plain
// values, which is what makes it possible to unit-test against known-good
// values and to reuse the same logic in both the sync job and the pairing
// engine without either depending on the other.
package scoring

import (
	"math"
	"sort"
	"time"
)

// UserID identifies a player. Scoring never looks anything up by it — it
// only appears on FinishedGame for the caller's own bookkeeping — so a
// plain string (e.g. a stringified UUID) is all this package needs.
type UserID string

// Result is the outcome of a finished game from one player's point of
// view. FinishedGame is always expressed relative to a single player, so
// there is no separate "white_win/black_win" distinction here — the
// caller resolves that against PlayedWhite before constructing the value.
type Result int

const (
	Loss Result = iota
	Draw
	Win
)

// FinishedGame is one completed league game, already resolved to a single
// player's perspective. OpponentRatingAtGame is that opponent's rating
// AT THE TIME the game was played, not their current rating (spec §5.2 —
// a deliberate divergence from the spreadsheet, decided so that every
// standing stays reproducible from the games table alone).
type FinishedGame struct {
	OpponentID           UserID
	OpponentRatingAtGame int
	PlayedWhite          bool
	Result               Result
	FinishedAt           time.Time
}

// Rating is one of a player's Lichess perf ratings (correspondence or
// classical), as input to BaseRating.
type Rating struct {
	Value       int
	Provisional bool
}

// BaseRating implements the corrected spec §5.1 rule: any correspondence
// rating — provisional or not — is used ahead of classical, which is
// consulted only as a bootstrap for a newcomer with no correspondence
// rating at all. This is a deliberate correction of the spreadsheet's
// MAX(correspondence, classical), which permanently inflated any player
// whose classical rating exceeded their correspondence rating.
// Provisional status plays no role beyond being one of the two things
// that can be entirely missing; unrated is true only when neither
// rating exists.
func BaseRating(correspondence, classical *Rating) (value int, unrated bool) {
	switch {
	case correspondence != nil:
		return correspondence.Value, false
	case classical != nil:
		return classical.Value, false
	default:
		return 0, true
	}
}

// fideDeltaTable is the FIDE "rating difference dp" table, indexed by
// score percentage in 10% steps from 0% to 100% (spec §5.3). It is a
// standard chess-rating table, not specific to this league.
var fideDeltaTable = [11]float64{-800, -366, -240, -149, -72, 0, 72, 149, 240, 366, 800}

// PerfDelta returns the FIDE performance-rating delta for a score out of
// windowSize games. The table is indexed by score PERCENTAGE (spec §5.3)
// and linearly interpolated between its 10% steps, so it stays correct
// for any windowSize — indexing by raw score instead would silently
// break the moment the configured last-k window size changes. A
// percentage outside [0,1] (which should not happen for a well-formed
// score) is clamped to the nearest endpoint rather than extrapolated,
// matching the FIDE table's own ±800 cap.
func PerfDelta(score float64, windowSize int) float64 {
	if windowSize <= 0 {
		panic("scoring: PerfDelta: windowSize must be positive")
	}
	p := score / float64(windowSize)
	switch {
	case p <= 0:
		return fideDeltaTable[0]
	case p >= 1:
		return fideDeltaTable[10]
	}
	scaled := p * 10
	lo := int(math.Floor(scaled))
	hi := lo + 1
	frac := scaled - float64(lo)
	return fideDeltaTable[lo] + frac*(fideDeltaTable[hi]-fideDeltaTable[lo])
}

// LastK returns the player's k most recently finished games (spec §5.2),
// ordered by FinishedAt descending, capped at k games. It sorts its own
// copy of games rather than trusting caller order, and returns fewer than
// k games — down to zero — for a player who hasn't played that many yet;
// callers must handle a short or empty window rather than treat it as an
// error.
func LastK(games []FinishedGame, k int) []FinishedGame {
	if k <= 0 {
		return nil
	}
	sorted := make([]FinishedGame, len(games))
	copy(sorted, games)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].FinishedAt.After(sorted[j].FinishedAt)
	})
	if len(sorted) > k {
		sorted = sorted[:k]
	}
	return sorted
}

// LastKScore is the player's score within a last-k window (spec §5.2):
// wins count 1, draws count 0.5, losses count 0. It is meaningless on an
// unwindowed game slice — callers pass the result of LastK.
func LastKScore(window []FinishedGame) float64 {
	var score float64
	for _, g := range window {
		switch g.Result {
		case Win:
			score++
		case Draw:
			score += 0.5
		}
	}
	return score
}

// PerfRating computes the player's rolling performance rating (spec
// §5.3): the average opponent rating within their last-k window, plus
// the FIDE delta for the score they achieved in it. ok is false only when
// the window is empty (a player with zero finished games), in which case
// perf is meaningless and callers fall back to the base rating (§5.4).
func PerfRating(games []FinishedGame, k int) (perf float64, ok bool) {
	window := LastK(games, k)
	if len(window) == 0 {
		return 0, false
	}
	var sumOpponentRating int
	for _, g := range window {
		sumOpponentRating += g.OpponentRatingAtGame
	}
	avgOpponentRating := float64(sumOpponentRating) / float64(len(window))
	delta := PerfDelta(LastKScore(window), len(window))
	return avgOpponentRating + delta, true
}

// PowerRating implements spec §5.4: use the rolling performance rating
// once the player has both a computable one (perfOK) and at least
// minGamesForPerf finished league games; otherwise fall back to the base
// rating. This is the value the pairing engine sorts and matches on.
func PowerRating(perf float64, perfOK bool, base int, gamesPlayed, minGamesForPerf int) int {
	if !perfOK || gamesPlayed < minGamesForPerf {
		return base
	}
	return int(math.Round(perf))
}

// ColorScore is (games played as white) − (games played as black) across
// all of the player's games, not just their last-k window (spec §5.5).
// Positive means the player has had white too often and is due black.
func ColorScore(games []FinishedGame) int {
	var score int
	for _, g := range games {
		if g.PlayedWhite {
			score++
		} else {
			score--
		}
	}
	return score
}

// XPWeights are the configurable per-result XP awards (settings
// xp.win / xp.draw / xp.loss, spec §4.2).
type XPWeights struct {
	Win  int
	Draw int
	Loss int
}

// DefaultXPWeights are the spec §4.2 defaults, for tests and for any
// caller that hasn't loaded the Setting overrides yet.
var DefaultXPWeights = XPWeights{Win: 3, Draw: 2, Loss: 1}

// XP sums the XP earned across all of a player's finished games (spec
// §5.6). Every completed game awards something, even a loss — this is
// deliberate: it rewards participation, so a player who plays constantly
// climbs levels regardless of results.
func XP(games []FinishedGame, weights XPWeights) int {
	var xp int
	for _, g := range games {
		switch g.Result {
		case Win:
			xp += weights.Win
		case Draw:
			xp += weights.Draw
		case Loss:
			xp += weights.Loss
		}
	}
	return xp
}

// Level implements spec §5.6: level = floor(sqrt(xp)), and the XP still
// needed to reach the next level. math.Sqrt is correctly rounded and xp
// values in this league are tiny relative to float64's exact-integer
// range, so int() truncation of the float result is exact here — no
// integer-arithmetic correction needed.
func Level(xp int) (level, xpToNextLevel int) {
	if xp < 0 {
		xp = 0
	}
	level = int(math.Sqrt(float64(xp)))
	xpToNextLevel = (level+1)*(level+1) - xp
	return level, xpToNextLevel
}

// CapacityAllows implements spec §5.8. maxConcurrent == nil means
// UNLIMITED — the default for every player — and must never be treated
// as zero. A NULL-to-0 coercion bug here would silently exclude the
// entire default population from every pairing round while the engine
// appeared to run successfully; spec §11 requires this exact case to
// have a dedicated test, so do not "simplify" this function to drop the
// nil check.
func CapacityAllows(maxConcurrent *int, ongoing int) bool {
	if maxConcurrent == nil {
		return true
	}
	return ongoing < *maxConcurrent
}
