package lichess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// BulkPairingRequest is one bulk of correspondence games to create
// (spec §3.2). Everything the league does not vary — standard variant,
// games starting at once — is fixed by CreateBulkPairing.
type BulkPairingRequest struct {
	// Pairs is one entry per game. For correspondence Lichess accepts
	// the same player in more than one pair, which is how the double
	// game's two games go in one bulk.
	Pairs []BulkPair
	// Days is the days-per-move time control: one of 1, 2, 3, 5, 7, 10
	// or 14, or Lichess refuses the whole bulk.
	Days  int
	Rated bool
	// Message is sent to each player from the organiser account when
	// their game is created. Empty sends Lichess's default; a custom
	// message must contain the {game} placeholder.
	Message string
}

// BulkPair is one game of a bulk: the two players' challenge:write
// tokens, white first.
type BulkPair struct {
	White, Black tokencrypt.Secret
}

// BulkPairing is Lichess's record of a bulk (the documented BulkPairing
// schema). A correspondence bulk has no clock; its time control is in
// Correspondence instead (read from the server's source, not documented).
type BulkPairing struct {
	ID    string     `json:"id"`
	Games []BulkGame `json:"games"`
	// Variant is the variant's key, e.g. "standard".
	Variant string `json:"variant"`
	Rated   bool   `json:"rated"`
	// ScheduledAt is when the bulk was created, PairAt when its games
	// are (or were) created, in epoch milliseconds. PairedAt is null
	// until then — and in the creation response even for a bulk whose
	// games were created at once, because Lichess answers with the bulk
	// as it was stored.
	ScheduledAt int64  `json:"scheduledAt"`
	PairAt      int64  `json:"pairAt"`
	PairedAt    *int64 `json:"pairedAt"`

	Correspondence *CorrespondenceTimeControl `json:"correspondence"`
}

// CorrespondenceTimeControl is a correspondence bulk's time control.
type CorrespondenceTimeControl struct {
	DaysPerTurn int `json:"daysPerTurn"`
}

// BulkGame is one game of a bulk. White and Black are the players'
// Lichess user ids (lowercase usernames). The game id is assigned when
// the bulk is created, before the game itself exists.
type BulkGame struct {
	ID    string `json:"id"`
	White string `json:"white"`
	Black string `json:"black"`
}

func (b BulkPairing) ScheduledAtTime() time.Time { return time.UnixMilli(b.ScheduledAt) }

// BulkRejection is Lichess refusing a bulk (HTTP 400). A bulk is
// all-or-nothing: none of its games were created, and a refused bulk
// costs nothing against the rate limit (spec §6.3).
//
// Lichess names bad tokens by their value — the response body is full of
// secrets — so the body itself is never kept: only the reasons, keyed by
// the tokens the caller sent, from which it finds the players.
type BulkRejection struct {
	// Tokens is each token Lichess refused, with its reason (for
	// example "No such token", or a missing scope).
	Tokens map[tokencrypt.Secret]string
	// DuplicateUsers is set when a player appears in more than one pair
	// of a bulk that does not allow it (real-time games only, so not
	// expected here).
	DuplicateUsers []string
	// Message is any other reason Lichess gave: too many bulks queued, a
	// form field it refused, and so on.
	Message string
}

// Error names how many tokens were refused and why, never a token.
func (e *BulkRejection) Error() string {
	var parts []string
	if len(e.Tokens) > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d %s refused (%s)",
			len(e.Tokens),
			plural(len(e.Tokens), "token", "tokens"),
			countReasons(e.Tokens),
		))
	}
	if len(e.DuplicateUsers) > 0 {
		parts = append(parts, "paired more than once: "+strings.Join(e.DuplicateUsers, ", "))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if len(parts) == 0 {
		return "lichess: bulk pairing refused, no reason given"
	}
	return "lichess: bulk pairing refused: " + strings.Join(parts, "; ")
}

// countReasons summarises the refused tokens' reasons as
// `"No such token" ×2, "Missing scope" ×1`, most frequent first, then
// alphabetically, so the same rejection always reads the same.
func countReasons(tokens map[tokencrypt.Secret]string) string {
	counts := map[string]int{}
	for _, reason := range tokens {
		counts[reason]++
	}
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, reason)
	}
	slices.SortFunc(reasons, func(a, b string) int {
		if counts[a] != counts[b] {
			return counts[b] - counts[a]
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%q ×%d", reason, counts[reason]))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// maxRejectionBytes bounds a refused bulk's body. It names up to a
// thousand tokens, each with a reason, so it can be far longer than the
// short error bodies readAPIError reads.
const maxRejectionBytes = 1 << 20

// CreateBulkPairing implements API.
func (c *Client) CreateBulkPairing(
	ctx context.Context,
	organiser tokencrypt.Secret,
	req BulkPairingRequest,
) (BulkPairing, error) {
	if len(req.Pairs) == 0 {
		return BulkPairing{}, errors.New("lichess: bulk pairing with no games")
	}

	pairs := make([]string, 0, len(req.Pairs))
	for _, p := range req.Pairs {
		pairs = append(pairs, string(p.White)+":"+string(p.Black))
	}
	form := url.Values{
		"players": {strings.Join(pairs, ",")},
		"days":    {strconv.Itoa(req.Days)},
		"rated":   {strconv.FormatBool(req.Rated)},
		"variant": {"standard"},
	}
	if req.Message != "" {
		form.Set("message", req.Message)
	}

	resp, err := c.send(
		ctx,
		http.MethodPost,
		"/api/bulk-pairing",
		nil,
		[]byte(form.Encode()),
		"application/x-www-form-urlencoded",
		"application/json",
		string(organiser),
	)
	if err != nil {
		return BulkPairing{}, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return BulkPairing{}, readBulkRejection(resp.Body, req.Pairs)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return BulkPairing{}, readAPIError(resp)
	}

	var bulk BulkPairing
	if err := json.NewDecoder(resp.Body).Decode(&bulk); err != nil {
		return BulkPairing{}, fmt.Errorf("lichess: decode bulk pairing: %w", err)
	}
	return bulk, nil
}

// readBulkRejection reads a refused bulk's body, in any of the three
// shapes Lichess's server sends: {"tokens": {"<token>": "<reason>"}},
// {"duplicateUsers": [...]} or {"error": ...}. Every text taken from it
// has the request's tokens blanked out, in case a reason ever quotes
// one.
func readBulkRejection(body io.Reader, pairs []BulkPair) error {
	var decoded struct {
		Tokens         map[string]string `json:"tokens"`
		DuplicateUsers []string          `json:"duplicateUsers"`
		Error          json.RawMessage   `json:"error"`
	}
	rejection := &BulkRejection{}
	if err := json.NewDecoder(io.LimitReader(body, maxRejectionBytes)).Decode(&decoded); err != nil {
		rejection.Message = "unreadable response"
		return rejection
	}

	sent := make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		sent = append(sent, string(p.White), string(p.Black))
	}
	redact := func(s string) string {
		for _, token := range sent {
			if token != "" {
				s = strings.ReplaceAll(s, token, "[redacted]")
			}
		}
		return s
	}

	if len(decoded.Tokens) > 0 {
		rejection.Tokens = make(map[tokencrypt.Secret]string, len(decoded.Tokens))
		for token, reason := range decoded.Tokens {
			rejection.Tokens[tokencrypt.Secret(token)] = redact(reason)
		}
	}
	rejection.DuplicateUsers = decoded.DuplicateUsers
	rejection.Message = redact(errorMessage(decoded.Error))
	return rejection
}

// ListBulkPairings implements API. Lichess returns the organiser's 100
// most recent bulks.
func (c *Client) ListBulkPairings(ctx context.Context, organiser tokencrypt.Secret) ([]BulkPairing, error) {
	resp, err := c.do(
		ctx,
		http.MethodGet,
		"/api/bulk-pairing",
		nil,
		nil,
		"",
		"application/json",
		string(organiser),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body struct {
		Bulks []BulkPairing `json:"bulks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("lichess: decode bulk pairing list: %w", err)
	}
	return body.Bulks, nil
}
