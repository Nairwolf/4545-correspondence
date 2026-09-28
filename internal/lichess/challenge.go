package lichess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// Color is a side of the board, as Lichess spells it.
type Color string

const (
	ColorWhite Color = "white"
	ColorBlack Color = "black"
)

// ChallengeRequest is a correspondence challenge to send (spec §3.3).
// The variant is always standard.
type ChallengeRequest struct {
	// Days is the days-per-move time control: one of 1, 2, 3, 5, 7, 10
	// or 14.
	Days  int
	Rated bool
	// Color is the colour the challenger plays — the one the pairing
	// assigned them. It is always set: a random colour would undo the
	// engine's colour balancing.
	Color Color
}

// Challenge is Lichess's record of a challenge (the documented
// ChallengeJson schema, the fields this application reads).
type Challenge struct {
	// ID is also the id of the game the challenge becomes once accepted.
	ID  string `json:"id"`
	URL string `json:"url"`
	// Status is "created" while it waits for the opponent, then
	// "accepted", "declined", "canceled" or "offline".
	Status     string         `json:"status"`
	Challenger ChallengeUser  `json:"challenger"`
	DestUser   *ChallengeUser `json:"destUser"`
	Rated      bool           `json:"rated"`
	Speed      string         `json:"speed"`
	// TimeControl is {"type": "correspondence", "daysPerTurn": N} for
	// every challenge the league sends.
	TimeControl struct {
		Type        string `json:"type"`
		DaysPerTurn int    `json:"daysPerTurn"`
	} `json:"timeControl"`
	// Color is what was asked for; FinalColor the challenger's actual
	// colour.
	Color      string `json:"color"`
	FinalColor string `json:"finalColor"`
}

// ChallengeUser is one side of a challenge.
type ChallengeUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CreateChallenge implements API. The response is not streamed
// (keepAliveStream is not sent): a correspondence challenge does not
// expire the way a real-time one does after 20 seconds.
func (c *Client) CreateChallenge(
	ctx context.Context,
	challenger tokencrypt.Secret,
	opponent string,
	req ChallengeRequest,
) (Challenge, error) {
	form := url.Values{
		"days":    {strconv.Itoa(req.Days)},
		"rated":   {strconv.FormatBool(req.Rated)},
		"color":   {string(req.Color)},
		"variant": {"standard"},
	}
	resp, err := c.do(
		ctx,
		http.MethodPost,
		"/api/challenge/"+url.PathEscape(opponent),
		nil,
		[]byte(form.Encode()),
		"application/x-www-form-urlencoded",
		"application/json",
		string(challenger),
	)
	if err != nil {
		return Challenge{}, err
	}
	defer resp.Body.Close()

	var ch Challenge
	if err := json.NewDecoder(resp.Body).Decode(&ch); err != nil {
		return Challenge{}, fmt.Errorf("lichess: decode challenge: %w", err)
	}
	return ch, nil
}

// CancelChallenge implements API. Lichess answers 404 for a challenge
// that is gone or was never challenger's; the caller decides whether
// that matters. Note that once a challenge has been accepted, the same
// call aborts the game instead, as long as it can still be aborted
// (until both players have made their first move).
func (c *Client) CancelChallenge(ctx context.Context, challenger tokencrypt.Secret, id string) error {
	resp, err := c.do(
		ctx,
		http.MethodPost,
		"/api/challenge/"+url.PathEscape(id)+"/cancel",
		nil,
		nil,
		"",
		"application/json",
		string(challenger),
	)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
