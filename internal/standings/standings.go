// Package standings recomputes a player's materialised player_standings
// row (spec §4.1) from their finished games and latest rating snapshot,
// using internal/scoring for every actual calculation — this package's
// own job is only to load the inputs, call scoring, and write the
// result back in one upsert (spec: "rather than the read-modify-write
// pattern an incremental UPDATE would need").
package standings

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/scoring"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// Recompute rebuilds userID's player_standings row from scratch and
// writes it. leveledUp is true only when the newly computed level is
// higher than what was stored before this call — first-ever computation
// for a player is never itself a "level up".
func Recompute(ctx context.Context, q *gen.Queries, userID pgtype.UUID, cfg settings.Settings) (leveledUp bool, err error) {
	games, err := q.ListFinishedGamesForUser(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("standings: list finished games: %w", err)
	}
	finished, skippedNoRating := toFinishedGames(userID, games)
	_ = skippedNoRating // surfaced via job_runs.detail by the caller, not logged here

	corr, classical, err := latestRatings(ctx, q, userID)
	if err != nil {
		return false, err
	}
	base, unrated := scoring.BaseRating(corr, classical)
	effectiveBase := base
	if unrated {
		effectiveBase = cfg.UnratedDefault
	}

	window := scoring.LastK(finished, cfg.LastK)
	perf, perfOK := scoring.PerfRating(finished, cfg.LastK)
	power := scoring.PowerRating(perf, perfOK, effectiveBase, len(finished), cfg.MinGamesForPerf)
	color := scoring.ColorScore(finished)
	xp := scoring.XP(finished, cfg.XPWeights)
	level, xpToNext := scoring.Level(xp)
	wins, draws, losses := countResults(finished)

	ongoing, err := q.CountInFlightGamesForUser(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("standings: count in-flight games: %w", err)
	}

	profile, err := q.GetPlayerProfile(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("standings: player profile: %w", err)
	}
	user, err := q.GetUserByID(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("standings: user: %w", err)
	}
	// A display column (spec §5.7): the pairing pool reads eligibility
	// live from users/player_profiles and never trusts this copy.
	isEligible := user.Status == gen.UserStatusApproved &&
		profile.IsActive &&
		!profile.PausedByAdmin &&
		!profile.AutoPausedAt.Valid

	prev, err := q.GetPlayerStanding(ctx, userID)
	hasPrev := true
	if errors.Is(err, pgx.ErrNoRows) {
		hasPrev = false
	} else if err != nil {
		return false, fmt.Errorf("standings: previous standing: %w", err)
	}
	leveledUp = hasPrev && level > int(prev.Level)

	params := gen.UpsertPlayerStandingParams{
		UserID:        userID,
		IsUnrated:     unrated,
		GamesPlayed:   int32(len(finished)),
		Wins:          int32(wins),
		Draws:         int32(draws),
		Losses:        int32(losses),
		Ongoing:       int32(ongoing),
		PowerRating:   int32(power),
		ColorScore:    int32(color),
		Xp:            int32(xp),
		Level:         int32(level),
		XpToNextLevel: int32(xpToNext),
		IsEligible:    isEligible,
	}
	if !unrated {
		r := int32(base)
		params.Rating = &r
	}
	if perfOK {
		params.LastKScore = numericFromHalfPoints(scoring.LastKScore(window))
		p := int32(math.Round(perf))
		params.LastKPerfRating = &p
	}
	if len(games) > 0 {
		// games is ordered by finished_at DESC (ListFinishedGamesForUser),
		// so the first row is always the most recent finished game,
		// regardless of whether it was excluded from `finished` above
		// for lacking an opponent rating.
		params.LastGameFinishedAt = games[0].FinishedAt
		if leveledUp {
			params.LastLevelUpRound = &games[0].RoundNumber
			params.LastLevelUpAt = games[0].FinishedAt
		}
	}
	if !leveledUp && hasPrev {
		params.LastLevelUpRound = prev.LastLevelUpRound
		params.LastLevelUpAt = prev.LastLevelUpAt
	}

	if err := q.UpsertPlayerStanding(ctx, params); err != nil {
		return false, fmt.Errorf("standings: upsert: %w", err)
	}
	return leveledUp, nil
}

// RecomputeAll rebuilds every approved player's standing — the nightly
// recompute-aggregates job (spec §7): a full rebuild as a self-healing
// backstop against incremental drift.
func RecomputeAll(ctx context.Context, q *gen.Queries, cfg settings.Settings) (count int, err error) {
	users, err := q.ListApprovedUsers(ctx)
	if err != nil {
		return 0, fmt.Errorf("standings: list approved users: %w", err)
	}
	for _, u := range users {
		if _, err := Recompute(ctx, q, u.ID, cfg); err != nil {
			return count, fmt.Errorf("standings: recompute %s: %w", u.LichessUsername, err)
		}
		count++
	}
	return count, nil
}

func latestRatings(ctx context.Context, q *gen.Queries, userID pgtype.UUID) (corr, classical *scoring.Rating, err error) {
	snapshot, err := q.GetLatestRatingSnapshot(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil // no snapshot yet: refresh-ratings/seed-players hasn't run for them
	}
	if err != nil {
		return nil, nil, fmt.Errorf("standings: latest rating snapshot: %w", err)
	}
	if snapshot.CorrespondenceRating != nil && snapshot.CorrespondenceGames != nil && *snapshot.CorrespondenceGames > 0 {
		corr = &scoring.Rating{
			Value:       int(*snapshot.CorrespondenceRating),
			Provisional: snapshot.CorrespondenceProv != nil && *snapshot.CorrespondenceProv,
		}
	}
	if snapshot.ClassicalRating != nil && snapshot.ClassicalGames != nil && *snapshot.ClassicalGames > 0 {
		classical = &scoring.Rating{
			Value:       int(*snapshot.ClassicalRating),
			Provisional: snapshot.ClassicalProv != nil && *snapshot.ClassicalProv,
		}
	}
	return corr, classical, nil
}

// toFinishedGames converts userID's games rows into the shape
// internal/scoring takes: each game resolved to userID's own
// perspective. A game whose payload lacks the opponent's rating —
// spec §5.2 notes this "should not happen for rated games" — is
// excluded from the count returned as skipped, for the caller to
// surface in job_runs.detail rather than silently drop.
func toFinishedGames(userID pgtype.UUID, games []gen.Game) (finished []scoring.FinishedGame, skipped int) {
	for _, g := range games {
		if g.Result == nil {
			continue // defensive: the games CHECK constraint should make this impossible
		}
		isWhite := g.WhiteUserID == userID

		var opponentRating *int32
		var opponentID pgtype.UUID
		if isWhite {
			opponentRating, opponentID = g.BlackRatingAtGame, g.BlackUserID
		} else {
			opponentRating, opponentID = g.WhiteRatingAtGame, g.WhiteUserID
		}
		if opponentRating == nil {
			skipped++
			continue
		}

		var result scoring.Result
		switch {
		case *g.Result == gen.GameResultDraw:
			result = scoring.Draw
		case (*g.Result == gen.GameResultWhiteWin) == isWhite:
			result = scoring.Win
		default:
			result = scoring.Loss
		}

		finished = append(finished, scoring.FinishedGame{
			OpponentID:           scoring.UserID(opponentID.String()),
			OpponentRatingAtGame: int(*opponentRating),
			PlayedWhite:          isWhite,
			Result:               result,
			FinishedAt:           g.FinishedAt.Time,
		})
	}
	return finished, skipped
}

func countResults(games []scoring.FinishedGame) (wins, draws, losses int) {
	for _, g := range games {
		switch g.Result {
		case scoring.Win:
			wins++
		case scoring.Draw:
			draws++
		case scoring.Loss:
			losses++
		}
	}
	return wins, draws, losses
}

// numericFromHalfPoints builds a pgtype.Numeric for a score that is
// always a multiple of 0.5 (wins=1, draws=0.5 — spec §5.2), avoiding
// any float-to-decimal-string round-trip.
func numericFromHalfPoints(score float64) pgtype.Numeric {
	tenths := int64(math.Round(score * 10))
	return pgtype.Numeric{Int: big.NewInt(tenths), Exp: -1, Valid: true}
}
