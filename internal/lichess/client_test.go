package lichess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	allOpts := append([]Option{WithBaseURL(srv.URL)}, opts...)
	return New("", allOpts...)
}

func TestUsersByID_DecodesProvisionalAbsentAsFalse(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/users", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		// "established" has no "prov" key at all, matching the verified
		// API behaviour that prov is omitted (not false) when not
		// provisional.
		fmt.Fprint(w, `[
			{"id":"provisional","username":"Provisional","perfs":{"correspondence":{"games":3,"rating":1500,"rd":300,"prov":true}}},
			{"id":"established","username":"Established","perfs":{"correspondence":{"games":400,"rating":1900,"rd":45}}}
		]`)
	})

	users, err := client.UsersByID(context.Background(), []string{"provisional", "established"})
	require.NoError(t, err)
	require.Len(t, users, 2)

	assert.True(t, users[0].Perfs.Correspondence.Provisional)
	assert.False(t, users[1].Perfs.Correspondence.Provisional)
	assert.Equal(t, 1900, users[1].Perfs.Correspondence.Rating)
}

func TestUsersByID_BatchesOver300(t *testing.T) {
	var requests [][]string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, strings.Split(string(body), ","))
		var resp []User
		for _, id := range strings.Split(string(body), ",") {
			resp = append(resp, User{ID: id, Username: id})
		}
		json.NewEncoder(w).Encode(resp)
	})

	ids := make([]string, 650)
	for i := range ids {
		ids[i] = fmt.Sprintf("user%d", i)
	}

	users, err := client.UsersByID(context.Background(), ids)
	require.NoError(t, err)

	require.Len(t, requests, 3, "650 ids at 300/batch should be 3 requests")
	assert.Len(t, requests[0], 300)
	assert.Len(t, requests[1], 300)
	assert.Len(t, requests[2], 50)

	require.Len(t, users, 650)
	assert.Equal(t, "user0", users[0].ID)
	assert.Equal(t, "user649", users[649].ID)
}

func TestUserGames_StreamsNDJSON(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/games/user/somebody", r.URL.Path)
		assert.Equal(t, "correspondence", r.URL.Query().Get("perfType"))
		assert.Equal(t, "true", r.URL.Query().Get("rated"))
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"id":"game1","status":"resign","winner":"white"}`)
		fmt.Fprintln(w, `{"id":"game2","status":"draw"}`)
		fmt.Fprintln(w, `{"id":"game3","status":"mate","winner":"black"}`)
	})

	rated := true
	stream, err := client.UserGames(context.Background(), "somebody", UserGamesOptions{
		PerfType: "correspondence",
		Rated:    &rated,
		Finished: true,
	})
	require.NoError(t, err)
	defer stream.Close()

	var ids []string
	for stream.Next() {
		ids = append(ids, stream.Game().ID)
	}
	require.NoError(t, stream.Err())
	assert.Equal(t, []string{"game1", "game2", "game3"}, ids)
}

func TestGamesByID_ChainsAcrossBatches(t *testing.T) {
	var requestSizes []int
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Regression test: an earlier version of GamesByID sent no query
		// params at all, silently falling back to Lichess's defaults
		// (accuracy/opening off) and losing exactly the data spec §7.1
		// needs — caught by live verification, not by a unit test, which
		// is why this assertion now exists.
		assert.Equal(t, "true", r.URL.Query().Get("opening"))
		assert.Equal(t, "true", r.URL.Query().Get("accuracy"))
		assert.Empty(t, r.URL.Query().Get("clocks"), "clocks is deliberately never requested (spec §3.4)")

		body, _ := io.ReadAll(r.Body)
		ids := strings.Split(string(body), ",")
		requestSizes = append(requestSizes, len(ids))
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, id := range ids {
			fmt.Fprintf(w, `{"id":%q,"status":"mate"}`+"\n", id)
		}
	})

	ids := make([]string, 305)
	for i := range ids {
		ids[i] = fmt.Sprintf("g%d", i)
	}

	stream, err := client.GamesByID(context.Background(), ids)
	require.NoError(t, err)
	defer stream.Close()

	var got []string
	for stream.Next() {
		got = append(got, stream.Game().ID)
	}
	require.NoError(t, stream.Err())

	assert.Equal(t, []int{300, 5}, requestSizes)
	require.Len(t, got, 305)
	assert.Equal(t, "g0", got[0])
	assert.Equal(t, "g304", got[304])
}

func TestDo_RetriesOnce429ThenSucceeds(t *testing.T) {
	calls := 0
	var slept []time.Duration
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":"slow down"}`)
			return
		}
		json.NewEncoder(w).Encode([]User{{ID: "ok"}})
	}, WithSleepFunc(func(d time.Duration) { slept = append(slept, d) }))

	users, err := client.UsersByID(context.Background(), []string{"x"})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	require.Len(t, slept, 1)
	assert.Equal(t, 60*time.Second, slept[0])
	require.Len(t, users, 1)
	assert.Equal(t, "ok", users[0].ID)
}

func TestDo_429WaitAbortsWhenContextIsCancelled(t *testing.T) {
	// The wait is an hour, so only a context-aware sleep can return
	// promptly — and it must not spend a second attempt on a context
	// that is already done.
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
	}, WithRetryWait(time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.UsersByID(ctx, []string{"x"})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, calls, "the retry must not be attempted")
	assert.Less(t, elapsed, time.Second, "the wait must abort on cancellation")
}

func TestDo_ReturnsAPIErrorOnNon2xx(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"bad request"}`)
	})

	_, err := client.UsersByID(context.Background(), []string{"x"})
	require.Error(t, err)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, "bad request", apiErr.Message)
}

func TestExportGame_DecodesSingleObject(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/game/export/abc12345", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"abc12345","status":"resign","winner":"white","daysPerTurn":2}`)
	})

	g, raw, err := client.ExportGame(context.Background(), "abc12345")
	require.NoError(t, err)
	assert.Equal(t, "abc12345", g.ID)
	assert.Equal(t, "resign", g.Status)
	assert.Equal(t, 2, g.DaysPerTurn)
	assert.JSONEq(t, `{"id":"abc12345","status":"resign","winner":"white","daysPerTurn":2}`, string(raw))
}

func TestGameStream_RawIsTheExactLineAndSurvivesAcrossNext(t *testing.T) {
	// Two lines, each carrying a field lichess.Game doesn't declare, so
	// a re-encode of Game() would visibly differ from Raw(). The second
	// line is longer than the first so a buffer-reuse bug (Raw pointing
	// into the scanner's buffer) would show up as corrupted bytes.
	line1 := `{"id":"g1","undeclared":1}`
	line2 := `{"id":"g2","undeclared":2,"padding":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, line1)
		fmt.Fprintln(w, line2)
	})

	stream, err := client.UserGames(context.Background(), "x", UserGamesOptions{})
	require.NoError(t, err)
	defer stream.Close()

	require.True(t, stream.Next())
	assert.Equal(t, "g1", stream.Game().ID)
	firstRaw := stream.Raw() // deliberately NOT copied: the next Next must not corrupt a copy we take now
	firstCopy := string(firstRaw)
	assert.Equal(t, line1, firstCopy)

	require.True(t, stream.Next())
	assert.Equal(t, "g2", stream.Game().ID)
	assert.Equal(t, line2, string(stream.Raw()))
	assert.Equal(t, line1, firstCopy, "a string taken from Raw() before the second Next is unaffected")

	require.False(t, stream.Next())
	require.NoError(t, stream.Err())
}

func TestGameStream_SkipsBlankKeepAliveLines(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"id":"g1"}`)
		fmt.Fprintln(w, ``)
		fmt.Fprintln(w, `{"id":"g2"}`)
	})

	stream, err := client.UserGames(context.Background(), "x", UserGamesOptions{})
	require.NoError(t, err)
	defer stream.Close()

	var ids []string
	for stream.Next() {
		ids = append(ids, stream.Game().ID)
	}
	require.NoError(t, stream.Err())
	assert.Equal(t, []string{"g1", "g2"}, ids)
}

func TestAccount_DecodesFixtureAndSendsTheGivenBearer(t *testing.T) {
	fixture, err := os.ReadFile("testdata/user_thibault_live.json")
	require.NoError(t, err)

	var authHeader string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/account", r.URL.Path)
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write(fixture)
	})
	client.token = "app-token" // must NOT be what /api/account is called with

	a, raw, err := client.Account(context.Background(), "lio_player")
	require.NoError(t, err)
	assert.Equal(t, "Bearer lio_player", authHeader)
	assert.Equal(t, fixture, raw, "raw must be Lichess's bytes, untouched")

	assert.Equal(t, "thibault", a.ID)
	assert.Equal(t, 2010, a.CreatedAtTime().UTC().Year())
	assert.False(t, a.Disabled, "absent in the fixture → false")
	assert.False(t, a.TOSViolation)
	assert.True(t, a.Verified)
	assert.Equal(t, 21328, a.Count.Rated)
	require.NotNil(t, a.Perfs.Correspondence)
	assert.Equal(t, 1942, a.Perfs.Correspondence.Rating)
}

func TestAccount_FlagsPresentWhenTrue(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","username":"X","disabled":true,"tosViolation":true,"count":{"all":3}}`)
	})
	a, _, err := client.Account(context.Background(), "t")
	require.NoError(t, err)
	assert.True(t, a.Disabled)
	assert.True(t, a.TOSViolation)
	assert.Equal(t, 0, a.Count.Rated, "absent rated count decodes as 0")
}

func TestRevokeToken_SendsDeleteAsTheGivenBearer(t *testing.T) {
	var method, authHeader string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/token", r.URL.Path)
		method, authHeader = r.Method, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	})
	client.token = "app-token"

	require.NoError(t, client.RevokeToken(context.Background(), "lio_player"))
	assert.Equal(t, http.MethodDelete, method)
	assert.Equal(t, "Bearer lio_player", authHeader)
}
