package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// runRecompute implements the nightly recompute-aggregates job (spec
// §7): a full rebuild of every approved player's player_standings row
// from the games table, as a self-healing backstop against any drift in
// the incremental recompute sync-games does. It reads nothing from
// Lichess.
//
// db is a pool in production; tests pass a transaction so the job_runs
// row it writes can be read back and rolled away.
func runRecompute(ctx context.Context, db gen.DBTX, riverJobID *int64) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "recompute-aggregates", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("recompute-aggregates: create job run: %w", err)
	}

	// Everything after CreateJobRun ends in FinishJobRun, including a
	// settings failure — otherwise the row would sit at "running".
	var count int
	cfg, jobErr := settings.Load(ctx, q)
	if jobErr != nil {
		jobErr = fmt.Errorf("load settings: %w", jobErr)
	} else {
		count, jobErr = standings.RecomputeAll(ctx, q, cfg)
	}

	detail, _ := json.Marshal(map[string]int{"players_recomputed": count})
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	if jobErr != nil {
		status = gen.JobRunStatusFailed
		msg := jobErr.Error()
		errMsg = &msg
	}
	// The outcome row is written under a context that cannot be
	// cancelled: the work may have ended *because* ctx was cancelled
	// (river's job timeout, or shutdown), and a run that leaves its row
	// at "running" is exactly the silent failure spec §7 rules out.
	if finishErr := q.FinishJobRun(context.WithoutCancel(ctx), gen.FinishJobRunParams{
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
