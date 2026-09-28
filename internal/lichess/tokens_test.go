package lichess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

func TestTestTokens_DecodesTheDocumentedExample(t *testing.T) {
	var body, authHeader, contentType string
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/token/test", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		authHeader = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"lip_jose": {"userId": "jose", "scopes": "challenge:read,challenge:write,msg:write", "expires": null},
			"lip_ana": {"userId": "ana", "scopes": "challenge:write", "expires": 1812345678000},
			"lip_bare": {"userId": "bare", "scopes": "", "expires": null},
			"lip_badToken": null
		}`)
	})
	client.token = "app-token" // the call is unauthenticated: never sent

	got, err := client.TestTokens(context.Background(), []tokencrypt.Secret{
		"lip_jose", "lip_ana", "lip_bare", "lip_badToken", "lip_jose",
	})
	require.NoError(t, err)

	assert.Equal(t, "lip_jose,lip_ana,lip_bare,lip_badToken", body, "each token sent once")
	assert.Empty(t, authHeader)
	assert.Equal(t, "text/plain", contentType)

	require.Len(t, got, 4)
	require.NotNil(t, got["lip_jose"])
	assert.Equal(t, "jose", got["lip_jose"].UserID)
	assert.Equal(t, []string{"challenge:read", "challenge:write", "msg:write"}, got["lip_jose"].Scopes)
	assert.True(t, got["lip_jose"].HasScope(ScopeChallengeWrite))
	assert.True(t, got["lip_jose"].Expires.IsZero(), "null: never expires")

	require.NotNil(t, got["lip_ana"])
	assert.Equal(t, time.UnixMilli(1812345678000), got["lip_ana"].Expires)

	require.NotNil(t, got["lip_bare"])
	assert.Empty(t, got["lip_bare"].Scopes)
	assert.False(t, got["lip_bare"].HasScope(ScopeChallengeWrite))

	info, present := got["lip_badToken"]
	assert.True(t, present, "an invalid token is in the map…")
	assert.Nil(t, info, "…with a nil value")
}

func TestTestTokens_ATokenLichessDoesNotMentionIsAbsent(t *testing.T) {
	// Absent is not the same as invalid: a caller revoking tokens on
	// the strength of this call must not revoke one Lichess said
	// nothing about.
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"lip_jose": null}`)
	})

	got, err := client.TestTokens(context.Background(), []tokencrypt.Secret{"lip_jose", "lip_ana"})
	require.NoError(t, err)
	_, present := got["lip_ana"]
	assert.False(t, present)
}

func TestTestTokens_BatchesOver1000(t *testing.T) {
	var sizes []int
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		batch := strings.Split(string(b), ",")
		sizes = append(sizes, len(batch))
		resp := map[string]any{}
		for _, token := range batch {
			resp[token] = map[string]any{"userId": "u" + token, "scopes": "challenge:write", "expires": nil}
		}
		json.NewEncoder(w).Encode(resp)
	})

	tokens := make([]tokencrypt.Secret, 2005)
	for i := range tokens {
		tokens[i] = tokencrypt.Secret(fmt.Sprintf("lip_%d", i))
	}

	got, err := client.TestTokens(context.Background(), tokens)
	require.NoError(t, err)
	assert.Equal(t, []int{1000, 1000, 5}, sizes)
	require.Len(t, got, 2005)
	assert.Equal(t, "ulip_2004", got["lip_2004"].UserID)
}

func TestTestTokens_NothingToTestMakesNoCall(t *testing.T) {
	calls := 0
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls++ })

	got, err := client.TestTokens(context.Background(), []tokencrypt.Secret{"", ""})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Zero(t, calls)
}
