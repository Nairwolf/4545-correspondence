//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// isolatePairingPool takes every pre-existing approved user out of the
// pool and cancels any pre-existing draft, so a round-generation test
// pairs only the fixtures it adds. Without it, the pairing pool is
// "every approved user" and the dev database's seeded league would
// take part in every one of these tests.
func isolatePairingPool(t *testing.T, tx pgx.Tx) {
	t.Helper()
	ctx := context.Background()
	_, err := tx.Exec(ctx, `UPDATE users SET status = 'pending' WHERE status = 'approved'`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE rounds SET state = 'cancelled' WHERE state = 'draft'`)
	require.NoError(t, err)
}

func TestAdminRounds_RoleChecks(t *testing.T) {
	srv, q, tx := testServer(t)
	isolatePairingPool(t, tx)
	player := addPlayer(t, q, tx, "Pleb", true, 1500, 0, 1500, 1500, "0")
	playerSession := sessionFor(t, srv, player)

	for _, path := range []string{"/admin/rounds", "/admin/rounds/1"} {
		rec := getAs(t, srv, nil, path)
		assert.Equal(t, http.StatusFound, rec.Code, "anonymous %s", path)
		assert.Equal(t, "/login", rec.Header().Get("Location"))
		assert.Equal(t, http.StatusForbidden, getAs(t, srv, playerSession, path).Code, "player %s", path)
	}
	rec := postAs(t, srv, playerSession, "/admin/rounds/generate", url.Values{})
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// generateDraft runs a real generation through the admin handler,
// against a pool built with addPlayer, and returns the resulting round
// id and its pairings.
func generateDraft(t *testing.T, srv *Server, q *gen.Queries, admin *http.Cookie) (int32, []gen.ListPairingsForRoundRow) {
	t.Helper()
	ctx := context.Background()

	rec := postAs(t, srv, admin, "/admin/rounds/generate", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	loc := rec.Header().Get("Location")
	n, err := strconv.Atoi(strings.TrimPrefix(loc, "/admin/rounds/"))
	require.NoError(t, err, "Location: %s", loc)
	id := int32(n)

	pairings, err := q.ListPairingsForRound(ctx, id)
	require.NoError(t, err)
	return id, pairings
}

func TestAdminRounds_GenerateAndView(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")

	id, pairings := generateDraft(t, srv, q, adminSession)
	require.Len(t, pairings, 1)

	body := getAs(t, srv, adminSession, roundPath(id)).Body.String()
	assert.Contains(t, body, "Alpha")
	assert.Contains(t, body, "Bravo")
	assert.Contains(t, body, "Draft")

	runs, err := q.ListRecentJobRuns(context.Background(), 20)
	require.NoError(t, err)
	require.NotEmpty(t, runs)
	assert.Equal(t, "generate-round", runs[0].JobName)
	assert.Equal(t, gen.JobRunStatusSucceeded, runs[0].Status)
}

func TestAdminRounds_GenerateTwiceFails(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")

	generateDraft(t, srv, q, adminSession)

	rec := postAs(t, srv, adminSession, "/admin/rounds/generate", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	loc := rec.Header().Get("Location")
	assert.Equal(t, "/admin/rounds?error=draft-exists", loc)

	body := getAs(t, srv, adminSession, loc).Body.String()
	assert.Contains(t, body, "already waiting for review")

	// ORDER BY started_at is not enough to find the latest run here:
	// both generate-round calls landed inside this test's one wrapping
	// transaction, where now() is frozen to the transaction's start and
	// so is identical for both rows. The row ids are still monotonic.
	runs, err := q.ListRecentJobRuns(context.Background(), 20)
	require.NoError(t, err)
	latest := latestGenerateRoundRun(t, runs)
	assert.Equal(t, gen.JobRunStatusFailed, latest.Status, "a second generation is a FAILED run, not a silent no-op")
}

func TestAdminRounds_PublishNow(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, _ := generateDraft(t, srv, q, adminSession)

	rec := postAs(t, srv, adminSession, roundPath(id)+"/publish", url.Values{})
	assert.Equal(t, http.StatusSeeOther, rec.Code)

	round, err := q.GetRoundByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, gen.RoundStatePublished, round.State)
	assert.Equal(t, round.PublishedAt.Time, round.PairAt.Time)
}

func TestAdminRounds_CancelRequiresAReason(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, _ := generateDraft(t, srv, q, adminSession)

	rec := postAs(t, srv, adminSession, roundPath(id)+"/cancel", url.Values{})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Contains(t, rec.Header().Get("Location"), "error=reason")

	round, err := q.GetRoundByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, gen.RoundStateDraft, round.State, "nothing changed")

	rec = postAs(t, srv, adminSession, roundPath(id)+"/cancel", url.Values{"reason": {"bad pool"}})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/admin/rounds", rec.Header().Get("Location"))

	round, err = q.GetRoundByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, gen.RoundStateCancelled, round.State)
	require.NotNil(t, round.Notes)
	assert.Equal(t, "bad pool", *round.Notes)
}

func TestAdminRounds_Regenerate(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, before := generateDraft(t, srv, q, adminSession)
	require.Len(t, before, 1)

	rec := postAs(t, srv, adminSession, roundPath(id)+"/regenerate", url.Values{})
	assert.Equal(t, http.StatusSeeOther, rec.Code)

	after, err := q.ListPairingsForRound(context.Background(), id)
	require.NoError(t, err)
	assert.Len(t, after, 1, "regeneration replaces, not adds")
}

func TestAdminRounds_FlipPairing(t *testing.T) {
	srv, q, tx := testServer(t)
	admin, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, pairings := generateDraft(t, srv, q, adminSession)
	require.Len(t, pairings, 1)
	before := pairings[0]

	rec := postAs(t, srv, adminSession, roundPath(id)+"/pairings/"+before.ID.String()+"/flip", url.Values{})
	assert.Equal(t, http.StatusSeeOther, rec.Code)

	after, err := q.ListPairingsForRound(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, before.BlackUserID, after[0].WhiteUserID)
	assert.Equal(t, before.WhiteUserID, after[0].BlackUserID)
	assert.Equal(t, admin.ID, after[0].EditedBy, "edited_by records who made the change")
	assert.Nil(t, after[0].RatingGap, "diagnostics are nulled, not stale")
	assert.Nil(t, after[0].ColorPenalty)
}

func TestAdminRounds_RemovePairing(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, pairings := generateDraft(t, srv, q, adminSession)
	require.Len(t, pairings, 1)

	rec := postAs(t, srv, adminSession, roundPath(id)+"/pairings/"+pairings[0].ID.String()+"/remove", url.Values{})
	assert.Equal(t, http.StatusSeeOther, rec.Code)

	after, err := q.ListPairingsForRound(context.Background(), id)
	require.NoError(t, err)
	assert.Empty(t, after)

	exclusions, err := q.ListExclusionsForRound(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, exclusions, 2)
	for _, e := range exclusions {
		assert.Equal(t, gen.ExclusionReasonRemovedByAdmin, e.Reason)
	}
}

func TestAdminRounds_EditsOnAPublishedRoundConflict(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, pairings := generateDraft(t, srv, q, adminSession)
	require.Len(t, pairings, 1)

	require.Equal(t, http.StatusSeeOther, postAs(t, srv, adminSession, roundPath(id)+"/publish", url.Values{}).Code)

	pid := pairings[0].ID.String()
	for _, path := range []string{
		roundPath(id) + "/pairings/" + pid + "/flip",
		roundPath(id) + "/pairings/" + pid + "/remove",
		roundPath(id) + "/regenerate",
	} {
		rec := postAs(t, srv, adminSession, path, url.Values{})
		assert.Equal(t, http.StatusConflict, rec.Code, "%s on a published round", path)
	}
}

func TestAdminRounds_FailPairing(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	id, pairings := generateDraft(t, srv, q, adminSession)
	require.Len(t, pairings, 1)
	require.Equal(t, http.StatusSeeOther, postAs(t, srv, adminSession, roundPath(id)+"/publish", url.Values{}).Code)

	rec := postAs(t, srv, adminSession, roundPath(id)+"/pairings/"+pairings[0].ID.String()+"/fail", url.Values{})
	assert.Equal(t, http.StatusSeeOther, rec.Code)

	after, err := q.ListPairingsForRound(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, gen.PairingStatusFailed, after[0].Status)

	// A draft's pairing cannot be marked failed at all.
	addPlayer(t, q, tx, "Charlie", true, 1800, 0, 1800, 1800, "0")
	addPlayer(t, q, tx, "Delta", true, 1700, 0, 1700, 1700, "0")
	draftID, draftPairings := generateDraft(t, srv, q, adminSession)
	rec = postAs(t, srv, adminSession, roundPath(draftID)+"/pairings/"+draftPairings[0].ID.String()+"/fail", url.Values{})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminRounds_SwapPairings(t *testing.T) {
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	isolatePairingPool(t, tx)
	addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	addPlayer(t, q, tx, "Charlie", true, 1800, 0, 1800, 1800, "0")
	addPlayer(t, q, tx, "Delta", true, 1700, 0, 1700, 1700, "0")
	id, pairings := generateDraft(t, srv, q, adminSession)
	require.Len(t, pairings, 2)

	rec := postAs(t, srv, adminSession, roundPath(id)+"/pairings/swap", url.Values{
		"a": {pairings[0].ID.String()},
		"b": {pairings[1].ID.String()},
	})
	assert.Equal(t, http.StatusSeeOther, rec.Code)

	after, err := q.ListPairingsForRound(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, after, 2)

	origPairs := map[[2]string]bool{}
	for _, p := range pairings {
		origPairs[[2]string{p.WhiteUserID.String(), p.BlackUserID.String()}] = true
	}
	for _, p := range after {
		assert.False(t, origPairs[[2]string{p.WhiteUserID.String(), p.BlackUserID.String()}], "a swap must change both pairs")
	}
}

func latestGenerateRoundRun(t *testing.T, runs []gen.JobRun) gen.JobRun {
	t.Helper()
	var latest gen.JobRun
	for _, r := range runs {
		if r.JobName == "generate-round" && r.ID > latest.ID {
			latest = r
		}
	}
	require.NotZero(t, latest.ID, "no generate-round run recorded")
	return latest
}

func roundPath(id int32) string {
	return "/admin/rounds/" + strconv.Itoa(int(id))
}
