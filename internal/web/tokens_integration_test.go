//go:build integration

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
	"github.com/nairwolf/4545-correspondence/internal/tokenhealth"
)

// setSetting stores one settings row, as `ic setting` does.
func setSetting(t *testing.T, q *gen.Queries, key, value string) {
	t.Helper()
	_, err := q.UpsertSetting(context.Background(), gen.UpsertSettingParams{Key: key, Value: []byte(value)})
	require.NoError(t, err)
}

// recordValidateTokens writes a finished validate-tokens run that found
// organiser, as the job would.
func recordValidateTokens(t *testing.T, q *gen.Queries, status gen.JobRunStatus, organiser *tokenhealth.Organiser) {
	t.Helper()
	ctx := context.Background()
	run, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "validate-tokens"})
	require.NoError(t, err)
	detail, err := json.Marshal(map[string]any{"organiser_token": organiser})
	require.NoError(t, err)
	var errText *string
	if status == gen.JobRunStatusFailed {
		msg := "test tokens: lichess: 503"
		errText = &msg
	}
	require.NoError(t, q.FinishJobRun(ctx, gen.FinishJobRunParams{
		ID: run.ID, Status: status, Error: errText, Detail: detail,
	}))
}

// giveToken stores a token for u, then applies sql to its row (e.g.
// "revoked_at = now() - interval '3 days'"); "" leaves it valid.
func giveToken(t *testing.T, srv *Server, q *gen.Queries, tx pgx.Tx, u gen.User, sql string) {
	t.Helper()
	ctx := context.Background()
	sealed, err := srv.tokenKey.Seal(tokencrypt.Secret("lio_" + u.LichessUserID))
	require.NoError(t, err)
	require.NoError(t, q.UpsertOAuthToken(ctx, gen.UpsertOAuthTokenParams{
		UserID: u.ID, AccessToken: sealed, Scopes: []string{"challenge:write"},
	}))
	if sql != "" {
		_, err := tx.Exec(ctx, "UPDATE oauth_tokens SET "+sql+" WHERE user_id = $1", u.ID)
		require.NoError(t, err)
	}
}

func healthOf(t *testing.T, srv *Server) (int, healthResponse) {
	t.Helper()
	code, body := get(t, srv, "/health")
	var resp healthResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp), body)
	return code, resp
}

func TestHealth_OrganiserToken(t *testing.T) {
	srv, q, _ := testServer(t)
	expires := func(d time.Duration) *time.Time { t := time.Now().Add(d).UTC(); return &t }

	code, resp := healthOf(t, srv)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "manual", resp.GameCreation)
	assert.Equal(t, "unknown", resp.OrganiserToken.State, "never checked")
	assert.Contains(t, resp.Jobs, "validate-tokens")

	recordValidateTokens(t, q, gen.JobRunStatusSucceeded, &tokenhealth.Organiser{State: tokenhealth.OrganiserInvalid})
	code, resp = healthOf(t, srv)
	assert.Equal(t, http.StatusOK, code, "under manual nothing uses the organiser token")
	assert.Equal(t, "invalid", resp.OrganiserToken.State)

	setSetting(t, q, "pairing.game_creation", `"lichess"`)
	code, resp = healthOf(t, srv)
	assert.Equal(t, http.StatusServiceUnavailable, code, "the site cannot create games: the P1 alert")
	assert.Equal(t, "lichess", resp.GameCreation)

	recordValidateTokens(t, q, gen.JobRunStatusSucceeded, &tokenhealth.Organiser{
		State: tokenhealth.OrganiserValid, Account: "league", Expires: expires(-time.Hour),
	})
	code, resp = healthOf(t, srv)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "expired", resp.OrganiserToken.State, "valid at the check, expired since")

	recordValidateTokens(t, q, gen.JobRunStatusSucceeded, &tokenhealth.Organiser{
		State: tokenhealth.OrganiserValid, Account: "league", Expires: expires(300 * 24 * time.Hour),
	})
	code, resp = healthOf(t, srv)
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "valid", resp.OrganiserToken.State)
	assert.Equal(t, "league", resp.OrganiserToken.Account)
	assert.NotEmpty(t, resp.OrganiserToken.ExpiresAt)
	assert.NotEmpty(t, resp.OrganiserToken.CheckedAt)

	// A run that could not reach Lichess learned nothing: the state found
	// before stands, and the failed run itself is what turns /health red.
	recordValidateTokens(t, q, gen.JobRunStatusFailed, nil)
	code, resp = healthOf(t, srv)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "valid", resp.OrganiserToken.State)
	assert.Equal(t, "failed", resp.Jobs["validate-tokens"].LastStatus)
}

func TestHealth_InvalidSettingsRow(t *testing.T) {
	srv, q, _ := testServer(t)
	// Written past `ic setting`'s validation, as `make psql` could.
	setSetting(t, q, "pairing.game_creation", `"sometimes"`)

	code, resp := healthOf(t, srv)
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, "invalid", resp.Settings)
}

func TestAdminBanner(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	_, session := addAdmin(t, srv, q, tx)
	_, err := tx.Exec(ctx, `UPDATE rounds SET state = 'cancelled' WHERE state = 'draft'`)
	require.NoError(t, err)

	body := getAs(t, srv, session, "/admin/rounds").Body.String()
	assert.NotContains(t, body, "waiting for review")
	assert.NotContains(t, body, "organiser token", "under manual the organiser token is not watched")

	draft := thisWeekRound(t, q, 90001, gen.RoundStateDraft, "")
	run, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "sync-games"})
	require.NoError(t, err)
	require.NoError(t, q.FinishJobRun(ctx, gen.FinishJobRunParams{ID: run.ID, Status: gen.JobRunStatusFailed}))

	for _, page := range []string{"/admin/rounds", "/admin/registrations", "/admin/jobs", "/admin/tokens"} {
		body = getAs(t, srv, session, page).Body.String()
		assert.Contains(t, body, "Round 90001 is a draft waiting for review. It publishes itself at", page)
		assert.Contains(t, body, fmt.Sprintf(`href="/admin/rounds/%d"`, draft.ID), page)
		assert.Contains(t, body, "The latest sync-games run failed", page)
	}
	body = getAs(t, srv, session, "/standings").Body.String()
	assert.NotContains(t, body, "waiting for review", "the banner is on admin pages only")

	setSetting(t, q, "pairing.game_creation", `"lichess"`)
	body = getAs(t, srv, session, "/admin/rounds").Body.String()
	assert.Contains(t, body, "The organiser token has not been checked yet")

	soon := time.Now().Add(10 * 24 * time.Hour).UTC()
	recordValidateTokens(t, q, gen.JobRunStatusSucceeded, &tokenhealth.Organiser{
		State: tokenhealth.OrganiserValid, Account: "league", Expires: &soon,
	})
	body = getAs(t, srv, session, "/admin/rounds").Body.String()
	assert.Contains(t, body, "The organiser token expires on "+soon.Format("2 January 2006")+". Replace it before then.")

	recordValidateTokens(t, q, gen.JobRunStatusSucceeded, &tokenhealth.Organiser{State: tokenhealth.OrganiserNotSet})
	body = getAs(t, srv, session, "/admin/rounds").Body.String()
	assert.Contains(t, body, "LICHESS_ORG_TOKEN is not set. The site cannot create games until it is fixed.")
}

func TestAdminTokens(t *testing.T) {
	srv, q, tx := testServer(t)
	ctx := context.Background()
	admin, session := addAdmin(t, srv, q, tx)
	// The page lists every approved player; only this test's count.
	_, err := tx.Exec(ctx, `UPDATE users SET status = 'pending' WHERE status = 'approved' AND role <> 'admin'`)
	require.NoError(t, err)

	valid := addPlayer(t, q, tx, "Valid", true, 1800, 0, 1800, 1800, "0")
	giveToken(t, srv, q, tx, valid, "")
	addPlayer(t, q, tx, "Seeded", true, 1800, 0, 1800, 1800, "0")
	recent := addPlayer(t, q, tx, "Recent", true, 1800, 0, 1800, 1800, "0")
	giveToken(t, srv, q, tx, recent, "revoked_at = now() - interval '3 days'")
	old := addPlayer(t, q, tx, "Old", true, 1800, 0, 1800, 1800, "0")
	giveToken(t, srv, q, tx, old, "expires_at = now() - interval '20 days'")
	paused := addPlayer(t, q, tx, "Paused", true, 1800, 0, 1800, 1800, "0")
	_, err = tx.Exec(ctx, `UPDATE player_profiles SET paused_by_admin = true WHERE user_id = $1`, paused.ID)
	require.NoError(t, err)
	// The admin signs in like anyone else, so they have a token too.
	giveToken(t, srv, q, tx, admin, "")

	player := sessionFor(t, srv, valid)
	assert.Equal(t, http.StatusForbidden, getAs(t, srv, player, "/admin/tokens").Code)

	rec := getAs(t, srv, session, "/admin/tokens")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Game creation is <span class=\"font-medium text-zinc-200\">manual</span>")
	assert.Contains(t, body, "Players without a valid token (4)")
	assert.NotContains(t, body, `/players/Valid"`)
	assert.Contains(t, body, "never authorised")
	assert.Contains(t, body, "revoked")
	assert.Contains(t, body, "expired")
	assert.Contains(t, body, "(paused by an admin)")
	assert.Contains(t, body, "not gated (manual)")
	assert.Contains(t, body, "Not checked yet.", "no organiser check recorded")

	setSetting(t, q, "pairing.game_creation", `"lichess"`)
	recordValidateTokens(t, q, gen.JobRunStatusSucceeded, &tokenhealth.Organiser{State: tokenhealth.OrganiserValid, Account: "league"})
	body = getAs(t, srv, session, "/admin/tokens").Body.String()
	assert.NotContains(t, body, "not gated (manual)")
	assert.Contains(t, body, "11 days of grace left", "Recent lapsed three days ago")
	assert.Contains(t, body, "left out", "Seeded, Old and Paused")
	assert.Contains(t, body, "never expires")
	assert.Contains(t, body, `href="https://lichess.org/@/league"`)
}

func TestDashboard_TokenBannerFollowsGameCreation(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Lapsed", true, 1800, 10, 1800, 1800, "20")
	session := sessionFor(t, srv, u)
	account := func() string { return getAs(t, srv, session, "/account").Body.String() }

	// Manual: the banner says what is wrong, without a countdown.
	body := account()
	assert.Contains(t, body, "Games can't be created for you automatically")
	assert.NotContains(t, body, "not being paired")

	setSetting(t, q, "pairing.game_creation", `"lichess"`)
	body = account()
	assert.Contains(t, body, "You're not being paired until you re-authorise.", "no token: left out at once")
	assert.Contains(t, body, "You haven't authorised the league on Lichess yet")

	giveToken(t, srv, q, tx, u, "revoked_at = now() - interval '3 days'")
	body = account()
	assert.Contains(t, body, "Your games can no longer be created automatically.")
	assert.Contains(t, body, "was found revoked on "+time.Now().AddDate(0, 0, -3).UTC().Format("2 January 2006"))
	assert.Contains(t, body, "Re-authorise within 11 days, or you'll stop being paired until you do.")

	_, err := tx.Exec(context.Background(), `UPDATE oauth_tokens SET revoked_at = now() - interval '20 days' WHERE user_id = $1`, u.ID)
	require.NoError(t, err)
	body = account()
	assert.Contains(t, body, "You're not being paired until you re-authorise.")
	assert.Contains(t, body, "you're paired again from the next round")

	// Re-authorising (signing in again) clears it at once.
	giveToken(t, srv, q, tx, u, "")
	body = account()
	assert.NotContains(t, body, "Re-authorise within")
	assert.NotContains(t, body, "not being paired")
	assert.Contains(t, body, "last checked on "+time.Now().UTC().Format("2 January 2006"))

	// The round they sat out says why.
	r := thisWeekRound(t, q, 90002, gen.RoundStatePublished, "")
	excludeIn(t, q, r, u, gen.ExclusionReasonNoValidToken, nil, nil)
	assert.Contains(t, account(), "without your Lichess authorisation, so you sat out round 90002")
}
