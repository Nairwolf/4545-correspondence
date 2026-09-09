package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nairwolf/4545-correspondence/internal/config"
	"github.com/nairwolf/4545-correspondence/internal/db"
	"github.com/nairwolf/4545-correspondence/internal/jobs"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
)

// runServe starts the long-running process: the river job runner (spec
// §7's periodic sync workers) and the HTTP server. The public pages and
// the real /health endpoint (spec §8.1) land in the next build-order
// step; for now the server answers only a liveness probe so `serve` is
// something a deployment can point a health check at.
func runServe(ctx context.Context, cfg config.Config) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	client := lichess.New(cfg.LichessToken)

	riverClient, err := jobs.NewClient(pool, jobs.Handlers{
		SyncGames: func(ctx context.Context, id int64) error {
			return runSyncGames(ctx, pool, client, &id)
		},
		RefreshRatings: func(ctx context.Context, id int64) error {
			return runRefreshRatings(ctx, pool, client, &id)
		},
		Recompute: func(ctx context.Context, id int64) error {
			return runRecompute(ctx, pool, &id)
		},
	}, slog.Default())
	if err != nil {
		return fmt.Errorf("serve: build job runner: %w", err)
	}
	if err := riverClient.Start(ctx); err != nil {
		return fmt.Errorf("serve: start job runner: %w", err)
	}

	srv := &http.Server{
		Addr:        cfg.ListenAddr,
		Handler:     serveMux(pool),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	srvErr := make(chan error, 1)
	go func() {
		slog.Info("http listening", "addr", cfg.ListenAddr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		srvErr <- err
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-srvErr:
		if err != nil {
			return fmt.Errorf("serve: http: %w", err)
		}
	}

	// Bounded, signal-independent shutdown: let in-flight requests and a
	// running job drain, then force it.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("serve: http shutdown", "error", err)
	}
	if err := riverClient.Stop(shutdownCtx); err != nil {
		slog.Error("serve: job runner shutdown", "error", err)
	}
	return nil
}

func serveMux(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return mux
}
