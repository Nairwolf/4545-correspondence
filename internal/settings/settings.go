// Package settings provides typed access to the runtime-editable
// Setting table (spec §4.2), with the spec's own defaults for any key
// that has no override row yet — so a fresh database behaves correctly
// and the (future) admin UI only ever needs to store what an admin
// actually changed.
//
// Only the keys something actually reads are included: the scoring
// inputs (Phase 1, used by internal/standings) and the dashboard's
// capacity ceiling (Phase 3). The pairing-engine-only keys
// (color_weight, repeat_penalty, odd_pool_strategy, ...) are added when
// Phase 4 builds their first consumer, not speculatively now.
package settings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/scoring"
)

// Settings is the resolved configuration: defaults overridden by
// whatever rows exist in the settings table.
type Settings struct {
	// LastK is pairing.last_k (default 5): the rolling window size for
	// performance rating (spec §5.2).
	LastK int
	// MinGamesForPerf is pairing.min_games_for_perf (default 5): below
	// this many finished games, power rating falls back to base rating
	// (spec §5.4).
	MinGamesForPerf int
	// XPWeights is xp.win / xp.draw / xp.loss (default 3/2/1, spec §5.6).
	XPWeights scoring.XPWeights
	// UnratedDefault is rating.unrated_default (default 1500): the base
	// rating substituted for a player with neither a correspondence nor
	// a classical rating (spec §5.1).
	UnratedDefault int
	// DaysPerMove is pairing.days_per_move (default 2): the
	// correspondence time control every league game uses, and one of
	// the filters a candidate game must match (spec §7.3).
	DaysPerMove int
	// MaxConcurrentCeiling is player.max_concurrent_ceiling (default
	// 20): the highest cap a player may set on their dashboard, if they
	// set one at all (spec §4.2, §8.3). It bounds the input, not the
	// default — the default stays unlimited (NULL, spec §5.8).
	MaxConcurrentCeiling int
}

// Defaults are the spec §4.2 values, for a database with no override
// rows and for tests that don't need to exercise overrides.
func Defaults() Settings {
	return Settings{
		LastK:           5,
		MinGamesForPerf: 5,
		XPWeights:       scoring.DefaultXPWeights,
		UnratedDefault:  1500,
		DaysPerMove:     2,

		MaxConcurrentCeiling: 20,
	}
}

// key names as stored in the settings table (spec §4.2).
const (
	keyLastK           = "pairing.last_k"
	keyMinGamesForPerf = "pairing.min_games_for_perf"
	keyXPWin           = "xp.win"
	keyXPDraw          = "xp.draw"
	keyXPLoss          = "xp.loss"
	keyUnratedDefault  = "rating.unrated_default"
	keyDaysPerMove     = "pairing.days_per_move"

	keyMaxConcurrentCeiling = "player.max_concurrent_ceiling"
)

// Load returns Defaults() with every stored override applied. An
// unrecognised key is ignored (forward-compatible with settings a
// future Phase adds) rather than treated as an error.
func Load(ctx context.Context, q *gen.Queries) (Settings, error) {
	s := Defaults()
	rows, err := q.ListSettings(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("settings: list: %w", err)
	}
	for _, row := range rows {
		if err := applyOverride(&s, row.Key, row.Value); err != nil {
			return Settings{}, fmt.Errorf("settings: %s: %w", row.Key, err)
		}
	}
	return s, nil
}

func applyOverride(s *Settings, key string, value []byte) error {
	switch key {
	case keyLastK:
		return json.Unmarshal(value, &s.LastK)
	case keyMinGamesForPerf:
		return json.Unmarshal(value, &s.MinGamesForPerf)
	case keyXPWin:
		return json.Unmarshal(value, &s.XPWeights.Win)
	case keyXPDraw:
		return json.Unmarshal(value, &s.XPWeights.Draw)
	case keyXPLoss:
		return json.Unmarshal(value, &s.XPWeights.Loss)
	case keyUnratedDefault:
		return json.Unmarshal(value, &s.UnratedDefault)
	case keyDaysPerMove:
		return json.Unmarshal(value, &s.DaysPerMove)
	case keyMaxConcurrentCeiling:
		return json.Unmarshal(value, &s.MaxConcurrentCeiling)
	default:
		return nil // unknown key: not this package's concern yet
	}
}
