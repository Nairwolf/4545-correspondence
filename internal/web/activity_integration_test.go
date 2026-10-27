//go:build integration

package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

func failPath(round gen.Round, p gen.Pairing) string {
	return roundPath(round.ID) + "/pairings/" + p.ID.String() + "/fail"
}

func TestAdminRounds_MarkFailedWithdrawsAChallenge(t *testing.T) {
	// Spec §8.5: Mark failed on a challenge pairing queues its
	// withdrawal on Lichess with the change; without a job queue it is
	// refused, and the pairing stays as it was.
	srv, q, tx := testServer(t)
	_, adminSession := addAdmin(t, srv, q, tx)
	a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	round, p := challengedRound(t, q, a, b, b)

	rec := postAs(t, srv, adminSession, failPath(round, p), url.Values{})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	got, err := q.GetPairingByID(context.Background(), gen.GetPairingByIDParams{ID: p.ID, RoundID: round.ID})
	require.NoError(t, err)
	assert.Equal(t, gen.PairingStatusPending, got.Status)

	sched := &recordingScheduler{}
	srv.sched = sched
	rec = postAs(t, srv, adminSession, failPath(round, p), url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, []pgtype.UUID{p.ID}, sched.cancels)
}

// missedStart records that player left the round's challenge
// unanswered, as evaluate-activity does.
func missedStart(
	t *testing.T,
	q *gen.Queries,
	round gen.Round,
	p gen.Pairing,
	player gen.User,
	at time.Time,
) {
	t.Helper()
	n, err := q.InsertMissedStart(context.Background(), gen.InsertMissedStartParams{
		UserID:     player.ID,
		RoundID:    round.ID,
		PairingID:  p.ID,
		RecordedAt: pgtype.Timestamptz{Time: at, Valid: true},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

func TestDashboard_MissedStartWarning(t *testing.T) {
	// Spec §8.3, §6.4: a player one missed start from auto-pause sees the
	// warning under This week.
	srv, q, tx := testServer(t)
	a := addPlayer(t, q, tx, "Alpha", true, 2000, 0, 2000, 2000, "0")
	b := addPlayer(t, q, tx, "Bravo", true, 1900, 0, 1900, 1900, "0")
	round, p := challengedRound(t, q, a, b, b)
	_, err := tx.Exec(context.Background(), `UPDATE pairings SET status = 'failed' WHERE id = $1`, p.ID)
	require.NoError(t, err)
	missedStart(t, q, round, p, a, time.Now().Add(-time.Hour))
	session := sessionFor(t, srv, a)
	const warning = "You didn&#39;t accept your challenge in round 9103"

	body := getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, warning)
	assert.NotContains(t, getAs(t, srv, sessionFor(t, srv, b), "/account").Body.String(), warning)

	// With three to a pause, one miss is not yet worth a warning.
	setSetting(t, q, "activity.missed_starts_to_pause", `3`)
	assert.NotContains(t, getAs(t, srv, session, "/account").Body.String(), warning)
	setSetting(t, q, "activity.missed_starts_to_pause", `2`)

	// Paused, the banner speaks instead; resuming ends the run.
	_, err = tx.Exec(context.Background(), `UPDATE player_profiles SET auto_paused_at = now() WHERE user_id = $1`, a.ID)
	require.NoError(t, err)
	body = getAs(t, srv, session, "/account").Body.String()
	assert.Contains(t, body, "Challenges sent to you went unanswered")
	assert.NotContains(t, body, warning)

	rec := postAs(t, srv, session, "/account/resume", url.Values{})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	profile := profileOf(t, q, a)
	assert.False(t, profile.AutoPausedAt.Valid)
	assert.True(t, profile.ResumedAt.Valid, "the resume is when the run ends")
	assert.NotContains(t, getAs(t, srv, session, "/account").Body.String(), warning)
}

func TestHealth_WatchesEvaluateActivity(t *testing.T) {
	srv, _, _ := testServer(t)
	_, resp := healthOf(t, srv)
	assert.Contains(t, resp.Jobs, "evaluate-activity")
}
