package tokenhealth

import "time"

// OrganiserState is what the last check found about LICHESS_ORG_TOKEN,
// the league account's challenge:bulk token that creates the round's
// games (spec §3.2).
type OrganiserState string

const (
	// OrganiserValid: Lichess accepts the token and it carries
	// challenge:bulk.
	OrganiserValid OrganiserState = "valid"
	// OrganiserNotSet: LICHESS_ORG_TOKEN is empty. Expected while
	// pairing.game_creation is manual.
	OrganiserNotSet OrganiserState = "not_set"
	// OrganiserInvalid: Lichess does not know the token, or it expired.
	OrganiserInvalid OrganiserState = "invalid"
	// OrganiserMissingScope: the token works but cannot create bulks.
	OrganiserMissingScope OrganiserState = "missing_scope"
)

// ScopeChallengeBulk is the scope the organiser token needs.
const ScopeChallengeBulk = "challenge:bulk"

// ExpiryWarning is how long before the organiser token expires the
// admins are warned (spec §8.5).
const ExpiryWarning = 14 * 24 * time.Hour

// Organiser is what one validate-tokens run found about the organiser
// token, stored in the run's job_runs detail under "organiser_token".
// /health and the admin banner read it back from the latest run that
// got an answer from Lichess.
type Organiser struct {
	State OrganiserState `json:"state"`
	// Account is the Lichess user id the token belongs to, when it was
	// recognised: the league account whose name players see on the
	// bulk's message.
	Account string `json:"account,omitempty"`
	// Expires is when the token stops working; nil for a token that
	// never expires, or one Lichess did not recognise.
	Expires *time.Time `json:"expires,omitempty"`
}

// Usable reports whether games can be created with the token at now:
// it was valid at the check and has not expired since.
func (o Organiser) Usable(now time.Time) bool {
	return o.State == OrganiserValid && (o.Expires == nil || o.Expires.After(now))
}

// ExpiresSoon reports whether a usable token expires within
// ExpiryWarning of now.
func (o Organiser) ExpiresSoon(now time.Time) bool {
	return o.Usable(now) && o.Expires != nil && o.Expires.Sub(now) <= ExpiryWarning
}
