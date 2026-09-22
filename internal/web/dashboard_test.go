package web

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

func TestExclusionSentence(t *testing.T) {
	four, five := int32(4), int32(5)
	one := int32(1)
	tests := []struct {
		name     string
		exc      gen.RoundExclusion
		sentence string
	}{
		{"at capacity", gen.RoundExclusion{Reason: gen.ExclusionReasonAtCapacity, OngoingGames: &four, MaxConcurrentGames: &four},
			"You had 4 games in progress and your limit is 4, so you sat out round 170. No penalty — you're back as soon as a game finishes."},
		{"over capacity after lowering the cap", gen.RoundExclusion{Reason: gen.ExclusionReasonAtCapacity, OngoingGames: &five, MaxConcurrentGames: &four},
			"You had 5 games in progress and your limit is 4, so you sat out round 170. No penalty — you're back as soon as a game finishes."},
		{"at capacity, one game", gen.RoundExclusion{Reason: gen.ExclusionReasonAtCapacity, OngoingGames: &one, MaxConcurrentGames: &one},
			"You had 1 game in progress and your limit is 1, so you sat out round 170. No penalty — you're back as soon as a game finishes."},
		{"at capacity without the numbers", gen.RoundExclusion{Reason: gen.ExclusionReasonAtCapacity},
			"You were at your games-at-once limit, so you sat out round 170. No penalty — you're back as soon as a game finishes."},
		{"inactive", gen.RoundExclusion{Reason: gen.ExclusionReasonInactive},
			"Your quest was paused, so you sat out round 170."},
		{"paused by an admin", gen.RoundExclusion{Reason: gen.ExclusionReasonPaused},
			"An admin had paused your quest, so you sat out round 170."},
		{"auto-paused", gen.RoundExclusion{Reason: gen.ExclusionReasonAutoPaused},
			"Your quest was paused after unanswered challenges, so you sat out round 170."},
		{"removed by an admin", gen.RoundExclusion{Reason: gen.ExclusionReasonRemovedByAdmin},
			"An admin removed your pairing for round 170."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.sentence, exclusionSentence(tt.exc, 170))
		})
	}
}

func TestByeSentence(t *testing.T) {
	assert.Equal(t,
		"Odd number of players this week and nobody was free for a double game, so you sat out. You're first in line to avoid the next one.",
		byeSentence(settings.OddPoolDoubleThenBye))
	assert.Equal(t,
		"Odd number of players this week, so you sat out. You're first in line to avoid the next one.",
		byeSentence(settings.OddPoolByeOnly))
}

func TestOddPoolStrategyOf(t *testing.T) {
	assert.Equal(t, settings.OddPoolByeOnly,
		oddPoolStrategyOf(gen.Round{SettingsUsed: []byte(`{"odd_pool_strategy":"bye_only"}`)}))
	assert.Equal(t, settings.OddPoolDoubleThenBye,
		oddPoolStrategyOf(gen.Round{SettingsUsed: []byte(`{"odd_pool_strategy":"double_then_bye"}`)}))
	assert.Equal(t, settings.OddPoolDoubleThenBye, oddPoolStrategyOf(gen.Round{}), "imported round: no snapshot")
}

func TestCapacityFor(t *testing.T) {
	four := int32(4)
	tests := []struct {
		name     string
		ongoing  int
		cap      *int32
		sentence string
		over     bool
	}{
		{"unlimited", 6, nil, "6 games in progress — no limit set, you'll be paired every week.", false},
		{"unlimited, one game", 1, nil, "1 game in progress — no limit set, you'll be paired every week.", false},
		{"under cap", 3, &four, "3 of 4 games in progress — you'll be paired this week.", false},
		{"at cap", 4, &four, "4 of 4 games in progress — you'll be skipped until one finishes.", false},
		{"over cap", 6, &four, "6 of 4 games in progress — you'll be skipped until one finishes.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := capacityFor(tt.ongoing, tt.cap)
			assert.Equal(t, tt.sentence, v.Sentence)
			assert.Equal(t, tt.over, v.OverCap)
			assert.Equal(t, tt.cap != nil, v.Limited)
		})
	}
}
