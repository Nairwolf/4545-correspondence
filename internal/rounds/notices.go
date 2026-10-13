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
// When the site creates the games (the round is "creating"), a paired
// player is not told yet: create-games tells them once it knows how
// their game was made — created, or to be started by hand. The bye and
// the double game are told now, as neither depends on it.
func notifyPublished(ctx context.Context, q *gen.Queries, round gen.Round) error {
	snap, _ := SnapshotOf(round)
	creating := round.GameCreation == gen.GameCreationStateCreating

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
		for _, side := range sidesOf(p.WhiteUserID, p.BlackUserID, p.WhiteUsername, p.BlackUsername) {
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
			if creating {
				continue
			}
			if err := notify.Send(ctx, q, byHandNotice(round, snap, p.ID, side, startedByHand)); err != nil {
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
		next := fmt.Sprintf("Start each on Lichess as %s; the site finds them by itself once they begin.", timeControl(snap))
		if creating {
			next = "The league is creating them on Lichess; you'll be told when each is ready."
		}
		err := notify.Send(ctx, q, notify.Notice{
			User:     d.UserID,
			Category: notify.Round,
			Title:    fmt.Sprintf("Round %d: %d games this week", round.Number, len(games)),
			Body: fmt.Sprintf(
				"Odd number of players this week, so you're playing %d games: %s. %s",
				len(games),
				strings.Join(games, " and "),
				next,
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

// side is one player of a pairing, as a notice addresses them.
type side struct {
	user     pgtype.UUID
	colour   string
	opponent string
}

func sidesOf(white, black pgtype.UUID, whiteName, blackName string) [2]side {
	return [2]side{
		{user: white, colour: "white", opponent: blackName},
		{user: black, colour: "black", opponent: whiteName},
	}
}

// byHandReason is why a player must start their game themselves.
type byHandReason int

const (
	// startedByHand: the round was published under
	// pairing.game_creation = manual, as every round before Phase 5.
	startedByHand byHandReason = iota
	// notCreated: the league tried and could not create it — no working
	// token on either side, or Lichess kept refusing.
	notCreated
)

// byHandNotice is a pairing's notice (spec §10) when its players start
// the game themselves. It names the game to start — §7.3 matching only
// finds one with the right time control and colours. Its key is the
// pairing's "paired" event, like createdNotice's, so each player is
// told once how their game is made.
func byHandNotice(round gen.Round, snap Snapshot, pairingID pgtype.UUID, s side, reason byHandReason) notify.Notice {
	start := fmt.Sprintf("Start it on Lichess as %s, you with %s.", timeControl(snap), s.colour)
	if reason == notCreated {
		start = fmt.Sprintf(
			"The league couldn't create this game on Lichess, so please start it by hand as %s, you with %s.",
			timeControl(snap),
			s.colour,
		)
	}
	return notify.Notice{
		User:     s.user,
		Category: notify.Round,
		Title:    fmt.Sprintf("Round %d: you play %s with %s", round.Number, s.opponent, s.colour),
		Body:     start + " Either of you can send the challenge; the site finds the game by itself once it begins.",
		Link:     "/account",
		Key:      pairedKey(round, pairingID),
	}
}

// createdNotice tells a player the league created their game. A retry
// an admin asked for creates games whose players were told to start
// them by hand, so it says so under a key of its own and asks them not
// to start another: had they started it already, the retry's search
// would have found it and created nothing (spec §6.3).
func createdNotice(
	round gen.Round,
	snap Snapshot,
	pairingID pgtype.UUID,
	gameID string,
	s side,
	retry bool,
) notify.Notice {
	n := notify.Notice{
		User:     s.user,
		Category: notify.Round,
		Title:    fmt.Sprintf("Round %d: your game against %s is ready", round.Number, s.opponent),
		Body: fmt.Sprintf(
			"The league has created it on Lichess: %s, you with %s. It's already in your list of games there, so there's nothing to start.",
			timeControl(snap),
			s.colour,
		),
		Link: lichessGameURL + gameID,
		Key:  pairedKey(round, pairingID),
	}
	if retry {
		n.Title = fmt.Sprintf("Round %d: your game against %s has now been created", round.Number, s.opponent)
		n.Body = fmt.Sprintf(
			"The league has now created it on Lichess: %s, you with %s. Don't start another one by hand.",
			timeControl(snap),
			s.colour,
		)
		n.Key = fmt.Sprintf("round:%d:created:%s", round.Number, pairingID.String())
	}
	return n
}

// lichessGameURL prefixes a game id to link to the game.
const lichessGameURL = "https://lichess.org/"

func pairedKey(round gen.Round, pairingID pgtype.UUID) string {
	return fmt.Sprintf("round:%d:paired:%s", round.Number, pairingID.String())
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
