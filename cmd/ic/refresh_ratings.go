package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// runRefreshRatings implements the daily refresh-ratings job (spec §7):
// fetch every approved player's current Lichess ratings in one batched
// call and record a snapshot. It never decides who is "unrated" or
// computes a base rating — internal/standings does that later, from
// whatever these snapshots say (spec §5.1's rule needs a Games count as
// well as a Rating, so both are stored verbatim rather than interpreted
// here).
func runRefreshRatings(ctx context.Context, pool *pgxpool.Pool, client lichess.API, riverJobID *int64) error {
	q := gen.New(pool)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "refresh-ratings", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("refresh-ratings: create job run: %w", err)
	}

	inserted, jobErr := doRefreshRatings(ctx, q, client)

	detail, _ := json.Marshal(map[string]int{"snapshots_inserted": inserted})
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
		ItemsProcessed: int32(inserted),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("refresh-ratings: record job outcome", "error", finishErr)
	}

	if jobErr != nil {
		return fmt.Errorf("refresh-ratings: %w", jobErr)
	}
	slog.Info("refresh-ratings complete", "snapshots_inserted", inserted)
	return nil
}

func doRefreshRatings(ctx context.Context, q *gen.Queries, client lichess.API) (int, error) {
	users, err := q.ListApprovedUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("list approved users: %w", err)
	}
	if len(users) == 0 {
		return 0, nil
	}

	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.LichessUserID
	}

	lichessUsers, err := client.UsersByID(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("fetch ratings: %w", err)
	}
	byID := make(map[string]lichess.User, len(lichessUsers))
	for _, lu := range lichessUsers {
		byID[lu.ID] = lu
	}

	inserted := 0
	for _, u := range users {
		lu, ok := byID[u.LichessUserID]
		if !ok {
			// Closed, renamed, or otherwise gone. Phase 1 has no admin
			// panel to flag this yet; it's simply skipped and will show
			// up as an increasingly stale rating_snapshots row.
			slog.Warn("refresh-ratings: account not found on Lichess", "username", u.LichessUsername)
			continue
		}

		params := standings.SnapshotParams(u.ID, lu)
		if _, err := q.InsertRatingSnapshot(ctx, params); err != nil {
			return inserted, fmt.Errorf("insert rating snapshot for %s: %w", u.LichessUsername, err)
		}
		inserted++
	}
	return inserted, nil
}
