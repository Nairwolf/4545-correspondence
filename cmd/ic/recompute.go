package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// runRecompute implements the nightly recompute-aggregates job (spec
// §7): a full rebuild of every approved player's player_standings row
// from the games table, as a self-healing backstop against any drift in
// the incremental recompute sync-games does. It reads nothing from
// Lichess.
func runRecompute(ctx context.Context, pool *pgxpool.Pool, riverJobID *int64) error {
	q := gen.New(pool)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "recompute-aggregates", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("recompute-aggregates: create job run: %w", err)
	}

	cfg, err := settings.Load(ctx, q)
	if err != nil {
		return fmt.Errorf("recompute-aggregates: load settings: %w", err)
	}

	count, jobErr := standings.RecomputeAll(ctx, q, cfg)

	detail, _ := json.Marshal(map[string]int{"players_recomputed": count})
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	if jobErr != nil {
		status = gen.JobRunStatusFailed
		msg := jobErr.Error()
		errMsg = &msg
	}
	if finishErr := q.FinishJobRun(ctx, gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: int32(count),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("recompute-aggregates: record job outcome", "error", finishErr)
	}

	if jobErr != nil {
		return fmt.Errorf("recompute-aggregates: %w", jobErr)
	}
	slog.Info("recompute-aggregates complete", "players_recomputed", count)
	return nil
}
