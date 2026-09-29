package notify

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// allCategories is every category spec §10's table names.
var allCategories = []Category{
	Registration, Round, Challenge, Unstarted, MissedStart, AutoPause, Token, LevelUp,
}

func TestCategories_EachIsCriticalOrOptOut(t *testing.T) {
	// Spec §10: every player-facing category can be turned off except
	// the account-critical ones. A category that is neither could never
	// be silenced and would not be listed on the dashboard.
	for _, c := range allCategories {
		_, optOut := ParseOptOut(string(c))
		assert.True(t, c.Critical() != optOut, "%s must be exactly one of critical or opt-out", c)
		assert.True(t, c.known(), "%s", c)
	}
	assert.Len(t, OptOuts, len(allCategories)-3)
}

func TestCategories_CriticalOnesAreThoseOfTheSpec(t *testing.T) {
	assert.True(t, Registration.Critical())
	assert.True(t, AutoPause.Critical())
	assert.True(t, Token.Critical())
	assert.False(t, Round.Critical())
	assert.False(t, LevelUp.Critical())
}

func TestParseOptOut(t *testing.T) {
	c, ok := ParseOptOut("level_up")
	assert.True(t, ok)
	assert.Equal(t, LevelUp, c)

	_, ok = ParseOptOut("token")
	assert.False(t, ok, "a critical category cannot be turned off")
	_, ok = ParseOptOut("nonsense")
	assert.False(t, ok)
	assert.False(t, Category("nonsense").known())
}
