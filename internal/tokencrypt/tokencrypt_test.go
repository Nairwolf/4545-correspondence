package tokencrypt

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	keyA = "0000000000000000000000000000000000000000000000000000000000000001"
	keyB = "0000000000000000000000000000000000000000000000000000000000000002"
)

func TestParseKey(t *testing.T) {
	_, err := ParseKey(keyA)
	require.NoError(t, err)

	_, err = ParseKey("not hex")
	assert.ErrorContains(t, err, "not hex")

	_, err = ParseKey("abcd")
	assert.ErrorContains(t, err, "2 bytes, want 32")
}

func TestSealOpen(t *testing.T) {
	k, err := ParseKey(keyA)
	require.NoError(t, err)
	const token = Secret("lio_abcdefghijklmnopqrstuvwxyz")

	sealed, err := k.Seal(token)
	require.NoError(t, err)
	assert.False(t, bytes.Contains(sealed, []byte(token)), "plaintext must not appear in the sealed bytes")

	got, err := k.Open(sealed)
	require.NoError(t, err)
	assert.Equal(t, token, got)

	t.Run("fresh nonce each time", func(t *testing.T) {
		again, err := k.Seal(token)
		require.NoError(t, err)
		assert.NotEqual(t, sealed, again)
	})

	t.Run("tampered", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		bad[len(bad)-1] ^= 0x01
		_, err := k.Open(bad)
		assert.ErrorIs(t, err, ErrInvalid)
	})

	t.Run("truncated", func(t *testing.T) {
		_, err := k.Open(sealed[:3])
		assert.ErrorIs(t, err, ErrInvalid)
	})

	t.Run("wrong key", func(t *testing.T) {
		other, err := ParseKey(keyB)
		require.NoError(t, err)
		_, err = other.Open(sealed)
		assert.ErrorIs(t, err, ErrInvalid)
	})

	t.Run("empty secret round-trips", func(t *testing.T) {
		sealed, err := k.Seal("")
		require.NoError(t, err)
		got, err := k.Open(sealed)
		require.NoError(t, err)
		assert.Equal(t, Secret(""), got)
	})
}

func TestSecretIsRedacted(t *testing.T) {
	s := Secret("lio_secret")

	for _, verb := range []string{"%s", "%v", "%q", "%+v"} {
		out := fmt.Sprintf(verb, s)
		assert.NotContains(t, out, "lio_secret", verb)
		assert.Contains(t, out, "redacted", verb)
	}

	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("token", "value", s)
	assert.NotContains(t, buf.String(), "lio_secret")
	assert.Contains(t, buf.String(), "redacted")

	// The one sanctioned way out is an explicit conversion.
	assert.Equal(t, "lio_secret", string(s))
}
