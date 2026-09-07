package lichess

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests decode the recorded fixtures in testdata/ (see its
// README for provenance) to confirm the struct tags in types.go
// actually match real Lichess JSON, not just the shapes client_test.go
// hand-writes inline.

func TestDecode_RealUserFixture(t *testing.T) {
	var u User
	requireDecodeFixture(t, "testdata/user_thibault_live.json", &u)

	assert.Equal(t, "thibault", u.ID)
	require.NotNil(t, u.Perfs.Correspondence)
	assert.Equal(t, 377, u.Perfs.Correspondence.Games)
	assert.Equal(t, 1942, u.Perfs.Correspondence.Rating)
	// A real account with 377 correspondence games and rd=173 (>110)
	// is still provisional — confirms provisional isn't just a
	// newcomer's flag, and that our omitempty assumption (present when
	// true) is right, since the live payload does carry the key here.
	assert.True(t, u.Perfs.Correspondence.Provisional)

	require.NotNil(t, u.Perfs.Classical)
	assert.True(t, u.Perfs.Classical.Provisional)
}

func TestDecode_RealDrawGameFixture(t *testing.T) {
	var g Game
	requireDecodeFixture(t, "testdata/game_q7ZvsdUF_draw_live.json", &g)

	assert.Equal(t, "q7ZvsdUF", g.ID)
	assert.Equal(t, StatusDraw, g.Status)
	assert.Equal(t, "", g.Winner) // no winner on a draw
	assert.Equal(t, "lance5500", g.Players.White.User.ID)
	assert.Equal(t, 2389, g.Players.White.Rating)
	require.NotNil(t, g.Players.White.Analysis)
	assert.Equal(t, 26, g.Players.White.Analysis.ACPL)
}

func TestDecode_OngoingCorrespondenceGameFixture(t *testing.T) {
	var g Game
	requireDecodeFixture(t, "testdata/game_correspondence_ongoing.json", &g)

	assert.Equal(t, StatusStarted, g.Status)
	assert.Equal(t, 2, g.DaysPerTurn)
	assert.Equal(t, "correspondence", g.Speed)
	require.NotNil(t, g.Opening)
	assert.Equal(t, "Italian Game", g.Opening.Name)
	assert.Nil(t, g.Players.White.Analysis) // in-progress games aren't analysed
}

func TestDecode_CheatGameFixture(t *testing.T) {
	var g Game
	requireDecodeFixture(t, "testdata/game_cheat.json", &g)

	assert.Equal(t, StatusCheat, g.Status)
	assert.Equal(t, "black", g.Winner)
}

func requireDecodeFixture(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, v))
}
