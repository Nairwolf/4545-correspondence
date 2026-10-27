package activity

import "time"

// MissedStart is one recorded missed start: when evaluate-activity
// recorded it, and the round whose challenge went unanswered.
type MissedStart struct {
	At    time.Time
	Round int32
}

// Streak returns the player's missed starts that are still consecutive
// (spec §6.4): those recorded after both their latest started league
// game and their latest resume. Any started game, and resuming, ends a
// run. The zero time means "never". missed is oldest first, and so is
// the result: its last element is the latest miss.
func Streak(missed []MissedStart, lastGameStarted, resumed time.Time) []MissedStart {
	since := lastGameStarted
	if resumed.After(since) {
		since = resumed
	}
	for i, m := range missed {
		if m.At.After(since) {
			return missed[i:]
		}
	}
	return nil
}

// Action is what a run of missed starts leads to.
type Action int

const (
	// None: nothing to say yet.
	None Action = iota
	// Warn: one more missed start would pause the player.
	Warn
	// Pause: the player is auto-paused.
	Pause
)

// Decide applies activity.missed_starts_to_pause to a streak of the
// given length (spec §6.4): at pauseAfter the player is paused, and
// one before it they are warned. A threshold of 1 pauses at the first
// miss, with no warning: there is no earlier miss to warn at.
func Decide(streak, pauseAfter int) Action {
	switch {
	case streak >= pauseAfter:
		return Pause
	case streak >= 1 && streak == pauseAfter-1:
		return Warn
	}
	return None
}
