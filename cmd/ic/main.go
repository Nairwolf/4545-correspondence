// Command ic is the Infinite Correspondence server and admin CLI.
package main

import (
	"context"
	"errors"
	"flag"
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
		return errors.New("usage: ic <serve|migrate|refresh-ratings|seed-players|import-pairings|sync-games|recompute> ...")
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
		return runRefreshRatings(ctx, pool, lichess.New(cfg.LichessToken), nil)
	case "seed-players":
		if len(args) < 2 {
			return errors.New("usage: ic seed-players <usernames-file>")
		}
		pool, err := db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		return runSeedPlayers(ctx, pool, lichess.New(cfg.LichessToken), args[1])
	case "import-pairings":
		fs := flag.NewFlagSet("import-pairings", flag.ContinueOnError)
		pairAt := fs.String("pair-at", "", "RFC3339 timestamp; defaults to the most recent Monday 12:00 UTC")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return errors.New("usage: ic import-pairings [--pair-at RFC3339] <pairings.csv>")
		}
		pool, err := db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		return runImportPairings(ctx, pool, fs.Arg(0), *pairAt)
	case "sync-games":
		pool, err := db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		return runSyncGames(ctx, pool, lichess.New(cfg.LichessToken), nil)
	case "recompute":
		pool, err := db.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		return runRecompute(ctx, pool, nil)
	default:
		return fmt.Errorf(
			"unknown command %q (want: serve, migrate, refresh-ratings, seed-players, import-pairings, sync-games, recompute)",
			args[0],
		)
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
