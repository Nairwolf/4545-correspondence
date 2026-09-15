package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthStateCookie(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	st := oauthState{State: "abc", Verifier: "vvv", Intent: intentJoin, Expires: now.Add(10 * time.Minute).Unix()}

	value, err := signState(secret, st)
	require.NoError(t, err)

	got, ok := verifyState(secret, value, now)
	require.True(t, ok)
	assert.Equal(t, st, got)

	t.Run("expired", func(t *testing.T) {
		_, ok := verifyState(secret, value, now.Add(11*time.Minute))
		assert.False(t, ok)
	})

	t.Run("wrong secret", func(t *testing.T) {
		_, ok := verifyState([]byte(strings.Repeat("x", 32)), value, now)
		assert.False(t, ok)
	})

	t.Run("tampered payload", func(t *testing.T) {
		enc, sig, _ := strings.Cut(value, ".")
		forged, _ := signState(secret, oauthState{State: "zzz", Intent: intentLogin, Expires: st.Expires})
		forgedEnc, _, _ := strings.Cut(forged, ".")
		_, ok := verifyState(secret, forgedEnc+"."+sig, now)
		assert.False(t, ok, "payload from one cookie with the signature of another")
		_ = enc
	})

	t.Run("garbage", func(t *testing.T) {
		for _, v := range []string{"", ".", "abc", "abc.def"} {
			_, ok := verifyState(secret, v, now)
			assert.False(t, ok, v)
		}
	})
}
