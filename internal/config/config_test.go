package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validAuth() Auth {
	return Auth{
		LichessClientID:    "ic.example.org",
		LichessRedirectURI: "https://ic.example.org/auth/lichess/callback",
		TokenEncryptionKey: strings.Repeat("ab", 32),
		SessionSecret:      strings.Repeat("s", 32),
	}
}

func TestAuthValidate(t *testing.T) {
	require.NoError(t, validAuth().Validate())

	tests := []struct {
		name   string
		mutate func(*Auth)
		want   string
	}{
		{"missing client id", func(a *Auth) { a.LichessClientID = "" }, "LICHESS_CLIENT_ID"},
		{"missing redirect", func(a *Auth) { a.LichessRedirectURI = "" }, "LICHESS_REDIRECT_URI is required"},
		{"relative redirect", func(a *Auth) { a.LichessRedirectURI = "/auth/lichess/callback" }, "absolute"},
		{"short key", func(a *Auth) { a.TokenEncryptionKey = "abcd" }, "TOKEN_ENCRYPTION_KEY"},
		{"non-hex key", func(a *Auth) { a.TokenEncryptionKey = strings.Repeat("zz", 32) }, "TOKEN_ENCRYPTION_KEY"},
		{"short session secret", func(a *Auth) { a.SessionSecret = "short" }, "SESSION_SECRET"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := validAuth()
			tc.mutate(&a)
			err := a.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("reports every problem at once", func(t *testing.T) {
		err := Auth{}.Validate()
		require.Error(t, err)
		for _, name := range []string{"LICHESS_CLIENT_ID", "LICHESS_REDIRECT_URI", "TOKEN_ENCRYPTION_KEY", "SESSION_SECRET"} {
			assert.Contains(t, err.Error(), name)
		}
	})
}

func TestSecureCookies(t *testing.T) {
	a := validAuth()
	assert.True(t, a.SecureCookies())
	a.LichessRedirectURI = "http://localhost:8080/auth/lichess/callback"
	assert.False(t, a.SecureCookies())
}

func TestSplitList(t *testing.T) {
	assert.Nil(t, splitList(""))
	assert.Equal(t, []string{"a", "b"}, splitList(" a, b,, "))
}
