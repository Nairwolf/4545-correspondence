// Package lichess is the one place in this application that speaks HTTP
// to lichess.org (spec §3.2: "wrap all of this in a single LichessClient
// module so the rest of the app never touches HTTP directly"). It
// implements only what Phase 1 needs: reading public user ratings and
// game history. Bulk pairing, challenges and OAuth are later phases.
package lichess

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://lichess.org"

// usersBatchSize and gamesBatchSize are Lichess's own documented limits
// (spec verification: "Get up to 300 users by their IDs"; "300 IDs can
// be submitted" for game export). The client batches internally so no
// caller needs to know these numbers.
const (
	usersBatchSize = 300
	gamesBatchSize = 300
)

// API is what the rest of the application depends on — never the
// concrete *Client — so tests can substitute Fake without any HTTP
// involved.
type API interface {
	// UsersByID fetches public rating data for any number of accounts,
	// batching internally. Accounts that don't exist (closed, renamed)
	// are simply absent from the result rather than causing an error.
	UsersByID(ctx context.Context, ids []string) ([]User, error)

	// UserGames streams one account's games, most options mapped from
	// UserGamesOptions. The caller must Close the returned stream.
	UserGames(
		ctx context.Context,
		username string,
		opts UserGamesOptions,
	) (*GameStream, error)

	// GamesByID streams games by id, batching internally. IDs that
	// don't match a game are simply absent from the stream.
	GamesByID(ctx context.Context, ids []string) (*GameStream, error)

	// ExportGame fetches one game by id.
	ExportGame(ctx context.Context, gameID string) (Game, error)
}

// Client is the real, HTTP-backed implementation of API.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client

	// mu serializes every outbound call this Client makes. Lichess's own
	// rate-limiting guidance is "only make one request at a time" (spec
	// §7.2) — this is not an optimisation, it's what keeps the app from
	// ever tripping a 429 in the first place.
	mu sync.Mutex

	sleep     func(time.Duration) // injected in tests to avoid a real 60s wait
	retryWait time.Duration
}

// Option configures a Client constructed with New.
type Option func(*Client)

// WithBaseURL overrides the default https://lichess.org, for pointing a
// Client at an httptest.Server in tests.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient overrides the default *http.Client (e.g. for a custom
// timeout).
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.httpClient = hc } }

// WithSleepFunc overrides how the client waits out a 429 — tests inject
// a no-op that just records the requested duration instead of sleeping.
func WithSleepFunc(f func(time.Duration)) Option { return func(c *Client) { c.sleep = f } }

// WithRetryWait overrides how long the client waits after a 429 before
// retrying once (default 60s, per spec §7.2).
func WithRetryWait(d time.Duration) Option { return func(c *Client) { c.retryWait = d } }

// New creates a Client. token is optional — Phase 1 only makes
// unauthenticated, read-only calls, so an empty token is the normal
// case; a token merely raises Lichess's per-account throttle.
func New(token string, opts ...Option) *Client {
	c := &Client{
		baseURL:    defaultBaseURL,
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		sleep:      time.Sleep,
		retryWait:  60 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

var _ API = (*Client)(nil)

// APIError is returned for any non-2xx response that survives the 429
// retry. It carries Lichess's own {"error": "..."} message when one was
// sent (spec's verified Error schema), so callers and logs see the real
// reason rather than just a status code.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("lichess: %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("lichess: unexpected status %d", e.StatusCode)
}

// do issues one request, retrying exactly once after a 429 (spec §7.2:
// wait at least 60s, retry once, then let the caller's job fail and be
// retried by the scheduler — this function only owns the one retry, not
// the job-level retry). The caller must close the returned response
// body on success.
func (c *Client) do(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
	contentType, accept string,
) (*http.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fullURL := c.baseURL + path
	if len(query) > 0 {
		fullURL += "?" + query.Encode()
	}

	attempt := func() (*http.Response, error) {
		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("lichess: build request: %w", err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		return c.httpClient.Do(req)
	}

	resp, err := attempt()
	if err != nil {
		return nil, fmt.Errorf("lichess: %s %s: %w", method, path, err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		c.sleep(c.retryWait)
		resp, err = attempt()
		if err != nil {
			return nil, fmt.Errorf(
				"lichess: %s %s (retry after 429): %w",
				method, path, err,
			)
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readAPIError(resp)
	}

	return resp, nil
}

func readAPIError(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		var body struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&body); err == nil {
			apiErr.Message = body.Error
		}
	}
	return apiErr
}

// UsersByID implements API.
func (c *Client) UsersByID(ctx context.Context, ids []string) ([]User, error) {
	var all []User
	for len(ids) > 0 {
		n := usersBatchSize
		if n > len(ids) {
			n = len(ids)
		}
		batch := ids[:n]
		ids = ids[n:]

		resp, err := c.do(
			ctx,
			http.MethodPost,
			"/api/users",
			nil,
			[]byte(strings.Join(batch, ",")),
			"text/plain",
			"application/json",
		)
		if err != nil {
			return nil, err
		}
		var users []User
		err = json.NewDecoder(resp.Body).Decode(&users)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("lichess: decode /api/users response: %w", err)
		}
		all = append(all, users...)
	}
	return all, nil
}

// UserGamesOptions configures UserGames. Options not listed here
// (moves, pgnInJson, tags, division, literate, ...) are left at their
// Lichess defaults because nothing in this application uses them.
type UserGamesOptions struct {
	PerfType string    // e.g. "correspondence"
	Rated    *bool     // nil omits the filter (both rated and casual)
	Since    time.Time // zero omits the filter (Lichess default: account creation)
	Ongoing  bool
	Finished bool
	Sort     string // "dateAsc" | "dateDesc"; Lichess defaults to dateDesc
}

// UserGames implements API. opening and accuracy are always requested;
// evals and clocks are deliberately never requested (spec §3.4/§7.1: no
// metric uses the per-ply analysis array, and per-move clock data is
// meaningless for a days-per-move game — both would only bloat
// raw_payload).
func (c *Client) UserGames(
	ctx context.Context,
	username string,
	opts UserGamesOptions,
) (*GameStream, error) {
	q := url.Values{}
	if opts.PerfType != "" {
		q.Set("perfType", opts.PerfType)
	}
	if opts.Rated != nil {
		q.Set("rated", strconv.FormatBool(*opts.Rated))
	}
	if !opts.Since.IsZero() {
		q.Set("since", strconv.FormatInt(opts.Since.UnixMilli(), 10))
	}
	q.Set("ongoing", strconv.FormatBool(opts.Ongoing))
	q.Set("finished", strconv.FormatBool(opts.Finished))
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	q.Set("opening", "true")
	q.Set("accuracy", "true")

	resp, err := c.do(
		ctx,
		http.MethodGet,
		"/api/games/user/"+url.PathEscape(username),
		q,
		nil,
		"",
		"application/x-ndjson",
	)
	if err != nil {
		return nil, err
	}
	return newGameStream(resp.Body), nil
}

// GamesByID implements API, batching into groups of gamesBatchSize and
// chaining them behind one stream so callers never see the batch
// boundary.
func (c *Client) GamesByID(
	ctx context.Context,
	ids []string,
) (*GameStream, error) {
	var batches [][]string
	for len(ids) > 0 {
		n := gamesBatchSize
		if n > len(ids) {
			n = len(ids)
		}
		batches = append(batches, ids[:n])
		ids = ids[n:]
	}

	q := url.Values{"opening": {"true"}, "accuracy": {"true"}}

	idx := 0
	fetch := func() (io.ReadCloser, error) {
		if idx >= len(batches) {
			return nil, nil
		}
		batch := batches[idx]
		idx++
		resp, err := c.do(
			ctx,
			http.MethodPost,
			"/api/games/export/_ids",
			q,
			[]byte(strings.Join(batch, ",")),
			"text/plain",
			"application/x-ndjson",
		)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	}

	first, err := fetch()
	if err != nil {
		return nil, err
	}
	if first == nil {
		return newGameStream(io.NopCloser(strings.NewReader(""))), nil
	}
	s := newGameStream(first)
	s.next = fetch
	return s, nil
}

// ExportGame implements API. It exists for an admin "re-fetch this
// game" command (spec §3.4) — the regular sync jobs always know a
// game's id already, from a pairing or a batch export, and use GamesByID
// instead.
func (c *Client) ExportGame(ctx context.Context, gameID string) (Game, error) {
	q := url.Values{"opening": {"true"}, "accuracy": {"true"}}
	resp, err := c.do(
		ctx,
		http.MethodGet,
		"/game/export/"+url.PathEscape(gameID),
		q,
		nil,
		"",
		"application/json",
	)
	if err != nil {
		return Game{}, err
	}
	defer resp.Body.Close()
	var g Game
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return Game{}, fmt.Errorf("lichess: decode game export: %w", err)
	}
	return g, nil
}

// GameStream reads newline-delimited games one at a time, mirroring
// bufio.Scanner's pull idiom: call Next, then Game, until Next returns
// false; check Err afterwards. It never loads a whole response into
// memory — a user with a very long game history streams the same way as
// one with three games.
type GameStream struct {
	body    io.Closer
	scanner *bufio.Scanner
	cur     Game
	err     error

	// next fetches the next batch's body when the current one is
	// exhausted (nil for a single-request stream). Returning (nil, nil)
	// means there are no more batches.
	next func() (io.ReadCloser, error)
}

func newGameStream(body io.ReadCloser) *GameStream {
	return &GameStream{body: body, scanner: newLineScanner(body)}
}

func newLineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	// A long, fully-analysed game's ndjson line can exceed the default
	// 64KB token size; 4MB is comfortably beyond anything a real game
	// produces.
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return scanner
}

// Next advances to the next game, fetching the next batch transparently
// if the current one is exhausted. It returns false at the end of the
// stream or on error — check Err to tell them apart.
func (s *GameStream) Next() bool {
	for {
		for s.scanner.Scan() {
			line := bytes.TrimSpace(s.scanner.Bytes())
			if len(line) == 0 {
				continue // Lichess's ndjson stream can include blank keep-alive lines
			}
			if err := json.Unmarshal(line, &s.cur); err != nil {
				s.err = fmt.Errorf("lichess: decode game: %w", err)
				return false
			}
			return true
		}
		if err := s.scanner.Err(); err != nil {
			s.err = fmt.Errorf("lichess: read game stream: %w", err)
			return false
		}
		s.body.Close()
		if s.next == nil {
			return false
		}
		nextBody, err := s.next()
		if err != nil {
			s.err = err
			return false
		}
		if nextBody == nil {
			return false
		}
		s.body = nextBody
		s.scanner = newLineScanner(nextBody)
	}
}

// Game returns the game decoded by the most recent successful Next.
func (s *GameStream) Game() Game { return s.cur }

// Err returns the error that stopped iteration, if Next returned false
// because of one rather than because the stream is simply exhausted.
func (s *GameStream) Err() error { return s.err }

// Close releases the current underlying response body. Safe to call
// even after Next has already closed it at end-of-stream.
func (s *GameStream) Close() error { return s.body.Close() }
