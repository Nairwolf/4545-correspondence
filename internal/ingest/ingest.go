// Package ingest maps a lichess.Game (plus the pairing it was matched
// to, spec §7.3) into the params for the UpsertGame query. It is
// deliberately just a mapping — the actual upsert, the pairing status
// update, and triggering a standings recompute are orchestration that
// belongs to the sync-games job, not to this package.
package ingest

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
)

// IsStorable reports whether a game with this raw Lichess status should
// become a games row at all. Aborted and noStart games are not games —
// spec §4.1 says they belong on the pairing instead (as failed), never
// in the games table.
func IsStorable(status string) bool {
	return status != lichess.StatusAborted && status != lichess.StatusNoStart
}

// IsFinished reports whether status is terminal (anything but Lichess's
// two in-progress statuses).
func IsFinished(status string) bool {
	switch status {
	case lichess.StatusCreated, lichess.StatusStarted:
		return false
	default:
		return true
	}
}

// TerminationFor maps a Lichess game's raw status to our termination
// enum (spec §7.1). A bare "draw" is stored as draw_other in Phase 1:
// splitting it into draw_agreement / threefold / fifty_move requires
// replaying the game's moves, deferred to when the Stats page is built.
func TerminationFor(status string) gen.GameTermination {
	switch status {
	case lichess.StatusMate:
		return gen.GameTerminationMate
	case lichess.StatusResign:
		return gen.GameTerminationResign
	case lichess.StatusOutOfTime, lichess.StatusTimeout:
		return gen.GameTerminationClockFlag
	case lichess.StatusStalemate:
		return gen.GameTerminationStalemate
	case lichess.StatusInsufficientMaterialClaim:
		return gen.GameTerminationInsufficientMaterial
	case lichess.StatusDraw:
		return gen.GameTerminationDrawOther
	default: // cheat, unknownFinish, variantEnd
		return gen.GameTerminationUnknown
	}
}

// ResultFor derives the result from Winner: "white"/"black" map
// directly to a win; anything else — a genuine draw, a stalemate,
// insufficient material, or the rare finished game with no winner
// recorded — is scored as a draw, since no side is credited with a win.
func ResultFor(g lichess.Game) gen.GameResult {
	switch g.Winner {
	case "white":
		return gen.GameResultWhiteWin
	case "black":
		return gen.GameResultBlackWin
	default:
		return gen.GameResultDraw
	}
}

// BuildGameParams maps g, plus the pairing it belongs to, into
// UpsertGame's parameters. roundNumber and the two user ids come from
// the caller — Game itself carries neither, they're resolved from the
// pairing/round rows already loaded before matching. Call only when
// IsStorable(g.Status).
func BuildGameParams(
	pairingID pgtype.UUID,
	roundNumber int32,
	whiteUserID, blackUserID pgtype.UUID,
	g lichess.Game,
) (gen.UpsertGameParams, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return gen.UpsertGameParams{}, fmt.Errorf("ingest: marshal raw payload: %w", err)
	}

	params := gen.UpsertGameParams{
		LichessGameID: g.ID,
		PairingID:     pairingID,
		RoundNumber:   roundNumber,
		WhiteUserID:   whiteUserID,
		BlackUserID:   blackUserID,
		LichessStatus: g.Status,
		StartedAt:     pgtype.Timestamptz{Time: g.CreatedAtTime(), Valid: true},
		LastMoveAt:    pgtype.Timestamptz{Time: g.LastMoveAtTime(), Valid: true},
		RawPayload:    raw,
	}

	if IsFinished(g.Status) {
		params.Status = gen.GameStatusFinished
		result := ResultFor(g)
		termination := TerminationFor(g.Status)
		params.Result = &result
		params.Termination = &termination
		params.FinishedAt = pgtype.Timestamptz{Time: g.LastMoveAtTime(), Valid: true}
		duration := int64(g.LastMoveAtTime().Sub(g.CreatedAtTime()).Seconds())
		params.DurationSeconds = &duration
	} else {
		params.Status = gen.GameStatusInProgress
	}

	if g.DaysPerTurn != 0 {
		d := int32(g.DaysPerTurn)
		params.DaysPerTurn = &d
	}
	if g.Opening != nil {
		eco, name, ply := g.Opening.ECO, g.Opening.Name, int32(g.Opening.Ply)
		params.Eco, params.OpeningName, params.OpeningPly = &eco, &name, &ply
	}

	if moves := strings.Fields(g.Moves); len(moves) > 0 {
		white := moves[0]
		params.WhiteFirstMove = &white
		if len(moves) > 1 {
			black := moves[1]
			params.BlackFirstMove = &black
		}
		whiteCount, blackCount := int32((len(moves)+1)/2), int32(len(moves)/2)
		params.WhiteMoves, params.BlackMoves = &whiteCount, &blackCount
	}

	whiteRating, blackRating := int32(g.Players.White.Rating), int32(g.Players.Black.Rating)
	params.WhiteRatingAtGame, params.BlackRatingAtGame = &whiteRating, &blackRating

	params.WhiteAccuracy, params.WhiteAcpl = analysisFields(g.Players.White.Analysis)
	params.BlackAccuracy, params.BlackAcpl = analysisFields(g.Players.Black.Analysis)

	return params, nil
}

// analysisFields maps a possibly-absent GamePlayerAnalysis to the
// accuracy/acpl columns. Both stay unset when the game hasn't been
// analysed on Lichess — spec §7.1: correspondence games are not
// analysed automatically, so this is the common case, not the
// exception.
func analysisFields(a *lichess.GamePlayerAnalysis) (pgtype.Numeric, *int32) {
	if a == nil {
		return pgtype.Numeric{}, nil
	}
	acpl := int32(a.ACPL)
	if a.Accuracy == nil {
		return pgtype.Numeric{}, &acpl
	}
	return pgtype.Numeric{Int: big.NewInt(int64(*a.Accuracy)), Exp: 0, Valid: true}, &acpl
}
