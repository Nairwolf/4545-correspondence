// Package settings provides typed access to the runtime-editable
// Setting table (spec §4.2), with the spec's own defaults for any key
// that has no override row yet — so a fresh database behaves correctly
// and the (future) admin UI only ever needs to store what an admin
// actually changed.
//
// Phase 4 (2026-09-17) adds the pairing-engine keys that were
// deliberately left out of Phases 1-3: there is no settings UI yet
// (Phase 6), so a bad value typed directly into the table must fail
// the next generate-round run visibly rather than default silently —
// every numeric and enum key is validated as it is loaded.
package settings

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/scoring"
)

// PairingMode selects how a generated round is published (spec §4.2,
// §6.1).
type PairingMode string

const (
	// PairingModeReviewWindow generates a draft that publishes itself
	// after ReviewWindowHours unless an admin acts first. The default.
	PairingModeReviewWindow PairingMode = "review_window"
	// PairingModeAutoPublish generates and publishes a round in the
	// same job, with no draft state.
	PairingModeAutoPublish PairingMode = "auto_publish"
)

// OddPoolStrategy selects how the engine resolves an odd-sized pool
// (spec §4.2, §6.2 step 6).
type OddPoolStrategy string

const (
	// OddPoolDoubleThenBye prefers a double-game volunteer and falls
	// back to a bye only when none is eligible. The default.
	OddPoolDoubleThenBye OddPoolStrategy = "double_then_bye"
	// OddPoolByeOnly always gives a bye, skipping step 6a entirely.
	OddPoolByeOnly OddPoolStrategy = "bye_only"
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

	// PairingCron is pairing.cron (default "0 12 * * 1"): the schedule
	// generate-round runs on. Read once at server start-up; its syntax
	// is validated by the scheduler that parses it (internal/jobs), not
	// here, so this package needs no cron-parsing dependency.
	PairingCron string
	// PairingMode is pairing.mode (default review_window, spec §6.1).
	PairingMode PairingMode
	// ReviewWindowHours is pairing.review_window_hours (default 6): how
	// long a draft waits before it auto-publishes in review_window mode.
	ReviewWindowHours int
	// AvoidRecentRounds is pairing.avoid_recent_rounds (default 5): the
	// repeat-opponent window (spec §6.2 step 3).
	AvoidRecentRounds int
	// ColorWeight is pairing.color_weight (default 100): rating points
	// one unit of leftover colour imbalance is worth (spec §6.2 step 3).
	ColorWeight int
	// RepeatPenalty is pairing.repeat_penalty (default 1_000_000): the
	// large FINITE cost of a repeat opponent (spec §6.2 step 3,
	// CLAUDE.md) — never infinity, and never zero or negative, or a
	// repeat pairing stops being discouraged at all.
	RepeatPenalty int
	// OddPoolStrategy is pairing.odd_pool_strategy (default
	// double_then_bye, spec §6.2 step 6).
	OddPoolStrategy OddPoolStrategy
	// Rated is pairing.rated (default true): whether generated games
	// are rated on Lichess (spec §4.2; read by Phase 5's publish, and
	// snapshotted into Round.settings_used at generation).
	Rated bool
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

		PairingCron:       "0 12 * * 1",
		PairingMode:       PairingModeReviewWindow,
		ReviewWindowHours: 6,
		AvoidRecentRounds: 5,
		ColorWeight:       100,
		RepeatPenalty:     1_000_000,
		OddPoolStrategy:   OddPoolDoubleThenBye,
		Rated:             true,
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

	keyPairingCron       = "pairing.cron"
	keyPairingMode       = "pairing.mode"
	keyReviewWindowHours = "pairing.review_window_hours"
	keyAvoidRecentRounds = "pairing.avoid_recent_rounds"
	keyColorWeight       = "pairing.color_weight"
	keyRepeatPenalty     = "pairing.repeat_penalty"
	keyOddPoolStrategy   = "pairing.odd_pool_strategy"
	keyRated             = "pairing.rated"
)

// Load returns Defaults() with every stored override applied. An
// unrecognised key is ignored (forward-compatible with settings a
// future Phase adds) rather than treated as an error; a recognised key
// with a malformed or out-of-range value fails, naming the key, so a
// bad row in the settings table is caught at the next job run rather
// than silently substituting a default or an invalid value.
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

	case keyPairingCron:
		return json.Unmarshal(value, &s.PairingCron)
	case keyPairingMode:
		var mode string
		if err := json.Unmarshal(value, &mode); err != nil {
			return err
		}
		switch PairingMode(mode) {
		case PairingModeReviewWindow, PairingModeAutoPublish:
			s.PairingMode = PairingMode(mode)
			return nil
		default:
			return fmt.Errorf("must be %q or %q, got %q", PairingModeReviewWindow, PairingModeAutoPublish, mode)
		}
	case keyReviewWindowHours:
		return unmarshalNonNegative(value, &s.ReviewWindowHours)
	case keyAvoidRecentRounds:
		return unmarshalNonNegative(value, &s.AvoidRecentRounds)
	case keyColorWeight:
		return unmarshalNonNegative(value, &s.ColorWeight)
	case keyRepeatPenalty:
		var v int
		if err := json.Unmarshal(value, &v); err != nil {
			return err
		}
		if v <= 0 {
			return fmt.Errorf("must be a large positive number, got %d (CLAUDE.md: finite, never zero or infinite)", v)
		}
		s.RepeatPenalty = v
		return nil
	case keyOddPoolStrategy:
		var strategy string
		if err := json.Unmarshal(value, &strategy); err != nil {
			return err
		}
		switch OddPoolStrategy(strategy) {
		case OddPoolDoubleThenBye, OddPoolByeOnly:
			s.OddPoolStrategy = OddPoolStrategy(strategy)
			return nil
		default:
			return fmt.Errorf("must be %q or %q, got %q", OddPoolDoubleThenBye, OddPoolByeOnly, strategy)
		}
	case keyRated:
		return json.Unmarshal(value, &s.Rated)

	default:
		return nil // unknown key: not this package's concern yet
	}
}

// unmarshalNonNegative decodes value into *dst and rejects a negative
// number — shared by the three pairing settings that must be >= 0
// (review window hours, the repeat-avoidance window, the colour
// weight).
func unmarshalNonNegative(value []byte, dst *int) error {
	var v int
	if err := json.Unmarshal(value, &v); err != nil {
		return err
	}
	if v < 0 {
		return fmt.Errorf("must be >= 0, got %d", v)
	}
	*dst = v
	return nil
}
