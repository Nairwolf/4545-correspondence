//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// createdRound writes a published round whose games the league
// created: the first pairing in bulk "bulk1", the second left for its
// players to start by hand.
func createdRound(t *testing.T, q *gen.Queries, a, b, c, d gen.User) (gen.Round, []gen.Pairing) {
	t.Helper()
	ctx := context.Background()
	r := thisWeekRound(t, q, 9100, gen.RoundStatePublished, `{"days_per_move": 2, "rated": true}`)
	pairings := []gen.Pairing{pairIn(t, q, r, a, b), pairIn(t, q, r, c, d)}

	_, err := q.StartGameCreation(ctx, gen.StartGameCreationParams{ID: r.ID, GameCreation: gen.GameCreationStateCreating})
	require.NoError(t, err)
	game, bulk := "bulkgame1", "bulk1"
	n, err := q.RecordCreatedGame(ctx, gen.RecordCreatedGameParams{
		ID:             pairings[0].ID,
		CreationMethod: gen.PairingMethodBulk,
		Status:         gen.PairingStatusCreated,
		LichessGameID:  &game,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	require.NoError(t, q.SetRoundBulkPairing(ctx, gen.SetRoundBulkPairingParams{ID: r.ID, BulkPairingID: &bulk}))
	r, err = q.FinishGameCreation(ctx, gen.FinishGameCreationParams{
		ID:             r.ID,
		Outcome:        gen.GameCreationStateCreated,
		GamesCreatedAt: r.PublishedAt,
	})
	require.NoError(t, err)
	return r, pairings
}

func TestAdminRounds_PublishNowUnderLichessQueuesTheGameCreation(t *testing.T) {
	srv, q, tx := testServer(t)
	sched := &recordingScheduler{}
	srv.sched = sched
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, _ := generateDraft(t, srv, q, adminSession)

	setSetting(t, q, "pairing.game_creation", `"lichess"`)
	rec := postAs(t, srv, adminSession, roundPath(id)+"/publish", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)

	assert.Equal(t, []scheduledCreation{{roundID: id, retry: false}}, sched.creations)
	body := getAs(t, srv, adminSession, roundPath(id)).Body.String()
	assert.Contains(t, body, "The games are being created on Lichess")
	assert.NotContains(t, body, "Retry game creation")
}

func TestAdminRounds_GameCreationLineAndRetry(t *testing.T) {
	ctx := context.Background()
	srv, q, tx := testServer(t)
	sched := &recordingScheduler{}
	srv.sched = sched
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	c := addPlayer(t, q, tx, "Charlie", true, 1800, 0, 1800, 1800, "0")
	d := addPlayer(t, q, tx, "Delta", true, 1700, 0, 1700, 1700, "0")
	round, _ := createdRound(t, q, a, b, c, d)
	path := roundPath(round.ID)

	// Under manual, the page says why there is no button.
	body := getAs(t, srv, adminSession, path).Body.String()
	assert.Contains(t, body, "Games created on Lichess")
	assert.Contains(t, body, "bulk bulk1")
	assert.Contains(t, body, "1 pairing left for the players to start by hand.")
	assert.Contains(t, body, `href="https://lichess.org/bulkgame1">bulkgame1</a>`)
	assert.Contains(t, body, "by hand")
	assert.NotContains(t, body, "Retry game creation</button>")
	assert.Contains(t, body, "Game creation is switched off (pairing.game_creation = manual), so it can't be retried.")

	rec := postAs(t, srv, adminSession, path+"/create-games", url.Values{})
	assert.Equal(t, path+"?error=creation-off", rec.Header().Get("Location"))
	assert.Empty(t, sched.creations)

	setSetting(t, q, "pairing.game_creation", `"lichess"`)
	body = getAs(t, srv, adminSession, path).Body.String()
	assert.Contains(t, body, "Retry game creation</button>")

	// A job already waiting: nothing changes.
	sched.busy = true
	rec = postAs(t, srv, adminSession, path+"/create-games", url.Values{})
	assert.Equal(t, path+"?error=already-queued", rec.Header().Get("Location"))
	unchanged, err := q.GetRoundByID(ctx, round.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.GameCreationStateCreated, unchanged.GameCreation, "rolled back")

	sched.busy = false
	rec = postAs(t, srv, adminSession, path+"/create-games", url.Values{})
	assert.Equal(t, path, rec.Header().Get("Location"))
	assert.Equal(t, []scheduledCreation{{roundID: round.ID, retry: true}}, sched.creations)
	retried, err := q.GetRoundByID(ctx, round.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.GameCreationStateCreating, retried.GameCreation)
	assert.Contains(t, getAs(t, srv, adminSession, path).Body.String(), "The games are being created on Lichess")
}

func TestAdminRounds_NoRetryForARoundPublishedByHand(t *testing.T) {
	srv, q, tx := testServer(t)
	srv.sched = &recordingScheduler{}
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	setSetting(t, q, "pairing.game_creation", `"lichess"`)
	a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	r := thisWeekRound(t, q, 9101, gen.RoundStatePublished, "")
	pairIn(t, q, r, a, b)
	path := roundPath(r.ID)

	body := getAs(t, srv, adminSession, path).Body.String()
	assert.Contains(t, body, "Game creation was off: players start every game by hand.")
	assert.NotContains(t, body, "Retry game creation")

	rec := postAs(t, srv, adminSession, path+"/create-games", url.Values{})
	assert.Equal(t, path+"?error=not-creating", rec.Header().Get("Location"))
}

func TestDashboard_ThisWeekForGamesTheLeagueCreates(t *testing.T) {
	ctx := context.Background()

	t.Run("created: the game, nothing to start", func(t *testing.T) {
		srv, q, tx := testServer(t)
		a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
		b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
		c := addPlayer(t, q, tx, "Charlie", true, 1800, 0, 1800, 1800, "0")
		d := addPlayer(t, q, tx, "Delta", true, 1700, 0, 1700, 1700, "0")
		createdRound(t, q, a, b, c, d)

		body := getAs(t, srv, sessionFor(t, srv, a), "/account").Body.String()
		assert.Contains(t, body, "The league created the game on Lichess: nothing to start.")
		assert.Contains(t, body, `href="https://lichess.org/bulkgame1">open on Lichess`)
		assert.NotContains(t, body, "challenge them on Lichess")

		// The pairing left by hand reads as before.
		body = getAs(t, srv, sessionFor(t, srv, c), "/account").Body.String()
		assert.Contains(t, body, "challenge them on Lichess")
		assert.Contains(t, body, "Start the game on Lichess by hand")
	})

	t.Run("being created: no link yet", func(t *testing.T) {
		srv, q, tx := testServer(t)
		a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
		b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
		r := thisWeekRound(t, q, 9102, gen.RoundStatePublished, "")
		pairIn(t, q, r, a, b)
		_, err := q.StartGameCreation(ctx, gen.StartGameCreationParams{ID: r.ID, GameCreation: gen.GameCreationStateCreating})
		require.NoError(t, err)

		body := getAs(t, srv, sessionFor(t, srv, a), "/account").Body.String()
		assert.Contains(t, body, "Your game is being created on Lichess.")
		assert.NotContains(t, body, "challenge them on Lichess")
		assert.NotContains(t, body, "Start the game on Lichess by hand")
	})
}

// challengedRound is a round whose games the league created, with one
// pairing — white plays black — made a challenge from challenger.
func challengedRound(t *testing.T, q *gen.Queries, white, black, challenger gen.User) (gen.Round, gen.Pairing) {
	t.Helper()
	ctx := context.Background()
	r := thisWeekRound(t, q, 9103, gen.RoundStatePublished, `{"days_per_move": 2, "rated": true}`)
	p := pairIn(t, q, r, white, black)
	_, err := q.StartGameCreation(ctx, gen.StartGameCreationParams{ID: r.ID, GameCreation: gen.GameCreationStateCreating})
	require.NoError(t, err)
	challenge := "chal1"
	n, err := q.RecordChallenge(ctx, gen.RecordChallengeParams{
		ID:               p.ID,
		ChallengerUserID: challenger.ID,
		LichessGameID:    &challenge,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	r, err = q.FinishGameCreation(ctx, gen.FinishGameCreationParams{
		ID:             r.ID,
		Outcome:        gen.GameCreationStateCreated,
		GamesCreatedAt: r.PublishedAt,
	})
	require.NoError(t, err)
	return r, p
}

func TestDashboard_ThisWeekForAChallenge(t *testing.T) {
	// Spec §8.3: the challenger waits for the opponent, who must accept.
	srv, q, tx := testServer(t)
	a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	_, p := challengedRound(t, q, a, b, b)

	body := getAs(t, srv, sessionFor(t, srv, b), "/account").Body.String()
	assert.Contains(t, body, "We challenged them for you on Lichess; the game starts once they accept.")
	assert.Contains(t, body, `href="https://lichess.org/chal1">see the challenge`)
	assert.NotContains(t, body, "challenge them on Lichess")
	assert.NotContains(t, body, "Start the game on Lichess by hand")

	body = getAs(t, srv, sessionFor(t, srv, a), "/account").Body.String()
	assert.Contains(t, body, "They've challenged you on Lichess: accept it to start the game.")
	assert.Contains(t, body, `href="https://lichess.org/chal1">accept on Lichess`)
	assert.NotContains(t, body, "Start the game on Lichess by hand")

	// Accepted: the challenge is the game, and reads like any other.
	chal := "chal1"
	require.NoError(t, q.AttachGameToPairing(context.Background(), gen.AttachGameToPairingParams{
		ID:            p.ID,
		LichessGameID: &chal,
		Status:        gen.PairingStatusInProgress,
	}))
	body = getAs(t, srv, sessionFor(t, srv, a), "/account").Body.String()
	assert.Contains(t, body, `href="https://lichess.org/chal1">open on Lichess`)
	assert.NotContains(t, body, "challenged you")
}

func TestAdminRounds_AChallengeNamesWhoSentIt(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	round, _ := challengedRound(t, q, a, b, b)

	body := getAs(t, srv, adminSession, roundPath(round.ID)).Body.String()
	assert.Contains(t, body, "1 challenge waiting to be accepted.")
	assert.Contains(t, body, "challenge from Bravo")
	assert.Contains(t, body, `href="https://lichess.org/chal1">chal1</a>`)
	assert.NotContains(t, body, "left for the players to start by hand")
	assert.NotContains(t, body, "Retry game creation</button>", "a challenge is not the players' to start")
}

func TestHealth_WatchesCreateGames(t *testing.T) {
	srv, _, _ := testServer(t)
	_, resp := healthOf(t, srv)
	assert.Contains(t, resp.Jobs, "create-games")
}
