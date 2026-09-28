package lichess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

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

	// Game creation (Phase 5). TokenInfo is what Lichess knows about each
	// player's token: TestTokens reports it (a token absent here is
	// invalid, null), and a bulk or a challenge refuses a token that is
	// absent or lacks challenge:write, as Lichess does. A bulk's games
	// name their players by TokenInfo's UserID.
	TokenInfo map[tokencrypt.Secret]TokenInfo

	// RejectTokens makes CreateBulkPairing refuse these tokens with the
	// given reason even though TestTokens calls them valid — a token
	// revoked between the probe and the bulk.
	RejectTokens map[tokencrypt.Secret]string

	// Bulks is the organiser's bulk list: every bulk CreateBulkPairing
	// created is appended, and ListBulkPairings returns them newest
	// first. A test can seed it to stand for a bulk created by an
	// earlier run that crashed before recording it.
	Bulks []BulkPairing
	// BulkRequests is every request CreateBulkPairing accepted, in order.
	BulkRequests []BulkPairingRequest

	// Challenges is every challenge CreateChallenge created, in order;
	// CancelChallenge marks one "canceled".
	Challenges []Challenge

	// Calls records every game-creation call, in order, with the token
	// it was made as (none for TestTokens), so a test can assert which
	// account acted — or that no call was made at all.
	Calls []FakeCall

	// Per-method failures, returned before anything is recorded or
	// created. ChallengeErrFor is keyed by the opponent's username.
	CreateBulkErr   error
	ListBulksErr    error
	TestTokensErr   error
	ChallengeErrFor map[string]error
	CancelErr       error

	nextID int
}

// FakeCall is one call to a game-creation method of Fake.
type FakeCall struct {
	Method string
	Token  tokencrypt.Secret
}

// NewFake returns an empty Fake ready for a test to populate.
func NewFake() *Fake {
	return &Fake{
		Users:    map[string]User{},
		Games:    map[string]Game{},
		RawGames: map[string][]byte{},
		Codes:    map[string]tokencrypt.Secret{},
		Accounts: map[tokencrypt.Secret]Account{},

		TokenInfo:       map[tokencrypt.Secret]TokenInfo{},
		RejectTokens:    map[tokencrypt.Secret]string{},
		ChallengeErrFor: map[string]error{},
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

// newID returns the next made-up Lichess id: bulk1, game2, chal3, ...
// Ids are unique across kinds, like Lichess's, and depend only on the
// order of calls, so a test can predict them.
func (f *Fake) newID(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s%d", prefix, f.nextID)
}

// noSuchToken is the 401 Lichess answers for an unknown bearer.
var noSuchToken = &APIError{StatusCode: 401, Message: "No such token"}

// tokenProblem is why Lichess would refuse t for game creation, or ""
// if it would accept it.
func (f *Fake) tokenProblem(t tokencrypt.Secret) string {
	info, ok := f.TokenInfo[t]
	switch {
	case !ok:
		return "No such token"
	case !info.HasScope(ScopeChallengeWrite):
		return "Missing scope " + ScopeChallengeWrite
	}
	return f.RejectTokens[t]
}

// CreateBulkPairing implements API. Like Lichess it is all-or-nothing:
// every token in the request is checked first, and any problem refuses
// the whole bulk as a *BulkRejection naming each bad token.
func (f *Fake) CreateBulkPairing(
	ctx context.Context,
	organiser tokencrypt.Secret,
	req BulkPairingRequest,
) (BulkPairing, error) {
	if f.Err != nil {
		return BulkPairing{}, f.Err
	}
	if f.CreateBulkErr != nil {
		return BulkPairing{}, f.CreateBulkErr
	}
	f.Calls = append(f.Calls, FakeCall{Method: "CreateBulkPairing", Token: organiser})
	if organiser == "" {
		return BulkPairing{}, noSuchToken
	}

	bad := map[tokencrypt.Secret]string{}
	for _, p := range req.Pairs {
		for _, t := range []tokencrypt.Secret{p.White, p.Black} {
			if problem := f.tokenProblem(t); problem != "" {
				bad[t] = problem
			}
		}
	}
	if len(bad) > 0 {
		return BulkPairing{}, &BulkRejection{Tokens: bad}
	}

	// Stamped with the wall clock, as Lichess does: a reconciliation that
	// looks for bulks created after a round was published must find it.
	now := time.Now().UnixMilli()
	bulk := BulkPairing{
		ID:             f.newID("bulk"),
		Variant:        "standard",
		Rated:          req.Rated,
		ScheduledAt:    now,
		PairAt:         now,
		Correspondence: &CorrespondenceTimeControl{DaysPerTurn: req.Days},
	}
	for _, p := range req.Pairs {
		bulk.Games = append(bulk.Games, BulkGame{
			ID:    f.newID("game"),
			White: f.TokenInfo[p.White].UserID,
			Black: f.TokenInfo[p.Black].UserID,
		})
	}
	f.Bulks = append(f.Bulks, bulk)
	f.BulkRequests = append(f.BulkRequests, req)
	return bulk, nil
}

// ListBulkPairings implements API: Bulks, newest first.
func (f *Fake) ListBulkPairings(ctx context.Context, organiser tokencrypt.Secret) ([]BulkPairing, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if f.ListBulksErr != nil {
		return nil, f.ListBulksErr
	}
	f.Calls = append(f.Calls, FakeCall{Method: "ListBulkPairings", Token: organiser})
	if organiser == "" {
		return nil, noSuchToken
	}
	bulks := slices.Clone(f.Bulks)
	slices.Reverse(bulks)
	return bulks, nil
}

// TestTokens implements API from TokenInfo. Every token asked about is
// in the result, as Lichess answers for each one.
func (f *Fake) TestTokens(
	ctx context.Context,
	tokens []tokencrypt.Secret,
) (map[tokencrypt.Secret]*TokenInfo, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	if f.TestTokensErr != nil {
		return nil, f.TestTokensErr
	}
	f.Calls = append(f.Calls, FakeCall{Method: "TestTokens"})
	result := make(map[tokencrypt.Secret]*TokenInfo, len(tokens))
	for _, t := range tokens {
		if info, ok := f.TokenInfo[t]; ok {
			result[t] = &info
		} else {
			result[t] = nil
		}
	}
	return result, nil
}

// CreateChallenge implements API. The challenger's token must be one
// Lichess would accept; the opponent is taken as given.
func (f *Fake) CreateChallenge(
	ctx context.Context,
	challenger tokencrypt.Secret,
	opponent string,
	req ChallengeRequest,
) (Challenge, error) {
	if f.Err != nil {
		return Challenge{}, f.Err
	}
	if err := f.ChallengeErrFor[opponent]; err != nil {
		return Challenge{}, err
	}
	f.Calls = append(f.Calls, FakeCall{Method: "CreateChallenge", Token: challenger})
	if f.tokenProblem(challenger) != "" {
		return Challenge{}, noSuchToken
	}

	id := f.newID("chal")
	ch := Challenge{
		ID:         id,
		URL:        "https://lichess.example/" + id,
		Status:     "created",
		Challenger: ChallengeUser{ID: f.TokenInfo[challenger].UserID},
		DestUser:   &ChallengeUser{ID: strings.ToLower(opponent), Name: opponent},
		Rated:      req.Rated,
		Speed:      "correspondence",
		Color:      string(req.Color),
		FinalColor: string(req.Color),
	}
	ch.TimeControl.Type = "correspondence"
	ch.TimeControl.DaysPerTurn = req.Days
	f.Challenges = append(f.Challenges, ch)
	return ch, nil
}

// CancelChallenge implements API. Like Lichess it answers 404 for a
// challenge that does not exist or was sent by someone else.
func (f *Fake) CancelChallenge(ctx context.Context, challenger tokencrypt.Secret, id string) error {
	if f.Err != nil {
		return f.Err
	}
	if f.CancelErr != nil {
		return f.CancelErr
	}
	f.Calls = append(f.Calls, FakeCall{Method: "CancelChallenge", Token: challenger})
	for i, ch := range f.Challenges {
		if ch.ID == id && ch.Challenger.ID == f.TokenInfo[challenger].UserID {
			f.Challenges[i].Status = "canceled"
			return nil
		}
	}
	return &APIError{StatusCode: 404, Message: "Not found"}
}
