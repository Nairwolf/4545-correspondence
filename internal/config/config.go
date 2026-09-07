// Package config loads runtime configuration from the environment.
//
// Phase 1 only reads what the scaffold, migrations and read-only jobs need
// (spec §2.2 lists the full eventual set; later phases add fields here as
// they need them). The set is small enough that plain os.LookupEnv reads
// more clearly than a struct-tag/reflection library would.
package config

import (
	"fmt"
	"os"
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
	}, nil
}
