package lichess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// tokensBatchSize is Lichess's documented limit for one token test
// ("For up to 1000 OAuth tokens").
const tokensBatchSize = 1000

// ScopeChallengeWrite is the scope a player's token needs for their
// games to be created: in a bulk, or as a challenge they send.
const ScopeChallengeWrite = "challenge:write"

// TokenInfo is what Lichess says about a valid token.
type TokenInfo struct {
	// UserID is the Lichess user id (lowercase username) of the account
	// the token belongs to.
	UserID string
	Scopes []string
	// Expires is zero for a token that never expires.
	Expires time.Time
}

// HasScope reports whether the token was granted scope.
func (t TokenInfo) HasScope(scope string) bool { return slices.Contains(t.Scopes, scope) }

// TestTokens implements API. The call is unauthenticated: the tokens
// being tested travel in the body, never as the bearer. Duplicates are
// sent once.
func (c *Client) TestTokens(
	ctx context.Context,
	tokens []tokencrypt.Secret,
) (map[tokencrypt.Secret]*TokenInfo, error) {
	unique := make([]string, 0, len(tokens))
	seen := make(map[tokencrypt.Secret]bool, len(tokens))
	for _, t := range tokens {
		if t != "" && !seen[t] {
			seen[t] = true
			unique = append(unique, string(t))
		}
	}

	result := make(map[tokencrypt.Secret]*TokenInfo, len(unique))
	for len(unique) > 0 {
		n := min(tokensBatchSize, len(unique))
		batch := unique[:n]
		unique = unique[n:]

		resp, err := c.do(
			ctx,
			http.MethodPost,
			"/api/token/test",
			nil,
			[]byte(strings.Join(batch, ",")),
			"text/plain",
			"application/json",
			"",
		)
		if err != nil {
			return nil, err
		}
		var body map[string]*struct {
			UserID  string `json:"userId"`
			Scopes  string `json:"scopes"`
			Expires *int64 `json:"expires"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("lichess: decode token test: %w", err)
		}

		for token, info := range body {
			if info == nil {
				result[tokencrypt.Secret(token)] = nil
				continue
			}
			ti := &TokenInfo{UserID: info.UserID}
			if info.Scopes != "" {
				ti.Scopes = strings.Split(info.Scopes, ",")
			}
			if info.Expires != nil {
				ti.Expires = time.UnixMilli(*info.Expires)
			}
			result[tokencrypt.Secret(token)] = ti
		}
	}
	return result, nil
}
