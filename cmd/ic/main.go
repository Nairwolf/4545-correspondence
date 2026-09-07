// Command ic is the Infinite Correspondence server and admin CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/nairwolf/4545-correspondence/internal/config"
	"github.com/nairwolf/4545-correspondence/internal/db"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ic:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ic <serve|migrate|refresh-ratings> ...")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()

	switch args[0] {
	case "migrate":
		return runMigrate(ctx, cfg)
	case "serve":
		return runServe(ctx, cfg)
	case "refresh-ratings":
		pool, err := db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		return runRefreshRatings(ctx, pool, lichess.New(cfg.LichessToken))
	default:
		return fmt.Errorf("unknown command %q (want: serve, migrate, refresh-ratings)", args[0])
	}
}

func runMigrate(ctx context.Context, cfg config.Config) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, cfg.DatabaseURL); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}

func runServe(ctx context.Context, cfg config.Config) error {
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The web server, job runner and remaining subcommands (seed-players,
	// import-pairings, sync-games, refresh-ratings, recompute,
	// refetch-game) are added in later build-order steps; see PLAN.md.
	return fmt.Errorf("serve: not implemented yet")
}
