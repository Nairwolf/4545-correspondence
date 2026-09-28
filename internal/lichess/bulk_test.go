package lichess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// createdBulk is the documented creation example, as Lichess's server
// writes it for a correspondence bulk: a correspondence time control
// instead of a clock, and the message echoed back.
const createdBulk = `{
	"id": "QO9p9lUM",
	"games": [
		{"id": "XVfy5rp3", "white": "sai", "black": "nushi"},
		{"id": "Hm3Yq0b1", "white": "nushi", "black": "kaa"}
	],
	"variant": "standard",
	"rated": true,
	"pairAt": 1789845990610,
	"startClocksAt": null,
	"scheduledAt": 1789845990610,
	"pairedAt": null,
	"correspondence": {"daysPerTurn": 2},
	"message": "Round 203: your game with {opponent} is ready: {game}"
}`

func testBulkRequest() BulkPairingRequest {
	return BulkPairingRequest{
		Pairs: []BulkPair{
			{White: "lip_sai", Black: "lip_nushi"},
			{White: "lip_nushi", Black: "lip_kaa"},
		},
		Days:    2,
		Rated:   true,
		Message: "Round 203: your game with {opponent} is ready: {game}",
	}
}

func TestCreateBulkPairing_SendsTheFormAsTheOrganiser(t *testing.T) {
	var form url.Values
	var method, authHeader, contentType string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/bulk-pairing", r.URL.Path)
		method = r.Method
		authHeader = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, createdBulk)
	})
	client.token = "app-token" // must NOT be what the bulk is created as

	bulk, err := client.CreateBulkPairing(context.Background(), "lip_organiser", testBulkRequest())
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "Bearer lip_organiser", authHeader)
	assert.Equal(t, "application/x-www-form-urlencoded", contentType)

	assert.Equal(t, "lip_sai:lip_nushi,lip_nushi:lip_kaa", form.Get("players"),
		"white first; the double-game player appears in two pairs of the same bulk")
	assert.Equal(t, "2", form.Get("days"))
	assert.Equal(t, "true", form.Get("rated"))
	assert.Equal(t, "standard", form.Get("variant"))
	assert.Equal(t, "Round 203: your game with {opponent} is ready: {game}", form.Get("message"))
	assert.False(t, form.Has("pairAt"), "no pairAt: the games are created at once (spec §6.3)")
	for _, field := range []string{"clock.limit", "clock.increment", "startClocksAt"} {
		assert.False(t, form.Has(field), "%s is not sent for a correspondence bulk", field)
	}

	assert.Equal(t, "QO9p9lUM", bulk.ID)
	assert.Equal(t, []BulkGame{
		{ID: "XVfy5rp3", White: "sai", Black: "nushi"},
		{ID: "Hm3Yq0b1", White: "nushi", Black: "kaa"},
	}, bulk.Games)
	assert.True(t, bulk.Rated)
	assert.Nil(t, bulk.PairedAt)
	require.NotNil(t, bulk.Correspondence)
	assert.Equal(t, 2, bulk.Correspondence.DaysPerTurn)
	assert.Equal(t, time.UnixMilli(1789845990610), bulk.ScheduledAtTime())
}

func TestCreateBulkPairing_OmitsAnEmptyMessage(t *testing.T) {
	var form url.Values
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		fmt.Fprint(w, createdBulk)
	})
	req := testBulkRequest()
	req.Message = ""
	req.Rated = false

	_, err := client.CreateBulkPairing(context.Background(), "lip_organiser", req)
	require.NoError(t, err)
	assert.False(t, form.Has("message"), "no message: Lichess sends its default one")
	assert.Equal(t, "false", form.Get("rated"))
}

func TestCreateBulkPairing_RefusesAnEmptyBulkWithoutCallingLichess(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls++ })

	_, err := client.CreateBulkPairing(context.Background(), "lip_organiser", BulkPairingRequest{Days: 2})
	require.Error(t, err)
	assert.Zero(t, calls)
}

// TestCreateBulkPairing_Rejections covers every 400 body Lichess's
// server sends for a bulk. Whatever the shape, the error is a
// *BulkRejection and its text never contains a token.
func TestCreateBulkPairing_Rejections(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     BulkRejection
		wantText string
	}{
		{
			name: "bad tokens, keyed by the raw token",
			body: `{"tokens": {"lip_nushi": "No such token", "lip_kaa": "Missing scope challenge:write", "lip_sai": "No such token"}}`,
			want: BulkRejection{Tokens: map[tokencrypt.Secret]string{
				"lip_nushi": "No such token",
				"lip_kaa":   "Missing scope challenge:write",
				"lip_sai":   "No such token",
			}},
			wantText: `lichess: bulk pairing refused: 3 tokens refused ("No such token" ×2, "Missing scope challenge:write" ×1)`,
		},
		{
			name: "a reason quoting the token is redacted",
			body: `{"tokens": {"lip_kaa": "Token lip_kaa is revoked"}}`,
			want: BulkRejection{Tokens: map[tokencrypt.Secret]string{
				"lip_kaa": "Token [redacted] is revoked",
			}},
			wantText: `lichess: bulk pairing refused: 1 token refused ("Token [redacted] is revoked" ×1)`,
		},
		{
			name:     "duplicate users",
			body:     `{"duplicateUsers": ["nushi"]}`,
			want:     BulkRejection{DuplicateUsers: []string{"nushi"}},
			wantText: "lichess: bulk pairing refused: paired more than once: nushi",
		},
		{
			name:     "a plain error",
			body:     `{"error": "Already too many bulks queued"}`,
			want:     BulkRejection{Message: "Already too many bulks queued"},
			wantText: "lichess: bulk pairing refused: Already too many bulks queued",
		},
		{
			name:     "a refused form",
			body:     `{"error": {"players": ["Not enough tokens"], "days": ["error.invalid"]}}`,
			want:     BulkRejection{Message: "days: error.invalid; players: Not enough tokens"},
			wantText: "lichess: bulk pairing refused: days: error.invalid; players: Not enough tokens",
		},
		{
			name:     "not JSON",
			body:     `<html>Bad Request</html>`,
			want:     BulkRejection{Message: "unreadable response"},
			wantText: "lichess: bulk pairing refused: unreadable response",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, tc.body)
			})

			_, err := client.CreateBulkPairing(context.Background(), "lip_organiser", testBulkRequest())
			var rejection *BulkRejection
			require.ErrorAs(t, err, &rejection)
			assert.Equal(t, tc.want, *rejection)
			assert.Equal(t, tc.wantText, err.Error())
			assertNoToken(t, err.Error())
			assertNoToken(t, fmt.Sprintf("%v %+v", rejection, *rejection))
		})
	}
}

// assertNoToken fails if s contains any token used by these tests.
func assertNoToken(t *testing.T, s string) {
	t.Helper()
	for _, token := range []string{"lip_sai", "lip_nushi", "lip_kaa", "lip_organiser"} {
		assert.NotContains(t, s, token)
	}
}

func TestCreateBulkPairing_OtherRefusalsAreAPIErrors(t *testing.T) {
	var slept []time.Duration
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error": "Ratelimited! Max games per 10 minutes: 500"}`)
	}, WithSleepFunc(func(d time.Duration) { slept = append(slept, d) }))

	_, err := client.CreateBulkPairing(context.Background(), "lip_organiser", testBulkRequest())
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
	assert.Equal(t, "Ratelimited! Max games per 10 minutes: 500", apiErr.Message)
	assert.Len(t, slept, 1, "retried once after the 429, like every call")

	var rejection *BulkRejection
	assert.False(t, errors.As(err, &rejection))
}

func TestCreateBulkPairing_TransportErrorNamesNoToken(t *testing.T) {
	client := New("", WithBaseURL("http://127.0.0.1:1")) // nothing listens there

	_, err := client.CreateBulkPairing(context.Background(), "lip_organiser", testBulkRequest())
	require.Error(t, err)
	assertNoToken(t, err.Error())
}

func TestListBulkPairings(t *testing.T) {
	var method, authHeader string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/bulk-pairing", r.URL.Path)
		method, authHeader = r.Method, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		// The documented list example (a real-time bulk, with a clock)
		// and a correspondence bulk whose games have been created.
		fmt.Fprint(w, `{"bulks": [
			{"id": "Qx1corr2", "games": [{"id": "Hm3Yq0b1", "white": "nushi", "black": "kaa"}],
			 "variant": "standard", "rated": true, "pairAt": 1789845990700, "startClocksAt": null,
			 "scheduledAt": 1789845990700, "pairedAt": 1789845990750, "correspondence": {"daysPerTurn": 3}},
			{"id": "QO9p9lUM", "games": [{"id": "XVfy5rp3", "white": "sai", "black": "nushi"}],
			 "variant": "standard", "rated": false, "pairAt": 1789845990610, "startClocksAt": null,
			 "scheduledAt": 1789845990610, "pairedAt": 1789845990642, "clock": {"limit": 300, "increment": 0}}
		]}`)
	})

	bulks, err := client.ListBulkPairings(context.Background(), "lip_organiser")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "Bearer lip_organiser", authHeader)

	require.Len(t, bulks, 2)
	assert.Equal(t, "Qx1corr2", bulks[0].ID)
	require.NotNil(t, bulks[0].PairedAt)
	assert.Equal(t, int64(1789845990750), *bulks[0].PairedAt)
	require.NotNil(t, bulks[0].Correspondence)
	assert.Equal(t, 3, bulks[0].Correspondence.DaysPerTurn)
	assert.Equal(t, []BulkGame{{ID: "XVfy5rp3", White: "sai", Black: "nushi"}}, bulks[1].Games)
	assert.Nil(t, bulks[1].Correspondence, "a real-time bulk has a clock instead")
}

func TestListBulkPairings_Unauthorised(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error": "No such token"}`)
	})

	_, err := client.ListBulkPairings(context.Background(), "lip_organiser")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusUnauthorized, apiErr.StatusCode)
	assertNoToken(t, err.Error())
}
