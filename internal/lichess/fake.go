package lichess

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

	// UserGamesFn overrides UserGames's default behaviour, which is to
	// return every game in Games where the given username is a player.
	// Set it when a test needs UserGames to filter or order differently
	// than that default.
	UserGamesFn func(username string, opts UserGamesOptions) []Game
}

// NewFake returns an empty Fake ready for a test to populate.
func NewFake() *Fake {
	return &Fake{Users: map[string]User{}, Games: map[string]Game{}}
}

var _ API = (*Fake)(nil)

func (f *Fake) UsersByID(ctx context.Context, ids []string) ([]User, error) {
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
	return gameSliceStream(games), nil
}

func playerIs(p GamePlayer, username string) bool {
	return p.User != nil && p.User.ID == username
}

func (f *Fake) GamesByID(
	ctx context.Context,
	ids []string,
) (*GameStream, error) {
	var games []Game
	for _, id := range ids {
		if g, ok := f.Games[id]; ok {
			games = append(games, g)
		}
	}
	return gameSliceStream(games), nil
}

func (f *Fake) ExportGame(ctx context.Context, gameID string) (Game, error) {
	g, ok := f.Games[gameID]
	if !ok {
		return Game{}, &APIError{StatusCode: 404, Message: "Not found"}
	}
	return g, nil
}

// gameSliceStream encodes games to ndjson in memory and wraps them in
// the same GameStream type the real client returns, so Fake is a drop-in
// substitute for Client behind the API interface.
func gameSliceStream(games []Game) *GameStream {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, g := range games {
		_ = enc.Encode(g) // bytes.Buffer never fails to write
	}
	return newGameStream(io.NopCloser(&buf))
}
