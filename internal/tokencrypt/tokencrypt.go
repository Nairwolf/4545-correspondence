// Package tokencrypt encrypts player OAuth tokens at rest (spec §11:
// "encrypted at rest with a key held outside the database; never logged,
// never returned by any API response, never rendered in any template").
//
// It is deliberately tiny: AES-256-GCM from the standard library, one
// key from the environment, and a string type that refuses to print
// itself. There is no key rotation — rotating TOKEN_ENCRYPTION_KEY
// simply makes every stored token unreadable, and every player
// re-authorises through the one-click flow (spec §3.1).
package tokencrypt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
)

// Secret is a plaintext token. It is a distinct type so that it cannot
// be passed where an ordinary string is expected by accident, and so
// that fmt and slog print "[redacted]" instead of the value. The only
// way to get the bytes out is an explicit string(s) conversion — which
// should happen in exactly one place: setting the Authorization header
// inside internal/lichess.
type Secret string

// String implements fmt.Stringer: %s, %v and %q all print the
// placeholder, never the token.
func (Secret) String() string { return "[redacted]" }

// LogValue implements slog.LogValuer, so a Secret passed as a log
// attribute is redacted even when the handler bypasses fmt.
func (Secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Key is a parsed encryption key ready to seal and open tokens.
type Key struct {
	aead cipher.AEAD
}

// ParseKey builds a Key from TOKEN_ENCRYPTION_KEY's value: 64 hex
// characters encoding the 32 bytes AES-256 needs.
func ParseKey(hexKey string) (Key, error) {
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return Key{}, fmt.Errorf("tokencrypt: key is not hex: %w", err)
	}
	if len(raw) != 32 {
		return Key{}, fmt.Errorf("tokencrypt: key is %d bytes, want 32", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return Key{}, fmt.Errorf("tokencrypt: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Key{}, fmt.Errorf("tokencrypt: %w", err)
	}
	return Key{aead: aead}, nil
}

// Seal encrypts s. The result is nonce || ciphertext || tag, a single
// opaque byte string for one bytea column; every call uses a fresh
// random nonce, so sealing the same token twice yields different bytes.
func (k Key) Seal(s Secret) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("tokencrypt: nonce: %w", err)
	}
	return k.aead.Seal(nonce, nonce, []byte(s), nil), nil
}

// ErrInvalid is returned by Open for bytes that are not a valid sealed
// token under this key: truncated, tampered with, or sealed under a
// different key. The three cases are deliberately indistinguishable.
var ErrInvalid = errors.New("tokencrypt: ciphertext is not valid under this key")

// Open decrypts bytes produced by Seal.
func (k Key) Open(sealed []byte) (Secret, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n {
		return "", ErrInvalid
	}
	plain, err := k.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", ErrInvalid
	}
	return Secret(plain), nil
}
