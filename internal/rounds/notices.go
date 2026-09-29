package rounds

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// ByeSentence is spec §8.3's bye wording, shared by the dashboard and
// the bye notice. It must never read as a penalty: a bye goes by
// rotation alone, so the player who just had one is the last in line
// for the next.
func ByeSentence(strategy settings.OddPoolStrategy) string {
	if strategy == settings.OddPoolByeOnly {
		return "Odd number of players this week, so you sat out. You're first in line to avoid the next one."
	}
	return "Odd number of players this week and nobody was free for a double game, so you sat out. You're first in line to avoid the next one."
}

// notifyPublished tells each player of a round that has just been
// published what it means for them (spec §10): their pairing, their
// two games if they absorbed the odd pool, or their bye. It runs in the
// publishing transaction, so the notices exist exactly when the round
// does. A player excluded for another reason (at capacity, paused) gets
// no notice: nothing happened to them that the dashboard doesn't
// already say.
//
// The wording is for games players start by hand, the only kind until
// the league creates games itself.
func notifyPublished(ctx context.Context, q *gen.Queries, round gen.Round) error {
	snap, _ := SnapshotOf(round)

	pairings, err := q.ListPairingsForRound(ctx, round.ID)
	if err != nil {
		return fmt.Errorf("list pairings of round %d: %w", round.Number, err)
	}
	doubles, err := q.ListDoubleGamesForRound(ctx, round.ID)
	if err != nil {
		return fmt.Errorf("list double games of round %d: %w", round.Number, err)
	}
	byes, err := q.ListByesForRound(ctx, round.ID)
	if err != nil {
		return fmt.Errorf("list byes of round %d: %w", round.Number, err)
	}

	// The volunteer gets one notice explaining both games rather than
	// one per pairing; their opponents get the ordinary one.
	volunteerGames := make(map[pgtype.UUID][]string, len(doubles))
	for _, d := range doubles {
		volunteerGames[d.UserID] = nil
	}

	for _, p := range pairings {
		if p.Status == gen.PairingStatusFailed || p.Status == gen.PairingStatusCancelled {
			continue
		}
		sides := []struct {
			user     pgtype.UUID
			colour   string
			opponent string
		}{
			{p.WhiteUserID, "white", p.BlackUsername},
			{p.BlackUserID, "black", p.WhiteUsername},
		}
		for _, side := range sides {
			if games, ok := volunteerGames[side.user]; ok {
				game := fmt.Sprintf("with %s against %s", side.colour, side.opponent)
				if side.colour == "white" {
					// White first, whatever order the pairings are in.
					volunteerGames[side.user] = append([]string{game}, games...)
				} else {
					volunteerGames[side.user] = append(games, game)
				}
				continue
			}
			err := notify.Send(ctx, q, notify.Notice{
				User:     side.user,
				Category: notify.Round,
				Title: fmt.Sprintf(
					"Round %d: you play %s with %s",
					round.Number,
					side.opponent,
					side.colour,
				),
				Body: fmt.Sprintf(
					"Start it on Lichess as %s, you with %s. Either of you can send the challenge; the site finds the game by itself once it begins.",
					timeControl(snap),
					side.colour,
				),
				Link: "/account",
				Key:  fmt.Sprintf("round:%d:paired:%s", round.Number, p.ID.String()),
			})
			if err != nil {
				return err
			}
		}
	}

	// Removing one of the volunteer's games from the draft also drops
	// their double_games row, so each volunteer here has two games. The
	// notice is still worded from the games found rather than assuming
	// it: a notice must never be what stops a round being published.
	for _, d := range doubles {
		games := volunteerGames[d.UserID]
		if len(games) == 0 {
			continue
		}
		err := notify.Send(ctx, q, notify.Notice{
			User:     d.UserID,
			Category: notify.Round,
			Title:    fmt.Sprintf("Round %d: %d games this week", round.Number, len(games)),
			Body: fmt.Sprintf(
				"Odd number of players this week, so you're playing %d games: %s. Start each on Lichess as %s; the site finds them by itself once they begin.",
				len(games),
				strings.Join(games, " and "),
				timeControl(snap),
			),
			Link: "/account",
			Key:  fmt.Sprintf("round:%d:double", round.Number),
		})
		if err != nil {
			return err
		}
	}

	for _, b := range byes {
		err := notify.Send(ctx, q, notify.Notice{
			User:     b.UserID,
			Category: notify.Round,
			Title:    fmt.Sprintf("Round %d: you sit out this week", round.Number),
			Body:     ByeSentence(snap.OddPoolStrategy),
			Link:     "/account",
			Key:      fmt.Sprintf("round:%d:bye", round.Number),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// timeControl names the game to start, from the settings the round was
// generated under. A round without a snapshot (never published through
// here) falls back to the plain words.
func timeControl(snap Snapshot) string {
	if snap.DaysPerMove == 0 {
		return "a correspondence game"
	}
	kind := "casual"
	if snap.Rated {
		kind = "rated"
	}
	days := "days"
	if snap.DaysPerMove == 1 {
		days = "day"
	}
	return fmt.Sprintf("a %s correspondence game at %d %s per move", kind, snap.DaysPerMove, days)
}
