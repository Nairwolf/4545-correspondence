package ingest

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
)

// loadFixture reads a fixture from internal/lichess/testdata, shared
// with that package's own tests rather than duplicated here (see its
// testdata/README.md).
func loadFixture(t *testing.T, name string) (lichess.Game, []byte) {
	t.Helper()
	data, err := os.ReadFile("../lichess/testdata/" + name)
	require.NoError(t, err)
	var g lichess.Game
	require.NoError(t, json.Unmarshal(data, &g))
	return g, data
}

// anyID etc. are placeholder ids for tests that only care about mapping.
var (
	anyPairing = pgtype.UUID{Valid: true}
	anyUser    = pgtype.UUID{Valid: true}
)

// build is the common case: map a fixture with placeholder ids.
func build(t *testing.T, name string) gen.UpsertGameParams {
	t.Helper()
	g, raw := loadFixture(t, name)
	params, err := BuildGameParams(anyPairing, 1, anyUser, anyUser, g, raw)
	require.NoError(t, err)
	return params
}

func TestIsStorable(t *testing.T) {
	assert.False(t, IsStorable(lichess.StatusAborted))
	assert.False(t, IsStorable(lichess.StatusNoStart))
	assert.True(t, IsStorable(lichess.StatusMate))
	assert.True(t, IsStorable(lichess.StatusResign))
	assert.True(t, IsStorable(lichess.StatusStarted))
}

func TestIsFinished(t *testing.T) {
	assert.False(t, IsFinished(lichess.StatusCreated))
	assert.False(t, IsFinished(lichess.StatusStarted))
	for _, s := range []string{
		lichess.StatusMate, lichess.StatusResign, lichess.StatusStalemate,
		lichess.StatusOutOfTime, lichess.StatusTimeout, lichess.StatusDraw,
		lichess.StatusCheat, lichess.StatusInsufficientMaterialClaim,
		lichess.StatusAborted, lichess.StatusNoStart,
	} {
		assert.True(t, IsFinished(s), "status=%s", s)
	}
}

func TestTerminationFor(t *testing.T) {
	tests := map[string]gen.GameTermination{
		lichess.StatusMate:                      gen.GameTerminationMate,
		lichess.StatusResign:                    gen.GameTerminationResign,
		lichess.StatusOutOfTime:                 gen.GameTerminationClockFlag,
		lichess.StatusTimeout:                   gen.GameTerminationClockFlag,
		lichess.StatusStalemate:                 gen.GameTerminationStalemate,
		lichess.StatusInsufficientMaterialClaim: gen.GameTerminationInsufficientMaterial,
		lichess.StatusDraw:                      gen.GameTerminationDrawOther,
		lichess.StatusCheat:                     gen.GameTerminationUnknown,
		lichess.StatusUnknownFinish:             gen.GameTerminationUnknown,
		lichess.StatusVariantEnd:                gen.GameTerminationUnknown,
	}
	for status, want := range tests {
		assert.Equal(t, want, TerminationFor(status), "status=%s", status)
	}
}

func TestResultFor(t *testing.T) {
	assert.Equal(t, gen.GameResultWhiteWin, ResultFor(lichess.Game{Winner: "white"}))
	assert.Equal(t, gen.GameResultBlackWin, ResultFor(lichess.Game{Winner: "black"}))
	assert.Equal(t, gen.GameResultDraw, ResultFor(lichess.Game{Winner: ""}))
}

func TestBuildGameParams_DecisiveResign(t *testing.T) {
	g, raw := loadFixture(t, "game_resign.json")
	pairingID := pgtype.UUID{Bytes: [16]byte{1}, Valid: true}
	whiteID := pgtype.UUID{Bytes: [16]byte{2}, Valid: true}
	blackID := pgtype.UUID{Bytes: [16]byte{3}, Valid: true}

	params, err := BuildGameParams(pairingID, 7, whiteID, blackID, g, raw)
	require.NoError(t, err)

	assert.Equal(t, "resign001", params.LichessGameID)
	assert.Equal(t, gen.GameStatusFinished, params.Status)
	require.NotNil(t, params.Result)
	assert.Equal(t, gen.GameResultBlackWin, *params.Result)
	require.NotNil(t, params.Termination)
	assert.Equal(t, gen.GameTerminationResign, *params.Termination)
	require.NotNil(t, params.WhiteFirstMove)
	assert.Equal(t, "e4", *params.WhiteFirstMove)
	require.NotNil(t, params.BlackFirstMove)
	assert.Equal(t, "c6", *params.BlackFirstMove)
	require.NotNil(t, params.WhiteMoves)
	assert.Equal(t, int32(4), *params.WhiteMoves) // 8 plies, 4 each side
	assert.Equal(t, int32(4), *params.BlackMoves)
	require.NotNil(t, params.DaysPerTurn)
	assert.Equal(t, int32(2), *params.DaysPerTurn)
	require.NotNil(t, params.WhiteRatingAtGame)
	assert.Equal(t, int32(1650), *params.WhiteRatingAtGame)
	assert.True(t, params.FinishedAt.Valid)
	require.NotNil(t, params.DurationSeconds)
	assert.Equal(t, int64(86400), *params.DurationSeconds) // exactly 1 day apart in the fixture
}

func TestBuildGameParams_Flag(t *testing.T) {
	params := build(t, "game_flag.json")
	require.NotNil(t, params.Result)
	assert.Equal(t, gen.GameResultWhiteWin, *params.Result)
	require.NotNil(t, params.Termination)
	assert.Equal(t, gen.GameTerminationClockFlag, *params.Termination)
}

func TestBuildGameParams_Stalemate(t *testing.T) {
	params := build(t, "game_stalemate.json")
	require.NotNil(t, params.Result)
	assert.Equal(t, gen.GameResultDraw, *params.Result)
	require.NotNil(t, params.Termination)
	assert.Equal(t, gen.GameTerminationStalemate, *params.Termination)
}

func TestBuildGameParams_InsufficientMaterial(t *testing.T) {
	params := build(t, "game_insufficient_material.json")
	require.NotNil(t, params.Termination)
	assert.Equal(t, gen.GameTerminationInsufficientMaterial, *params.Termination)
}

func TestBuildGameParams_BareDrawBecomesDrawOther(t *testing.T) {
	params := build(t, "game_q7ZvsdUF_draw_live.json")
	require.NotNil(t, params.Result)
	assert.Equal(t, gen.GameResultDraw, *params.Result)
	require.NotNil(t, params.Termination)
	assert.Equal(t, gen.GameTerminationDrawOther, *params.Termination)
	// This fixture has real per-player analysis: accuracy/acpl must map through.
	require.NotNil(t, params.WhiteAcpl)
	assert.Equal(t, int32(26), *params.WhiteAcpl)
}

func TestBuildGameParams_CheatMapsToUnknownTerminationButRealResult(t *testing.T) {
	params := build(t, "game_cheat.json")
	require.NotNil(t, params.Result)
	assert.Equal(t, gen.GameResultBlackWin, *params.Result)
	require.NotNil(t, params.Termination)
	assert.Equal(t, gen.GameTerminationUnknown, *params.Termination)
}

func TestBuildGameParams_InProgressGameHasNoResultOrDuration(t *testing.T) {
	params := build(t, "game_correspondence_ongoing.json")

	assert.Equal(t, gen.GameStatusInProgress, params.Status)
	assert.Nil(t, params.Result)
	assert.Nil(t, params.Termination)
	assert.Nil(t, params.DurationSeconds)
	assert.False(t, params.FinishedAt.Valid)
	// still-populated fields: opening, ratings, moves so far.
	require.NotNil(t, params.OpeningName)
	assert.Equal(t, "Italian Game", *params.OpeningName)
}

func TestBuildGameParams_UnanalysedGameLeavesAccuracyAcplUnset(t *testing.T) {
	params := build(t, "game_stalemate.json") // no "analysis" key at all
	assert.Nil(t, params.WhiteAcpl)
	assert.False(t, params.WhiteAccuracy.Valid)
	assert.Nil(t, params.BlackAcpl)
	assert.False(t, params.BlackAccuracy.Valid)
}

func TestAbortedGamesAreNotStorable_NoRowIsEverBuiltForThem(t *testing.T) {
	g, _ := loadFixture(t, "game_aborted.json")
	// This is the contract: callers must check IsStorable BEFORE calling
	// BuildGameParams at all for an aborted game — spec §4.1 says these
	// are never games rows. There is deliberately no BuildGameParams
	// call in this test; IsStorable is what a caller consults.
	assert.False(t, IsStorable(g.Status))
}

func TestBuildGameParams_RawPayloadIsTheLichessBytesVerbatim(t *testing.T) {
	// Spec §7.1: raw_payload is the complete response, not a
	// re-serialisation of lichess.Game. The live fixture carries fields
	// the struct doesn't declare (ratingDiff, per-player blunder counts,
	// an arena tournament) — they must survive into the payload.
	g, raw := loadFixture(t, "game_q7ZvsdUF_draw_live.json")
	params, err := BuildGameParams(anyPairing, 1, anyUser, anyUser, g, raw)
	require.NoError(t, err)

	assert.Equal(t, raw, params.RawPayload, "raw_payload must be the input bytes, untouched")
	for _, undeclared := range []string{`"ratingDiff"`, `"blunder"`, `"arenaTour"`} {
		assert.Contains(t, string(params.RawPayload), undeclared)
	}
	reencoded, _ := json.Marshal(g)
	assert.NotContains(t, string(reencoded), `"ratingDiff"`, "sanity: the struct really does drop this — that's why raw is passed separately")
}

func TestBuildGameParams_RejectsInvalidRawJSON(t *testing.T) {
	g, _ := loadFixture(t, "game_resign.json")
	_, err := BuildGameParams(anyPairing, 1, anyUser, anyUser, g, []byte(`{not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not valid JSON")
}
