package matching

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

var pairAt = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func testPairing() Pairing {
	return Pairing{
		WhiteLichessID: "whiteplayer",
		BlackLichessID: "blackplayer",
		PairAt:         pairAt,
		DaysPerMove:    2,
	}
}

func eligibleCandidate() Candidate {
	return Candidate{
		ID:             "game001",
		Variant:        "standard",
		Rated:          true,
		DaysPerTurn:    2,
		WhiteLichessID: "whiteplayer",
		BlackLichessID: "blackplayer",
		CreatedAt:      pairAt,
	}
}

func TestMatch_ExactlyOneCandidateMatches(t *testing.T) {
	result := Match(testPairing(), []Candidate{eligibleCandidate()}, nil)
	assert.Equal(t, "game001", result.Game)
	assert.False(t, result.Ambiguous)
}

func TestMatch_NoCandidatesLeavesItPending(t *testing.T) {
	result := Match(testPairing(), nil, nil)
	assert.Empty(t, result.Game)
	assert.False(t, result.Ambiguous)
}

func TestMatch_ReversedColoursIsNotACandidate(t *testing.T) {
	c := eligibleCandidate()
	c.WhiteLichessID, c.BlackLichessID = c.BlackLichessID, c.WhiteLichessID
	result := Match(testPairing(), []Candidate{c}, nil)
	assert.Empty(t, result.Game)
	assert.False(t, result.Ambiguous)
}

func TestMatch_WrongDaysPerTurnIsNotACandidate(t *testing.T) {
	c := eligibleCandidate()
	c.DaysPerTurn = 3
	result := Match(testPairing(), []Candidate{c}, nil)
	assert.Empty(t, result.Game)
}

func TestMatch_CreatedBeforeCutoffIsNotACandidate(t *testing.T) {
	c := eligibleCandidate()
	c.CreatedAt = pairAt.Add(-25 * time.Hour) // just past the 24h margin
	result := Match(testPairing(), []Candidate{c}, nil)
	assert.Empty(t, result.Game)
}

func TestMatch_CreatedWithinOneDayMarginIsStillACandidate(t *testing.T) {
	c := eligibleCandidate()
	c.CreatedAt = pairAt.Add(-23 * time.Hour) // within the margin
	result := Match(testPairing(), []Candidate{c}, nil)
	assert.Equal(t, "game001", result.Game)
}

func TestMatch_TwoCandidatesIsAmbiguous(t *testing.T) {
	c1 := eligibleCandidate()
	c2 := eligibleCandidate()
	c2.ID = "game002"
	result := Match(testPairing(), []Candidate{c1, c2}, nil)
	assert.Empty(t, result.Game)
	assert.True(t, result.Ambiguous)
}

func TestMatch_GameAlreadyAttachedToAnotherPairingIsSkipped(t *testing.T) {
	c := eligibleCandidate()
	result := Match(testPairing(), []Candidate{c}, map[string]bool{"game001": true})
	assert.Empty(t, result.Game)
	assert.False(t, result.Ambiguous)
}

func TestMatch_CasualGameIsIgnored(t *testing.T) {
	c := eligibleCandidate()
	c.Rated = false
	result := Match(testPairing(), []Candidate{c}, nil)
	assert.Empty(t, result.Game)
}

func TestMatch_WrongVariantIsIgnored(t *testing.T) {
	c := eligibleCandidate()
	c.Variant = "chess960"
	result := Match(testPairing(), []Candidate{c}, nil)
	assert.Empty(t, result.Game)
}

func TestMatch_TakenGameAmongMultipleLeavesOneEligible(t *testing.T) {
	// A realistic mix: one candidate already belongs to another pairing,
	// one is genuinely eligible — exactly one match, not ambiguous.
	taken := eligibleCandidate()
	taken.ID = "taken001"
	fresh := eligibleCandidate()
	fresh.ID = "fresh001"

	result := Match(testPairing(), []Candidate{taken, fresh}, map[string]bool{"taken001": true})
	assert.Equal(t, "fresh001", result.Game)
	assert.False(t, result.Ambiguous)
}
