package lichess

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// The game-creation half of Fake stands in for Lichess in the tests of
// later steps, so the Lichess behaviour they rely on is pinned here.

func fakeWithPlayers() *Fake {
	f := NewFake()
	for _, name := range []string{"sai", "nushi", "kaa"} {
		f.TokenInfo[tokencrypt.Secret("lip_"+name)] = TokenInfo{
			UserID: name,
			Scopes: []string{ScopeChallengeWrite},
		}
	}
	return f
}

func TestFake_CreateBulkPairing(t *testing.T) {
	f := fakeWithPlayers()
	req := BulkPairingRequest{
		Pairs: []BulkPair{{White: "lip_sai", Black: "lip_nushi"}, {White: "lip_nushi", Black: "lip_kaa"}},
		Days:  3,
		Rated: true,
	}

	bulk, err := f.CreateBulkPairing(context.Background(), "lip_organiser", req)
	require.NoError(t, err)
	assert.Equal(t, "bulk1", bulk.ID)
	assert.Equal(t, []BulkGame{
		{ID: "game2", White: "sai", Black: "nushi"},
		{ID: "game3", White: "nushi", Black: "kaa"},
	}, bulk.Games)
	assert.Equal(t, 3, bulk.Correspondence.DaysPerTurn)
	assert.NotZero(t, bulk.ScheduledAt)
	assert.Equal(t, []BulkPairingRequest{req}, f.BulkRequests)
	assert.Equal(t, []FakeCall{{Method: "CreateBulkPairing", Token: "lip_organiser"}}, f.Calls)

	listed, err := f.ListBulkPairings(context.Background(), "lip_organiser")
	require.NoError(t, err)
	assert.Equal(t, []BulkPairing{bulk}, listed)
}

func TestFake_CreateBulkPairingIsAllOrNothing(t *testing.T) {
	f := fakeWithPlayers()
	f.TokenInfo["lip_noscope"] = TokenInfo{UserID: "noscope", Scopes: []string{"msg:write"}}
	f.RejectTokens["lip_kaa"] = "Account closed"

	_, err := f.CreateBulkPairing(context.Background(), "lip_organiser", BulkPairingRequest{
		Pairs: []BulkPair{
			{White: "lip_sai", Black: "lip_nushi"},
			{White: "lip_kaa", Black: "lip_ghost"},
			{White: "lip_noscope", Black: "lip_sai"},
		},
		Days: 2,
	})
	var rejection *BulkRejection
	require.ErrorAs(t, err, &rejection)
	assert.Equal(t, map[tokencrypt.Secret]string{
		"lip_kaa":     "Account closed",
		"lip_ghost":   "No such token",
		"lip_noscope": "Missing scope challenge:write",
	}, rejection.Tokens)
	assert.Empty(t, f.Bulks, "a refused bulk creates nothing")
	assert.Empty(t, f.BulkRequests)
}

func TestFake_NoOrganiserToken(t *testing.T) {
	f := fakeWithPlayers()
	_, err := f.CreateBulkPairing(context.Background(), "", BulkPairingRequest{
		Pairs: []BulkPair{{White: "lip_sai", Black: "lip_nushi"}},
		Days:  2,
	})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 401, apiErr.StatusCode)
	assert.Empty(t, f.Bulks)
}

func TestFake_TestTokens(t *testing.T) {
	f := fakeWithPlayers()
	got, err := f.TestTokens(context.Background(), []tokencrypt.Secret{"lip_sai", "lip_ghost"})
	require.NoError(t, err)
	require.NotNil(t, got["lip_sai"])
	assert.Equal(t, "sai", got["lip_sai"].UserID)
	info, present := got["lip_ghost"]
	assert.True(t, present)
	assert.Nil(t, info)
	assert.Equal(t, []FakeCall{{Method: "TestTokens"}}, f.Calls)
}

func TestFake_Challenges(t *testing.T) {
	f := fakeWithPlayers()

	ch, err := f.CreateChallenge(context.Background(), "lip_sai", "Nushi", ChallengeRequest{
		Days:  2,
		Rated: true,
		Color: ColorBlack,
	})
	require.NoError(t, err)
	assert.Equal(t, "chal1", ch.ID)
	assert.Equal(t, "sai", ch.Challenger.ID)
	assert.Equal(t, "nushi", ch.DestUser.ID)
	assert.Equal(t, "black", ch.FinalColor)

	_, err = f.CreateChallenge(context.Background(), "lip_ghost", "Nushi", ChallengeRequest{Days: 2})
	assert.Error(t, err, "a challenger whose token Lichess does not know")

	var apiErr *APIError
	require.ErrorAs(t, f.CancelChallenge(context.Background(), "lip_nushi", ch.ID), &apiErr,
		"only the challenger can cancel")
	assert.Equal(t, 404, apiErr.StatusCode)

	require.NoError(t, f.CancelChallenge(context.Background(), "lip_sai", ch.ID))
	assert.Equal(t, "canceled", f.Challenges[0].Status)
	assert.Equal(t, []FakeCall{
		{Method: "CreateChallenge", Token: "lip_sai"},
		{Method: "CreateChallenge", Token: "lip_ghost"},
		{Method: "CancelChallenge", Token: "lip_nushi"},
		{Method: "CancelChallenge", Token: "lip_sai"},
	}, f.Calls)
}

func TestFake_PerMethodErrors(t *testing.T) {
	f := fakeWithPlayers()
	boom := assert.AnError
	f.ChallengeErrFor["Kaa"] = boom

	_, err := f.CreateChallenge(context.Background(), "lip_sai", "Kaa", ChallengeRequest{Days: 2})
	assert.ErrorIs(t, err, boom)
	_, err = f.CreateChallenge(context.Background(), "lip_sai", "Nushi", ChallengeRequest{Days: 2})
	assert.NoError(t, err, "the other challenges are unaffected")
	assert.Len(t, f.Challenges, 1)
}
