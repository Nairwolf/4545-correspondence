package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyOverride(t *testing.T) {
	s := Defaults()

	requireNoError := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	requireNoError(applyOverride(&s, keyLastK, []byte(`10`)))
	requireNoError(applyOverride(&s, keyMinGamesForPerf, []byte(`3`)))
	requireNoError(applyOverride(&s, keyXPWin, []byte(`5`)))
	requireNoError(applyOverride(&s, keyUnratedDefault, []byte(`1400`)))
	requireNoError(applyOverride(&s, keyDaysPerMove, []byte(`3`)))
	requireNoError(applyOverride(&s, keyMaxConcurrentCeiling, []byte(`8`)))
	requireNoError(applyOverride(&s, keyPairingCron, []byte(`"0 12 * * 2"`)))
	requireNoError(applyOverride(&s, keyPairingMode, []byte(`"auto_publish"`)))
	requireNoError(applyOverride(&s, keyReviewWindowHours, []byte(`12`)))
	requireNoError(applyOverride(&s, keyAvoidRecentRounds, []byte(`3`)))
	requireNoError(applyOverride(&s, keyColorWeight, []byte(`50`)))
	requireNoError(applyOverride(&s, keyRepeatPenalty, []byte(`2000000`)))
	requireNoError(applyOverride(&s, keyOddPoolStrategy, []byte(`"bye_only"`)))
	requireNoError(applyOverride(&s, keyRated, []byte(`false`)))
	requireNoError(applyOverride(&s, "some.unknown.key", []byte(`"ignored"`)))

	assert.Equal(t, 10, s.LastK)
	assert.Equal(t, 3, s.MinGamesForPerf)
	assert.Equal(t, 5, s.XPWeights.Win)
	assert.Equal(t, 2, s.XPWeights.Draw) // untouched, still default
	assert.Equal(t, 1400, s.UnratedDefault)
	assert.Equal(t, 3, s.DaysPerMove)
	assert.Equal(t, 8, s.MaxConcurrentCeiling)
	assert.Equal(t, "0 12 * * 2", s.PairingCron)
	assert.Equal(t, PairingModeAutoPublish, s.PairingMode)
	assert.Equal(t, 12, s.ReviewWindowHours)
	assert.Equal(t, 3, s.AvoidRecentRounds)
	assert.Equal(t, 50, s.ColorWeight)
	assert.Equal(t, 2_000_000, s.RepeatPenalty)
	assert.Equal(t, OddPoolByeOnly, s.OddPoolStrategy)
	assert.False(t, s.Rated)
}

func TestApplyOverride_RejectsInvalidPairingValues(t *testing.T) {
	// There is no settings UI until Phase 6, so a bad row typed
	// directly into the table (make psql, or a stray ic setting call)
	// must fail loudly, naming the key, rather than silently keeping a
	// default or accepting a value that breaks the engine's invariants.
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"mode: unknown value", keyPairingMode, `"shadow"`},
		{"mode: wrong type", keyPairingMode, `3`},
		{"review window: negative", keyReviewWindowHours, `-1`},
		{"avoid recent rounds: negative", keyAvoidRecentRounds, `-1`},
		{"color weight: negative", keyColorWeight, `-1`},
		{"repeat penalty: zero", keyRepeatPenalty, `0`},
		{"repeat penalty: negative", keyRepeatPenalty, `-1000000`},
		{"odd pool strategy: unknown value", keyOddPoolStrategy, `"draft_only"`},
		{"rated: wrong type", keyRated, `"true"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Defaults()
			err := applyOverride(&s, tc.key, []byte(tc.value))
			require.Error(t, err)
		})
	}
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

	assert.Equal(t, "0 12 * * 1", d.PairingCron)
	assert.Equal(t, PairingModeReviewWindow, d.PairingMode)
	assert.Equal(t, 6, d.ReviewWindowHours)
	assert.Equal(t, 5, d.AvoidRecentRounds)
	assert.Equal(t, 100, d.ColorWeight)
	assert.Equal(t, 1_000_000, d.RepeatPenalty)
	assert.Equal(t, OddPoolDoubleThenBye, d.OddPoolStrategy)
	assert.True(t, d.Rated)
}
