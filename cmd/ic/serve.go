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

	"github.com/nairwolf/4545-correspondence/internal/config"
	"github.com/nairwolf/4545-correspondence/internal/db"
	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/jobs"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/web"
)

// runServe starts the long-running process: the river job runner (spec
// §7's periodic sync workers) and the public HTTP site (spec §8.1). A
// SIGINT/SIGTERM drains in-flight requests and gives a running job a
// bounded grace to finish and record its outcome before exiting.
func runServe(ctx context.Context, cfg config.Config) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Any job_runs row still "running" was left by a process that died:
	// nothing else can leave one unfinished now that every job writes its
	// outcome under a non-cancelled context (and river cancels a run at
	// jobs.JobTimeout rather than letting it hang). Without this sweep
	// /health would report that job as "running" forever. Caveat: a
	// one-shot `ic sync-games` running at the moment serve starts has its
	// row marked failed too — operator error, and rare.
	interrupted := "interrupted: the server restarted before this run finished"
	swept, err := gen.New(pool).FailInterruptedJobRuns(ctx, &interrupted)
	if err != nil {
		return fmt.Errorf("serve: sweep interrupted job runs: %w", err)
	}
	if swept > 0 {
		slog.Warn("marked interrupted job runs as failed", "count", swept)
	}

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

	handler, err := web.New(ctx, pool)
	if err != nil {
		return fmt.Errorf("serve: build web handler: %w", err)
	}

	srv := &http.Server{
		Addr:        cfg.ListenAddr,
		Handler:     handler.Handler(),
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

	// Bounded, signal-independent shutdown. The signal already cancelled
	// river's start context, which begins a soft stop: a running job has
	// internal/jobs' soft-stop grace to finish before its own context is
	// cancelled, concurrently with the HTTP drain below. Stop then waits
	// for the job to record its outcome, and this deadline caps the lot.
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
