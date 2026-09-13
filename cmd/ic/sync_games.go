package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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

	// LichessErrors holds the first few failed calls' messages, so the
	// run's own row says what went wrong rather than only a count.
	LichessErrors []string `json:"lichess_errors,omitempty"`
}

// maxRecordedLichessErrors caps LichessErrors: when Lichess is down,
// every white player's call fails the same way and a hundred copies of
// one message help nobody.
const maxRecordedLichessErrors = 10

// lichessOK records one Lichess call that completed, stream included.
// Count only calls actually made: a run with nothing to fetch has made
// no calls, and must not look like it had a successful one.
func (s *syncGamesStats) lichessOK() { s.LichessCallsOK++ }

// lichessFailed records one Lichess call that failed. The job carries
// on with the rest of its work (spec §7.2: a Lichess outage degrades
// gracefully); whether the run as a whole counts as failed is decided
// once, at the end, by syncGamesOutcome.
func (s *syncGamesStats) lichessFailed(call string, err error) {
	s.LichessCallsFailed++
	slog.Warn("sync-games: lichess call failed", "call", call, "error", err)
	if len(s.LichessErrors) < maxRecordedLichessErrors {
		s.LichessErrors = append(s.LichessErrors, call+": "+err.Error())
	}
}

// syncGamesOutcome decides how a run is recorded in job_runs (spec §7.2):
//
//   - fatal is any error that is not a Lichess call failing — a database
//     write, a query, a mapping error. It always fails the run: the data
//     may be half-written and someone needs to know.
//   - if every Lichess call the run attempted failed, the run failed:
//     Lichess is unreachable and the run achieved nothing.
//   - otherwise the run succeeded. If some Lichess calls failed, the
//     error text still says which, so the failure is visible on /jobs
//     and in /health's last_error — without turning /health red for,
//     say, one renamed account whose game list 404s every hour.
//
// A run that attempted no Lichess call at all (nothing pending) with no
// fatal error has succeeded.
func syncGamesOutcome(stats syncGamesStats, fatal error) (gen.JobRunStatus, *string) {
	status := gen.JobRunStatusSucceeded
	var problems []string

	if fatal != nil {
		status = gen.JobRunStatusFailed
		problems = append(problems, fatal.Error())
	}
	if stats.LichessCallsFailed > 0 {
		if stats.LichessCallsOK == 0 {
			status = gen.JobRunStatusFailed
		}
		recorded := strings.Join(stats.LichessErrors, "; ")
		if stats.LichessCallsFailed > len(stats.LichessErrors) {
			recorded += "; …"
		}
		problems = append(problems, fmt.Sprintf(
			"%d of %d Lichess calls failed: %s",
			stats.LichessCallsFailed,
			stats.LichessCallsFailed+stats.LichessCallsOK,
			recorded,
		))
	}

	if len(problems) == 0 {
		return status, nil
	}
	msg := strings.Join(problems, "; ")
	return status, &msg
}

// runSyncGames implements the hourly sync-games job (spec §3.4, §7.3):
// re-check known in-progress games, match pairings that don't have a
// game id yet, ingest whatever's finished, and recompute standings for
// anyone whose game just finished. It never touches a game that isn't
// already known to belong to a pairing — that's the whole point of
// matching being pairing-anchored (spec §7.3): a member's other
// correspondence games are never looked at.
//
// db is a pool in production; tests pass a transaction so the job_runs
// row it writes can be read back and rolled away.
func runSyncGames(
	ctx context.Context,
	db gen.DBTX,
	client lichess.API,
	riverJobID *int64,
) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "sync-games", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("sync-games: create job run: %w", err)
	}

	// Everything after CreateJobRun ends in FinishJobRun, including a
	// settings failure — otherwise the row would sit at "running".
	var stats syncGamesStats
	cfg, fatal := settings.Load(ctx, q)
	if fatal != nil {
		fatal = fmt.Errorf("load settings: %w", fatal)
	} else {
		stats, fatal = doSyncGames(ctx, q, client, cfg)
	}
	// A run cut short by its context (river's job timeout, or shutdown)
	// left pairings unprocessed however many Lichess calls succeeded
	// first, and the calls it did not get to show up only as failures
	// with "context canceled" — which syncGamesOutcome would otherwise
	// forgive once any call had succeeded.
	if fatal == nil && ctx.Err() != nil {
		fatal = fmt.Errorf("interrupted: %w", ctx.Err())
	}

	status, errMsg := syncGamesOutcome(stats, fatal)
	detail, _ := json.Marshal(stats)
	itemsProcessed := stats.GamesFinished + stats.PairingsMatched + stats.InProgressChecked
	// The outcome row is written under a context that cannot be
	// cancelled: the work may have ended *because* ctx was cancelled
	// (river's job timeout, or shutdown), and a run that leaves its row
	// at "running" is exactly the silent failure spec §7 rules out.
	if finishErr := q.FinishJobRun(context.WithoutCancel(ctx), gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: int32(itemsProcessed),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("sync-games: record job outcome", "error", finishErr)
	}

	// A failed run returns an error so river retries it (spec §7.2).
	if fatal != nil {
		return fmt.Errorf("sync-games: %w", fatal)
	}
	if status == gen.JobRunStatusFailed {
		return fmt.Errorf("sync-games: %s", *errMsg)
	}

	logArgs := []any{
		"in_progress_checked", stats.InProgressChecked,
		"pairings_matched", stats.PairingsMatched,
		"pairings_ambiguous", stats.PairingsAmbiguous,
		"games_finished", stats.GamesFinished,
		"lichess_calls_failed", stats.LichessCallsFailed,
	}
	if errMsg != nil {
		slog.Warn("sync-games complete with Lichess failures", logArgs...)
	} else {
		slog.Info("sync-games complete", logArgs...)
	}
	return nil
}

// doSyncGames does the work of one run. The error it returns is always
// fatal — a database or mapping failure, after which it stops. A Lichess
// call failing is not returned: it is recorded on stats and the run
// carries on with the next pairing (see syncGamesOutcome).
func doSyncGames(ctx context.Context, q *gen.Queries, client lichess.API, cfg settings.Settings) (syncGamesStats, error) {
	var stats syncGamesStats

	if err := recheckInProgressGames(ctx, q, client, cfg, &stats); err != nil {
		return stats, fmt.Errorf("re-check in-progress games: %w", err)
	}
	if err := matchPendingPairings(ctx, q, client, cfg, &stats); err != nil {
		return stats, fmt.Errorf("match pending pairings: %w", err)
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
	if len(ids) == 0 {
		return nil
	}

	stream, err := client.GamesByID(ctx, ids)
	if err != nil {
		stats.lichessFailed("fetch games by id", err)
		return nil
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
	// A stream that breaks partway still keeps every game synced before
	// the break; the rest are picked up next run.
	if err := stream.Err(); err != nil {
		stats.lichessFailed("read games by id", err)
		return nil
	}
	stats.lichessOK()
	return nil
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
			stats.lichessFailed("fetch games for "+whiteID, err)
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
			stats.lichessFailed("read games for "+whiteID, streamErr)
			stats.PairingsPending += len(pairings)
			continue
		}
		stats.lichessOK()

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
