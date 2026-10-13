// Package tokenhealth says whether the league can create games with a
// Lichess authorisation (spec §3.3): a player's, from their
// oauth_tokens row, and the organiser's, from what the last
// validate-tokens run found. It is pure: the caller reads the rows and
// passes the time. The pairing pool, the dashboard, /admin/tokens and
// /health all apply the same rules from here.
package tokenhealth

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Player is a player's Lichess authorisation as the league last saw
// it: whether one is stored at all and, if it no longer works, since
// when. It is worked out from the oauth_tokens row each time it is
// needed rather than stored, so a re-authorisation, which replaces the
// row and clears revoked_at, takes effect at once with nothing to undo
// (spec §3.3).
type Player struct {
	// Stored is false for a player who has never signed in: a member
	// seeded from the spreadsheet.
	Stored bool
	// InvalidSince is when the token stopped working: when it was found
	// revoked, or when it expired. Zero while it works.
	InvalidSince time.Time
	// Revoked says which of the two InvalidSince is: true for the
	// revocation, false for the expiry.
	Revoked bool
}

// PlayerOf reads a token from the oauth_tokens columns. issuedAt is
// never NULL in a stored row, so an invalid one means there is no row.
// A token that has both expired and been revoked has been invalid
// since the earlier of the two.
func PlayerOf(issuedAt, revokedAt, expiresAt pgtype.Timestamptz, now time.Time) Player {
	if !issuedAt.Valid {
		return Player{}
	}
	p := Player{Stored: true}
	if revokedAt.Valid {
		p.InvalidSince, p.Revoked = revokedAt.Time, true
	}
	if expiresAt.Valid && !expiresAt.Time.After(now) {
		if p.InvalidSince.IsZero() || expiresAt.Time.Before(p.InvalidSince) {
			p.InvalidSince, p.Revoked = expiresAt.Time, false
		}
	}
	return p
}

// Valid reports whether the league can create games with this token,
// as far as the last check knows.
func (p Player) Valid() bool { return p.Stored && p.InvalidSince.IsZero() }

// GraceEnds is when a lapsed token stops being tolerated:
// token.invalid_grace_days after it stopped working. Only meaningful
// for a stored token that is not valid.
func (p Player) GraceEnds(graceDays int) time.Time {
	return p.InvalidSince.Add(time.Duration(graceDays) * 24 * time.Hour)
}

// Excluded reports whether the player is left out of pairing when the
// site creates games (spec §3.3, §5.7 item 6): at once with no token
// at all, since grace is for a token that lapsed, or once a lapsed
// token has been invalid for more than the grace period. Inside the
// grace period the player is still paired, and their games go through
// the challenge fallback.
func (p Player) Excluded(graceDays int, now time.Time) bool {
	switch {
	case !p.Stored:
		return true
	case p.Valid():
		return false
	default:
		return now.After(p.GraceEnds(graceDays))
	}
}

// GraceDaysLeft is how many days, rounded up, the player has before a
// lapsed token excludes them; 0 once it does. The dashboard and
// /admin/tokens quote it.
func (p Player) GraceDaysLeft(graceDays int, now time.Time) int {
	left := p.GraceEnds(graceDays).Sub(now)
	if left <= 0 {
		return 0
	}
	day := 24 * time.Hour
	return int((left + day - 1) / day)
}
