package lichess

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

const testRedirect = "https://ic.example.org/auth/lichess/callback"

func newTestOAuth(t *testing.T, handler http.HandlerFunc) *OAuth {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewOAuth("ic.example.org", testRedirect, WithOAuthBaseURL(srv.URL))
}

func TestOAuth_AuthCodeURL(t *testing.T) {
	o := NewOAuth("ic.example.org", testRedirect)
	verifier := GenerateVerifier()

	u, err := url.Parse(o.AuthCodeURL("state-123", verifier, []string{"challenge:write"}))
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "lichess.org", u.Host)
	assert.Equal(t, "/oauth", u.Path)

	q := u.Query()
	assert.Equal(t, "code", q.Get("response_type"))
	assert.Equal(t, "ic.example.org", q.Get("client_id"))
	assert.Equal(t, testRedirect, q.Get("redirect_uri"))
	assert.Equal(t, "challenge:write", q.Get("scope"))
	assert.Equal(t, "state-123", q.Get("state"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))

	sum := sha256.Sum256([]byte(verifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), q.Get("code_challenge"))

	t.Run("scopes are space-separated", func(t *testing.T) {
		u, err := url.Parse(o.AuthCodeURL("s", verifier, []string{"challenge:write", "msg:write"}))
		require.NoError(t, err)
		assert.Equal(t, "challenge:write msg:write", u.Query().Get("scope"))
	})
}

func TestOAuth_Exchange(t *testing.T) {
	var form url.Values
	var authHeader string
	o := newTestOAuth(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/api/token", r.URL.Path)
		require.NoError(t, r.ParseForm())
		form = r.PostForm
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token_type":"Bearer","access_token":"lio_abc","expires_in":31536000}`)
	})

	tok, err := o.Exchange(context.Background(), "code-xyz", "verifier-xyz")
	require.NoError(t, err)
	assert.Equal(t, tokencrypt.Secret("lio_abc"), tok.AccessToken)
	assert.False(t, tok.ExpiresAt.IsZero(), "expires_in must become ExpiresAt")

	// Exactly the fields Lichess documents, and nothing that would only
	// make sense for a confidential client.
	assert.Equal(t, "authorization_code", form.Get("grant_type"))
	assert.Equal(t, "code-xyz", form.Get("code"))
	assert.Equal(t, "verifier-xyz", form.Get("code_verifier"))
	assert.Equal(t, testRedirect, form.Get("redirect_uri"))
	assert.Equal(t, "ic.example.org", form.Get("client_id"))
	assert.NotContains(t, form, "client_secret")
	assert.Empty(t, authHeader, "no Basic auth: Lichess clients are public")
}

func TestOAuth_ExchangeRefused(t *testing.T) {
	o := newTestOAuth(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"code expired"}`)
	})

	_, err := o.Exchange(context.Background(), "stale", "v")
	var xe *ExchangeError
	require.ErrorAs(t, err, &xe)
	assert.Equal(t, "invalid_grant", xe.Code)
	assert.Equal(t, "code expired", xe.Description)
	assert.Contains(t, err.Error(), "invalid_grant: code expired")
}
