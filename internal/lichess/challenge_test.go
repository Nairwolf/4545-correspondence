package lichess

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateChallenge_SendsTheFormAsTheChallenger(t *testing.T) {
	var form url.Values
	var path, authHeader, contentType string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authHeader = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		w.Header().Set("Content-Type", "application/json")
		// The documented example, for a correspondence challenge.
		fmt.Fprint(w, `{
			"id": "C8fNpisS",
			"url": "https://lichess.org/C8fNpisS",
			"status": "created",
			"challenger": {"name": "Bobby", "id": "bobby", "rating": 1657},
			"destUser": {"name": "Mary", "id": "mary", "rating": 1079},
			"variant": {"key": "standard", "name": "Standard", "short": "Std"},
			"rated": true,
			"speed": "correspondence",
			"timeControl": {"type": "correspondence", "daysPerTurn": 2},
			"color": "black",
			"finalColor": "black",
			"perf": {"icon": "", "name": "Correspondence"},
			"direction": "out"
		}`)
	})
	client.token = "app-token"

	ch, err := client.CreateChallenge(context.Background(), "lip_bobby", "Mary", ChallengeRequest{
		Days:  2,
		Rated: true,
		Color: ColorBlack,
	})
	require.NoError(t, err)

	assert.Equal(t, "/api/challenge/Mary", path)
	assert.Equal(t, "Bearer lip_bobby", authHeader)
	assert.Equal(t, "application/x-www-form-urlencoded", contentType)
	assert.Equal(t, "2", form.Get("days"))
	assert.Equal(t, "true", form.Get("rated"))
	assert.Equal(t, "black", form.Get("color"), "the colour the pairing assigned, never random")
	assert.Equal(t, "standard", form.Get("variant"))
	assert.False(t, form.Has("clock.limit"))
	assert.False(t, form.Has("keepAliveStream"))

	assert.Equal(t, "C8fNpisS", ch.ID)
	assert.Equal(t, "created", ch.Status)
	assert.Equal(t, "bobby", ch.Challenger.ID)
	require.NotNil(t, ch.DestUser)
	assert.Equal(t, "mary", ch.DestUser.ID)
	assert.Equal(t, "correspondence", ch.TimeControl.Type)
	assert.Equal(t, 2, ch.TimeControl.DaysPerTurn)
	assert.Equal(t, "black", ch.FinalColor)
}

func TestCreateChallenge_RefusalNamesTheReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"a sentence", `{"error": "No such user: ghost"}`, "No such user: ghost"},
		{
			// Lichess's challenge form error: the fields both under
			// "error" and at the top level, for old clients.
			"a refused form",
			`{"error": {"days": ["Invalid value"]}, "days": ["Invalid value"]}`,
			"days: Invalid value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, tc.body)
			})

			_, err := client.CreateChallenge(context.Background(), "lip_bobby", "ghost", ChallengeRequest{
				Days:  4,
				Color: ColorWhite,
			})
			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
			assert.Equal(t, tc.want, apiErr.Message)
			assert.NotContains(t, err.Error(), "lip_bobby")
		})
	}
}

func TestCancelChallenge(t *testing.T) {
	var method, path, authHeader string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		authHeader = r.Header.Get("Authorization")
		assert.Empty(t, r.URL.Query().Get("opponentToken"), "never sent: it would let us abort a game in play")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok": true}`)
	})

	require.NoError(t, client.CancelChallenge(context.Background(), "lip_bobby", "C8fNpisS"))
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/api/challenge/C8fNpisS/cancel", path)
	assert.Equal(t, "Bearer lip_bobby", authHeader)
}

func TestCancelChallenge_NotFound(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error": "Not found"}`)
	})

	err := client.CancelChallenge(context.Background(), "lip_bobby", "gone1234")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}
