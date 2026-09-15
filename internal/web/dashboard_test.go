package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
