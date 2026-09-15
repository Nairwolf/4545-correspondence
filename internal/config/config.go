// Package config loads runtime configuration from the environment.
//
// The set is small enough that plain os.LookupEnv reads more clearly than
// a struct-tag/reflection library would (spec §2.2 lists the eventual
// variables; each phase adds the ones it needs here).
package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config holds process-wide configuration read from the environment.
type Config struct {
	// DatabaseURL is a standard libpq/pgx connection string.
	DatabaseURL string

	// ListenAddr is the address the web server binds to.
	ListenAddr string

	// LichessToken is an optional OAuth token used for outbound Lichess API
	// calls. Unauthenticated calls work but are throttled harder (spec
	// §3.4); this is not a per-player token, just a courtesy identity for
	// the app's own read-only polling.
	LichessToken string

	// Auth is what the sign-in flow needs (spec §2.2, §3.1). Load reads
	// it without validating, so the one-shot CLI subcommands keep working
	// with nothing but DATABASE_URL; `serve` calls Auth.Validate.
	Auth Auth
}

// Auth is the identity configuration (Phase 2).
type Auth struct {
	// LichessClientID is the public OAuth client id. Lichess registers
	// nothing and accepts any unique string; the site's hostname is a
	// good choice.
	LichessClientID string

	// LichessRedirectURI is the absolute callback URL, byte-for-byte the
	// same in the authorisation redirect and the token exchange. Its
	// scheme also decides whether cookies are marked Secure.
	LichessRedirectURI string

	// TokenEncryptionKey is the 32-byte AES key that encrypts player
	// tokens at rest, hex-encoded (64 characters). Rotating it makes every
	// stored token unreadable, so every player must re-authorise.
	TokenEncryptionKey string

	// SessionSecret signs the short-lived OAuth-state cookie. Sessions
	// themselves are random server-side ids and do not use it.
	SessionSecret string

	// AdminLichessUsernames is the bootstrap admin list: any of these
	// accounts is made an admin when it signs in.
	AdminLichessUsernames []string
}

// Load reads Config from the environment, returning an error naming any
// missing required variable.
func Load() (Config, error) {
	dbURL, ok := os.LookupEnv("DATABASE_URL")
	if !ok || dbURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required")
	}

	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	return Config{
		DatabaseURL:  dbURL,
		ListenAddr:   listenAddr,
		LichessToken: os.Getenv("LICHESS_TOKEN"),
		Auth: Auth{
			LichessClientID:       os.Getenv("LICHESS_CLIENT_ID"),
			LichessRedirectURI:    os.Getenv("LICHESS_REDIRECT_URI"),
			TokenEncryptionKey:    os.Getenv("TOKEN_ENCRYPTION_KEY"),
			SessionSecret:         os.Getenv("SESSION_SECRET"),
			AdminLichessUsernames: splitList(os.Getenv("ADMIN_LICHESS_USERNAMES")),
		},
	}, nil
}

// Validate checks that everything the sign-in flow needs is present and
// well-formed. It reports every problem at once rather than the first,
// so a fresh deployment is fixed in one round rather than four.
func (a Auth) Validate() error {
	var problems []string

	if a.LichessClientID == "" {
		problems = append(problems, "LICHESS_CLIENT_ID is required")
	}

	if a.LichessRedirectURI == "" {
		problems = append(problems, "LICHESS_REDIRECT_URI is required")
	} else if u, err := url.Parse(a.LichessRedirectURI); err != nil || !u.IsAbs() || u.Host == "" {
		problems = append(problems, "LICHESS_REDIRECT_URI must be an absolute http(s) URL")
	}

	if key, err := hex.DecodeString(a.TokenEncryptionKey); err != nil || len(key) != 32 {
		problems = append(problems, "TOKEN_ENCRYPTION_KEY must be 64 hex characters (32 bytes)")
	}

	if len(a.SessionSecret) < 32 {
		problems = append(problems, "SESSION_SECRET must be at least 32 characters")
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// SecureCookies reports whether the site is served over https — the
// redirect URI is the one place the deployment's public scheme is
// already stated, so cookies follow it rather than a separate flag.
func (a Auth) SecureCookies() bool {
	return strings.HasPrefix(strings.ToLower(a.LichessRedirectURI), "https://")
}

// splitList parses a comma-separated variable, trimming whitespace and
// dropping empty entries so " a, b,, " yields [a b].
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
