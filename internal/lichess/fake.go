package lichess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// Fake is an in-memory implementation of API for tests: no network, no
// httptest.Server — just canned data the test sets up directly. Every
// consumer of the Lichess API in this codebase (sync-games, refresh-
// ratings, ...) should depend on the API interface so its tests can use
// this instead of a real client (spec §11: "no test touches the live
// API").
type Fake struct {
	// Users is keyed by Lichess user id (lowercase username).
	Users map[string]User
	// Games is keyed by Lichess game id.
	Games map[string]Game

	// RawGames, also keyed by game id, supplies the exact bytes a stream
	// or ExportGame should hand back for that id. Optional: when absent,
	// the fake re-encodes the Game struct, which is fine for every test
	// that doesn't care what ends up in raw_payload. A test that does
	// (spec §7.1: the payload must be Lichess's response, not our
	// subset) sets both — the Game for behaviour, RawGames for bytes.
	RawGames map[string][]byte

	// UserGamesFn overrides UserGames's default behaviour, which is to
	// return every game in Games where the given username is a player.
	// Set it when a test needs UserGames to filter or order differently
	// than that default.
	UserGamesFn func(username string, opts UserGamesOptions) []Game

	// UserGamesErrFor, keyed by username, makes UserGames return that
	// error instead of a stream — the one knob this fake needs to test
	// a caller's graceful-degradation path (spec §7.2/§11: a Lichess
	// failure must not be fatal) without touching the live API.
	UserGamesErrFor map[string]error

	// Err, when set, makes every call fail with it — Lichess unreachable.
	Err error

	// Codes maps an authorization code to the token Exchange hands back
	// for it; an unknown code is refused the way Lichess refuses one.
	// Together with Accounts (token → profile) and Revoked (every token
	// RevokeToken was called with) this lets one Fake stand behind the
	// whole sign-in callback.
	Codes    map[string]tokencrypt.Secret
	Accounts map[tokencrypt.Secret]Account
	Revoked  []tokencrypt.Secret
}

// NewFake returns an empty Fake ready for a test to populate.
func NewFake() *Fake {
	return &Fake{
		Users:    map[string]User{},
		Games:    map[string]Game{},
		RawGames: map[string][]byte{},
		Codes:    map[string]tokencrypt.Secret{},
		Accounts: map[tokencrypt.Secret]Account{},
	}
}

var (
	_ API  = (*Fake)(nil)
	_ Auth = (*Fake)(nil)
)

func (f *Fake) UsersByID(ctx context.Context, ids []string) ([]User, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	var out []User
	for _, id := range ids {
		if u, ok := f.Users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *Fake) UserGames(
	ctx context.Context,
	username string,
	opts UserGamesOptions,
) (*GameStream, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if err := f.UserGamesErrFor[username]; err != nil {
		return nil, err
	}
	var games []Game
	if f.UserGamesFn != nil {
		games = f.UserGamesFn(username, opts)
	} else {
		for _, g := range f.Games {
			if playerIs(g.Players.White, username) || playerIs(g.Players.Black, username) {
				games = append(games, g)
			}
		}
	}
	return f.gameSliceStream(games), nil
}

func playerIs(p GamePlayer, username string) bool {
	return p.User != nil && p.User.ID == username
}

func (f *Fake) GamesByID(
	ctx context.Context,
	ids []string,
) (*GameStream, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	var games []Game
	for _, id := range ids {
		if g, ok := f.Games[id]; ok {
			games = append(games, g)
		}
	}
	return f.gameSliceStream(games), nil
}

func (f *Fake) ExportGame(ctx context.Context, gameID string) (Game, []byte, error) {
	if f.Err != nil {
		return Game{}, nil, f.Err
	}
	g, ok := f.Games[gameID]
	if !ok {
		return Game{}, nil, &APIError{StatusCode: 404, Message: "Not found"}
	}
	return g, f.rawFor(g), nil
}

// rawFor is the bytes the fake emits for g: RawGames if the test set
// them, otherwise a plain re-encoding.
func (f *Fake) rawFor(g Game) []byte {
	if raw, ok := f.RawGames[g.ID]; ok {
		return raw
	}
	b, _ := json.Marshal(g) // Game has no unmarshalable fields
	return b
}

// gameSliceStream writes games as ndjson in memory and wraps them in the
// same GameStream type the real client returns, so Fake is a drop-in
// substitute for Client behind the API interface. Each line is rawFor's
// bytes, so a test-supplied RawGames entry comes back through Raw()
// exactly as the real stream would deliver Lichess's line.
func (f *Fake) gameSliceStream(games []Game) *GameStream {
	var buf bytes.Buffer
	for _, g := range games {
		buf.Write(bytes.TrimSpace(f.rawFor(g)))
		buf.WriteByte('\n')
	}
	return newGameStream(io.NopCloser(&buf))
}

// AuthCodeURL implements Auth. The URL is not Lichess's, but it carries
// the same query so a test can check what the browser would be sent.
func (f *Fake) AuthCodeURL(state, verifier string, scopes []string) string {
	q := url.Values{
		"state":     {state},
		"scope":     {joinScopes(scopes)},
		"challenge": {verifier},
	}
	return "https://lichess.example/oauth?" + q.Encode()
}

func joinScopes(scopes []string) string {
	var b bytes.Buffer
	for i, s := range scopes {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	return b.String()
}

// Exchange implements Auth.
func (f *Fake) Exchange(ctx context.Context, code, verifier string) (Token, error) {
	if f.Err != nil {
		return Token{}, f.Err
	}
	tok, ok := f.Codes[code]
	if !ok {
		return Token{}, &ExchangeError{Code: "invalid_grant", Description: "unknown code"}
	}
	return Token{AccessToken: tok}, nil
}

// Account implements API. An unknown token is a 401, as Lichess answers
// for a revoked or made-up one.
func (f *Fake) Account(ctx context.Context, bearer tokencrypt.Secret) (Account, []byte, error) {
	if f.Err != nil {
		return Account{}, nil, f.Err
	}
	a, ok := f.Accounts[bearer]
	if !ok {
		return Account{}, nil, &APIError{StatusCode: 401, Message: "No such token"}
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return Account{}, nil, fmt.Errorf("fake: encode account: %w", err)
	}
	return a, raw, nil
}

// RevokeToken implements API by recording the call.
func (f *Fake) RevokeToken(ctx context.Context, bearer tokencrypt.Secret) error {
	if f.Err != nil {
		return f.Err
	}
	f.Revoked = append(f.Revoked, bearer)
	return nil
}
