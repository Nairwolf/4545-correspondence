package activity

import (
	"context"
	"fmt"
	"time"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
)

// Remind sends the 48-hour reminder (spec §6.4, §10) for every pairing
// still not started StartGrace after its round was published: to the
// player who must accept a challenge, or to both players of a game to
// be started by hand. The notice's key is the pairing's, so its players
// are reminded once however often this runs; sync-games calls it every
// hour, after it has looked for the games. It returns how many such
// pairings there are.
func Remind(ctx context.Context, q *gen.Queries, now time.Time) (int, error) {
	unstarted, err := q.ListUnstartedPairings(ctx, timestamp(now.Add(-StartGrace)))
	if err != nil {
		return 0, fmt.Errorf("list unstarted pairings: %w", err)
	}
	for _, p := range unstarted {
		for _, n := range reminders(p) {
			if err := notify.Send(ctx, q, n); err != nil {
				return 0, err
			}
		}
	}
	return len(unstarted), nil
}

// reminders words the 48-hour reminder of one pairing, for each player
// who can still start the game. Both players of a pairing are reminded
// under the pairing's key, which is unique per player.
func reminders(p gen.ListUnstartedPairingsRow) []notify.Notice {
	key := fmt.Sprintf("round:%d:unstarted:%s", p.RoundNumber, p.PairingID.String())

	if isChallenge(p) {
		m := missedStartOf(p)
		return []notify.Notice{{
			User:     m.user,
			Category: notify.Unstarted,
			Title:    fmt.Sprintf("Round %d: %s's challenge is waiting", m.round, m.challenger),
			Body: fmt.Sprintf(
				"Your round %d game against %s hasn't started: their challenge is still waiting for you to accept it on Lichess. "+
					"If it's still waiting when the next round is paired, the game is dropped and counts as a missed start.",
				m.round,
				m.challenger,
			),
			Link: rounds.GameURL(*p.LichessGameID),
			Key:  key,
		}}
	}

	snap, _ := rounds.ParseSnapshot(p.SettingsUsed)
	return []notify.Notice{
		byHandReminder(p, snap, "white", key),
		byHandReminder(p, snap, "black", key),
	}
}

// byHandReminder is the reminder to one player of a pairing whose game
// they start themselves. It names the game to start, as the round's
// first notice did: §7.3 matching only finds the right time control and
// colours.
func byHandReminder(
	p gen.ListUnstartedPairingsRow,
	snap rounds.Snapshot,
	colour string,
	key string,
) notify.Notice {
	user, opponent := p.WhiteUserID, p.BlackUsername
	if colour == "black" {
		user, opponent = p.BlackUserID, p.WhiteUsername
	}
	return notify.Notice{
		User:     user,
		Category: notify.Unstarted,
		Title:    fmt.Sprintf("Round %d: your game against %s hasn't started", p.RoundNumber, opponent),
		Body: fmt.Sprintf(
			"Your round %d game against %s hasn't started yet. Start it on Lichess as %s, you with %s; either of you can send the challenge. "+
				"If it still hasn't started when the next round is paired, it's dropped, with no penalty.",
			p.RoundNumber,
			opponent,
			rounds.TimeControl(snap),
			colour,
		),
		Link: "/account",
		Key:  key,
	}
}

// warningNotice tells a player one missed start from a pause (spec
// §6.4, §10). The player who had to accept a challenge is one whose
// own authorisation lapsed, so re-authorising is the way out.
func warningNotice(m missedStart) notify.Notice {
	return notify.Notice{
		User:     m.user,
		Category: notify.MissedStart,
		Title:    fmt.Sprintf("Round %d: your game against %s wasn't played", m.round, m.challenger),
		Body: fmt.Sprintf(
			"%s's challenge in round %d was never accepted, so it counts as a missed start. "+
				"If your next challenge goes unanswered too, your quest will be paused until you resume it. "+
				"Re-authorise from your account page and the league can create your games directly, with nothing to accept.",
			m.challenger,
			m.round,
		),
		Link: "/account",
		Key:  fmt.Sprintf("missed_start:%d", m.round),
	}
}

// pausedNotice tells a player the missed-start rule paused them (spec
// §6.4, §10), with the way back. It is account-critical: sent whatever
// their preferences.
func pausedNotice(m missedStart, streak int) notify.Notice {
	missed := fmt.Sprintf("%s's challenge in round %d went unanswered", m.challenger, m.round)
	if streak > 1 {
		missed = fmt.Sprintf("%d challenges in a row went unanswered, the last from %s in round %d", streak, m.challenger, m.round)
	}
	return notify.Notice{
		User:     m.user,
		Category: notify.AutoPause,
		Title:    "Your quest is paused",
		Body: missed + ", so you won't be paired until you resume your quest. " +
			"Resuming takes one click on your account page, and you're back from the next round. " +
			"If your Lichess authorisation has lapsed, re-authorise there too: the league can then create your games directly, with nothing to accept.",
		Link: "/account",
		Key:  fmt.Sprintf("auto_pause:%d", m.round),
	}
}
