package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyOverride(t *testing.T) {
	s := Defaults()

	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	require(applyOverride(&s, keyLastK, []byte(`10`)))
	require(applyOverride(&s, keyMinGamesForPerf, []byte(`3`)))
	require(applyOverride(&s, keyXPWin, []byte(`5`)))
	require(applyOverride(&s, keyUnratedDefault, []byte(`1400`)))
	require(applyOverride(&s, keyDaysPerMove, []byte(`3`)))
	require(applyOverride(&s, keyMaxConcurrentCeiling, []byte(`8`)))
	require(applyOverride(&s, "some.unknown.key", []byte(`"ignored"`)))

	assert.Equal(t, 10, s.LastK)
	assert.Equal(t, 3, s.MinGamesForPerf)
	assert.Equal(t, 5, s.XPWeights.Win)
	assert.Equal(t, 2, s.XPWeights.Draw) // untouched, still default
	assert.Equal(t, 1400, s.UnratedDefault)
	assert.Equal(t, 3, s.DaysPerMove)
	assert.Equal(t, 8, s.MaxConcurrentCeiling)
}

func TestDefaults_MatchSpecSection4_2(t *testing.T) {
	d := Defaults()
	assert.Equal(t, 5, d.LastK)
	assert.Equal(t, 5, d.MinGamesForPerf)
	assert.Equal(t, 3, d.XPWeights.Win)
	assert.Equal(t, 2, d.XPWeights.Draw)
	assert.Equal(t, 1, d.XPWeights.Loss)
	assert.Equal(t, 1500, d.UnratedDefault)
	assert.Equal(t, 2, d.DaysPerMove)
	assert.Equal(t, 20, d.MaxConcurrentCeiling)
}
