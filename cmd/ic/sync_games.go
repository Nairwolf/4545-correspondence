package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/ingest"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/matching"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// syncGamesStats is what ends up in job_runs.detail (spec §7: every job
// records its outcome).
type syncGamesStats struct {
	InProgressChecked  int `json:"in_progress_checked"`
	PairingsChecked    int `json:"pairings_checked"`
	PairingsMatched    int `json:"pairings_matched"`
	PairingsAmbiguous  int `json:"pairings_ambiguous"`
	PairingsPending    int `json:"pairings_still_pending"`
	GamesFinished      int `json:"games_finished"`
	LichessCallsOK     int `json:"lichess_calls_ok"`
	LichessCallsFailed int `json:"lichess_calls_failed"`
}

// runSyncGames implements the hourly sync-games job (spec §3.4, §7.3):
// re-check known in-progress games, match pairings that don't have a
// game id yet, ingest whatever's finished, and recompute standings for
// anyone whose game just finished. It never touches a game that isn't
// already known to belong to a pairing — that's the whole point of
// matching being pairing-anchored (spec §7.3): a member's other
// correspondence games are never looked at.
func runSyncGames(ctx context.Context, pool *pgxpool.Pool, client lichess.API, riverJobID *int64) error {
	q := gen.New(pool)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "sync-games", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("sync-games: create job run: %w", err)
	}

	cfg, err := settings.Load(ctx, q)
	if err != nil {
		return fmt.Errorf("sync-games: load settings: %w", err)
	}

	stats, jobErr := doSyncGames(ctx, q, client, cfg)

	detail, _ := json.Marshal(stats)
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	// "failed" only when nothing at all succeeded (spec §7): a job that
	// made progress on some pairings despite one bad Lichess call is
	// still a successful run.
	if jobErr != nil && stats.LichessCallsOK == 0 {
		status = gen.JobRunStatusFailed
		msg := jobErr.Error()
		errMsg = &msg
	}
	itemsProcessed := stats.GamesFinished + stats.PairingsMatched + stats.InProgressChecked
	if finishErr := q.FinishJobRun(ctx, gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: int32(itemsProcessed),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("sync-games: record job outcome", "error", finishErr)
	}

	if status == gen.JobRunStatusFailed {
		return fmt.Errorf("sync-games: %w", jobErr)
	}
	slog.Info("sync-games complete",
		"in_progress_checked", stats.InProgressChecked,
		"pairings_matched", stats.PairingsMatched,
		"pairings_ambiguous", stats.PairingsAmbiguous,
		"games_finished", stats.GamesFinished,
	)
	return nil
}

func doSyncGames(ctx context.Context, q *gen.Queries, client lichess.API, cfg settings.Settings) (syncGamesStats, error) {
	var stats syncGamesStats

	if err := recheckInProgressGames(ctx, q, client, cfg, &stats); err != nil {
		slog.Warn("sync-games: re-check in-progress games", "error", err)
		stats.LichessCallsFailed++
	} else {
		stats.LichessCallsOK++
	}

	if err := matchPendingPairings(ctx, q, client, cfg, &stats); err != nil {
		return stats, err
	}

	return stats, nil
}

// recheckInProgressGames is spec §7.3 step 1: batch-refetch every game
// a pairing already knows the id of but hasn't reached a terminal
// status for. This covers both a game already ingested as in_progress
// AND a pairing whose game id was already known at import time (the
// CSV's optional game_id column) but never fetched at all yet — see
// ListPairingsToRecheck's own comment.
func recheckInProgressGames(ctx context.Context, q *gen.Queries, client lichess.API, cfg settings.Settings, stats *syncGamesStats) error {
	pairings, err := q.ListPairingsToRecheck(ctx)
	if err != nil {
		return fmt.Errorf("list pairings to recheck: %w", err)
	}
	if len(pairings) == 0 {
		return nil
	}
	byGameID := make(map[string]gen.ListPairingsToRecheckRow, len(pairings))
	ids := make([]string, 0, len(pairings))
	for _, p := range pairings {
		if p.LichessGameID == nil {
			continue
		}
		byGameID[*p.LichessGameID] = p
		ids = append(ids, *p.LichessGameID)
	}

	stream, err := client.GamesByID(ctx, ids)
	if err != nil {
		return fmt.Errorf("fetch games by id: %w", err)
	}
	defer stream.Close()

	for stream.Next() {
		g := stream.Game()
		row, ok := byGameID[g.ID]
		if !ok {
			continue
		}
		stats.InProgressChecked++
		finished, err := syncOneGame(
			ctx,
			q,
			row.PairingID,
			row.RoundNumber,
			row.WhiteUserID,
			row.BlackUserID,
			g,
			stream.Raw(),
			cfg,
		)
		if err != nil {
			return fmt.Errorf("sync game %s: %w", g.ID, err)
		}
		if finished {
			stats.GamesFinished++
		}
	}
	return stream.Err()
}

// matchPendingPairings is spec §7.3 step 2: find the Lichess game for
// every pairing that doesn't have one yet, grouped by white player so a
// player with several pending pairings costs one Lichess call.
func matchPendingPairings(ctx context.Context, q *gen.Queries, client lichess.API, cfg settings.Settings, stats *syncGamesStats) error {
	unmatched, err := q.ListUnmatchedPairings(ctx)
	if err != nil {
		return fmt.Errorf("list unmatched pairings: %w", err)
	}
	stats.PairingsChecked += len(unmatched)
	if len(unmatched) == 0 {
		return nil
	}

	attached, err := q.ListAttachedGameIDs(ctx)
	if err != nil {
		return fmt.Errorf("list attached game ids: %w", err)
	}
	taken := make(map[string]bool, len(attached))
	for _, id := range attached {
		if id != nil {
			taken[*id] = true
		}
	}

	byWhite := map[string][]gen.ListUnmatchedPairingsRow{}
	for _, p := range unmatched {
		byWhite[p.WhiteLichessID] = append(byWhite[p.WhiteLichessID], p)
	}

	for whiteID, pairings := range byWhite {
		since := earliestPairAt(pairings).Add(-24 * time.Hour)
		rated := true
		stream, err := client.UserGames(ctx, whiteID, lichess.UserGamesOptions{
			PerfType: "correspondence",
			Rated:    &rated,
			Since:    since,
			Ongoing:  true,
			Finished: true,
		})
		if err != nil {
			slog.Warn("sync-games: fetch games for white player", "white", whiteID, "error", err)
			stats.LichessCallsFailed++
			stats.PairingsPending += len(pairings)
			continue
		}
		var candidates []lichess.Game
		rawByID := map[string][]byte{}
		for stream.Next() {
			g := stream.Game()
			candidates = append(candidates, g)
			rawByID[g.ID] = bytes.Clone(stream.Raw()) // Raw is only valid until the next Next
		}
		streamErr := stream.Err()
		stream.Close()
		if streamErr != nil {
			slog.Warn("sync-games: read games for white player", "white", whiteID, "error", streamErr)
			stats.LichessCallsFailed++
			stats.PairingsPending += len(pairings)
			continue
		}
		stats.LichessCallsOK++

		matchCandidates := toMatchingCandidates(candidates)
		byGameID := make(map[string]lichess.Game, len(candidates))
		for _, g := range candidates {
			byGameID[g.ID] = g
		}

		for _, p := range pairings {
			result := matching.Match(matching.Pairing{
				WhiteLichessID: p.WhiteLichessID,
				BlackLichessID: p.BlackLichessID,
				PairAt:         p.PairAt.Time,
				DaysPerMove:    cfg.DaysPerMove,
			}, matchCandidates, taken)

			switch {
			case result.Ambiguous:
				if err := q.MarkPairingAmbiguous(ctx, p.PairingID); err != nil {
					return fmt.Errorf("mark pairing ambiguous: %w", err)
				}
				stats.PairingsAmbiguous++
			case result.Game != "":
				g := byGameID[result.Game]
				finished, err := syncOneGame(
					ctx,
					q,
					p.PairingID,
					p.RoundNumber,
					p.WhiteUserID,
					p.BlackUserID,
					g,
					rawByID[g.ID],
					cfg,
				)
				if err != nil {
					return fmt.Errorf("sync matched game %s: %w", g.ID, err)
				}
				stats.PairingsMatched++
				if finished {
					stats.GamesFinished++
				}
			default:
				stats.PairingsPending++
			}
		}
	}
	return nil
}

// syncOneGame upserts g against the pairing it belongs to and updates
// the pairing's status. It returns true iff this call is what made the
// game finished — the caller recomputes both players' standings only
// then, never on an already-accounted-for game.
func syncOneGame(
	ctx context.Context,
	q *gen.Queries,
	pairingID pgtype.UUID,
	roundNumber int32,
	whiteID, blackID pgtype.UUID,
	g lichess.Game,
	raw []byte,
	cfg settings.Settings,
) (finished bool, err error) {
	if !ingest.IsStorable(g.Status) {
		// aborted/noStart: never a games row (spec §4.1); remove any
		// stale in-progress row this id might already have, and fail
		// the pairing.
		if err := q.DeleteGame(ctx, g.ID); err != nil {
			return false, fmt.Errorf("delete non-storable game: %w", err)
		}
		if err := q.MarkPairingFailed(ctx, pairingID); err != nil {
			return false, fmt.Errorf("mark pairing failed: %w", err)
		}
		return false, nil
	}

	params, err := ingest.BuildGameParams(pairingID, roundNumber, whiteID, blackID, g, raw)
	if err != nil {
		return false, err
	}
	if _, err := q.UpsertGame(ctx, params); err != nil {
		return false, fmt.Errorf("upsert game: %w", err)
	}

	pairingStatus := gen.PairingStatusInProgress
	nowFinished := ingest.IsFinished(g.Status)
	if nowFinished {
		pairingStatus = gen.PairingStatusCompleted
	}
	gameID := g.ID
	if err := q.AttachGameToPairing(ctx, gen.AttachGameToPairingParams{
		ID:            pairingID,
		LichessGameID: &gameID,
		Status:        pairingStatus,
	}); err != nil {
		return false, fmt.Errorf("attach game to pairing: %w", err)
	}

	if !nowFinished {
		return false, nil
	}

	if _, err := standings.Recompute(ctx, q, whiteID, cfg); err != nil {
		return false, fmt.Errorf("recompute white standing: %w", err)
	}
	if _, err := standings.Recompute(ctx, q, blackID, cfg); err != nil {
		return false, fmt.Errorf("recompute black standing: %w", err)
	}
	return true, nil
}

func earliestPairAt(pairings []gen.ListUnmatchedPairingsRow) time.Time {
	earliest := pairings[0].PairAt.Time
	for _, p := range pairings[1:] {
		if p.PairAt.Time.Before(earliest) {
			earliest = p.PairAt.Time
		}
	}
	return earliest
}

func toMatchingCandidates(games []lichess.Game) []matching.Candidate {
	candidates := make([]matching.Candidate, 0, len(games))
	for _, g := range games {
		c := matching.Candidate{
			ID:          g.ID,
			Variant:     g.Variant,
			Rated:       g.Rated,
			DaysPerTurn: g.DaysPerTurn,
			CreatedAt:   g.CreatedAtTime(),
		}
		if g.Players.White.User != nil {
			c.WhiteLichessID = g.Players.White.User.ID
		}
		if g.Players.Black.User != nil {
			c.BlackLichessID = g.Players.Black.User.ID
		}
		candidates = append(candidates, c)
	}
	return candidates
}
