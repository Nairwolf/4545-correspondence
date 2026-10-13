// Package notify is the on-site notification centre (spec §10): the
// league's only channel to a player, apart from the message Lichess
// sends by itself when bulk pairing creates a game. There is no email
// and no Lichess private message.
//
// Send writes one notification in the caller's transaction, so a
// notice is committed together with the change it announces, or not at
// all. The web layer reads them back for the bell and the list.
package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// Category is the kind of event a notification announces: the
// category column of spec §10's table. Players opt out per category.
type Category string

const (
	Registration Category = "registration" // application approved or rejected
	Round        Category = "round"        // paired, bye or double game at publication
	Challenge    Category = "challenge"    // a challenge to accept
	Unstarted    Category = "unstarted"    // a game not started 48 hours after publication
	MissedStart  Category = "missed_start" // one missed start from auto-pause
	AutoPause    Category = "auto_pause"   // paused after missed starts
	Token        Category = "token"        // the Lichess authorisation lapsed
	LevelUp      Category = "level_up"     // reached a new level
)

// Critical reports whether c is account-critical (spec §10): sent
// whatever the player's preferences, because it is about their access
// to the league itself.
func (c Category) Critical() bool {
	switch c {
	case Registration, AutoPause, Token:
		return true
	}
	return false
}

func (c Category) known() bool {
	if c.Critical() {
		return true
	}
	for _, o := range OptOuts {
		if o.Category == c {
			return true
		}
	}
	return false
}

// OptOut is one category a player may turn off, with the words the
// dashboard uses for it.
type OptOut struct {
	Category Category
	Label    string
}

// OptOuts lists every category a player may turn off, in the order the
// dashboard shows them: all but the critical ones (spec §10).
var OptOuts = []OptOut{
	{Round, "Pairings, byes and double games when a round is published"},
	{Challenge, "Challenges you need to accept"},
	{Unstarted, "Reminders when a game hasn't started after 48 hours"},
	{MissedStart, "A warning when one more missed start would pause your quest"},
	{LevelUp, "Reaching a new level"},
}

// ParseOptOut returns the opt-out-able category named s.
func ParseOptOut(s string) (Category, bool) {
	for _, o := range OptOuts {
		if string(o.Category) == s {
			return o.Category, true
		}
	}
	return "", false
}

// Notice is one notification to send.
type Notice struct {
	User     pgtype.UUID
	Category Category
	Title    string
	Body     string
	// Link is where the notification points, e.g. /account. Empty for
	// none.
	Link string
	// Key names the event, e.g. round:201:paired:<pairing id>. A second
	// Send with the same key to the same player is dropped, so a retried
	// job never notifies twice. Empty means no deduplication.
	Key string
}

// Send writes n unless the player turned its category off. It returns
// an error for a category this package does not know, rather than
// storing a notice no preference can ever silence.
func Send(ctx context.Context, q *gen.Queries, n Notice) error {
	if !n.Category.known() {
		return fmt.Errorf("notify: unknown category %q", n.Category)
	}
	if !n.Category.Critical() {
		muted, err := q.IsNotificationCategoryMuted(ctx, gen.IsNotificationCategoryMutedParams{
			UserID:   n.User,
			Category: string(n.Category),
		})
		if err != nil {
			return fmt.Errorf("notify: read preference: %w", err)
		}
		if muted {
			return nil
		}
	}
	_, err := q.InsertNotification(ctx, gen.InsertNotificationParams{
		UserID:    n.User,
		Category:  string(n.Category),
		Title:     n.Title,
		Body:      n.Body,
		LinkUrl:   optional(n.Link),
		DedupeKey: optional(n.Key),
	})
	if err != nil {
		return fmt.Errorf("notify: insert %s notice: %w", n.Category, err)
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// TokenLapsed is the token notice of spec §10, for a stored token
// Lichess no longer accepts. Both checks that can find one —
// validate-tokens and the probe before a round's games are created —
// send this same notice under the same key, keyed by the token's issue
// time, so a player is told once per token whichever finds it first.
// It is account-critical, so it reaches them whatever their
// preferences. The wording fits both values of pairing.game_creation:
// the dashboard banner, which follows the setting, gives the
// consequence in days.
func TokenLapsed(user pgtype.UUID, issuedAt time.Time) Notice {
	return Notice{
		User:     user,
		Category: Token,
		Title:    "Your Lichess authorisation has lapsed",
		Body: "Lichess no longer accepts the authorisation you gave the league, which it needs to create your games. " +
			"Re-authorise from your account page: it takes one click.",
		Link: "/account",
		Key:  "token:revoked:" + issuedAt.UTC().Format(time.RFC3339Nano),
	}
}
