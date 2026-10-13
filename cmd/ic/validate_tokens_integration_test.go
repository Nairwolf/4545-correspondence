//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
	"github.com/nairwolf/4545-correspondence/internal/tokenhealth"
)

// tokenTest is one validate-tokens run's world: a transaction with no
// tokens but the test's own, the key they are sealed with, and the
// Fake that plays Lichess.
type tokenTest struct {
	tx   pgx.Tx
	q    *gen.Queries
	key  tokencrypt.Key
	fake *lichess.Fake
}

func newTokenTest(t *testing.T) tokenTest {
	t.Helper()
	tx := testTx(t)
	// validate-tokens reads every stored token. Whatever the database
	// already holds is not this test's, and is rolled back with it.
	_, err := tx.Exec(context.Background(), `DELETE FROM oauth_tokens`)
	require.NoError(t, err)
	key, err := tokencrypt.ParseKey(strings.Repeat("cd", 32))
	require.NoError(t, err)
	return tokenTest{tx: tx, q: gen.New(tx), key: key, fake: lichess.NewFake()}
}

// player creates an approved player whose token is secret, sealed as
// sign-in seals it. lichessScopes is what Lichess will say the token
// carries; nil means Lichess does not know it.
func (tt tokenTest) player(t *testing.T, name string, secret tokencrypt.Secret, lichessScopes []string) gen.User {
	t.Helper()
	user := createUser(t, tt.q, name)
	tt.storeToken(t, user, secret)
	if lichessScopes != nil {
		tt.fake.TokenInfo[secret] = lichess.TokenInfo{UserID: name, Scopes: lichessScopes}
	}
	return user
}

func (tt tokenTest) storeToken(t *testing.T, user gen.User, secret tokencrypt.Secret) {
	t.Helper()
	sealed, err := tt.key.Seal(secret)
	require.NoError(t, err)
	require.NoError(t, tt.q.UpsertOAuthToken(context.Background(), gen.UpsertOAuthTokenParams{
		UserID:      user.ID,
		AccessToken: sealed,
		Scopes:      []string{lichess.ScopeChallengeWrite},
	}))
}

func (tt tokenTest) run(t *testing.T, client lichess.API, organiser tokencrypt.Secret, riverJobID int64) error {
	t.Helper()
	return runValidateTokens(context.Background(), tt.tx, client, tt.key, organiser, &riverJobID)
}

func (tt tokenTest) token(t *testing.T, user gen.User) gen.OauthToken {
	t.Helper()
	tok, err := tt.q.GetOAuthToken(context.Background(), user.ID)
	require.NoError(t, err)
	return tok
}

func (tt tokenTest) notices(t *testing.T, user gen.User) []gen.Notification {
	t.Helper()
	ns, err := tt.q.ListNotificationsForUser(context.Background(), gen.ListNotificationsForUserParams{
		UserID: user.ID,
		Limit:  50,
	})
	require.NoError(t, err)
	return ns
}

func runDetail(t *testing.T, run gen.JobRun) validateTokensResult {
	t.Helper()
	var detail validateTokensResult
	require.NoError(t, json.Unmarshal(run.Detail, &detail))
	return detail
}

func TestRunValidateTokens_RevokesWhatLichessRefusesAndTellsThePlayerOnce(t *testing.T) {
	tt := newTokenTest(t)
	ctx := context.Background()

	expires := time.Now().Add(200 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	valid := tt.player(t, "valid", "tok-valid", []string{lichess.ScopeChallengeWrite})
	tt.fake.TokenInfo["tok-valid"] = lichess.TokenInfo{
		UserID:  "valid",
		Scopes:  []string{lichess.ScopeChallengeWrite},
		Expires: expires,
	}
	revoked := tt.player(t, "revoked", "tok-revoked", nil)
	noScope := tt.player(t, "no-scope", "tok-no-scope", []string{"preference:read"})

	// A rejected applicant's token is never used, so it is not tested.
	rejected := tt.player(t, "rejected", "tok-rejected", nil)
	_, err := tt.tx.Exec(ctx, `UPDATE users SET status = 'rejected' WHERE id = $1`, rejected.ID)
	require.NoError(t, err)

	// Back-date the valid token's last check so the refresh is visible.
	_, err = tt.tx.Exec(ctx, `UPDATE oauth_tokens SET last_validated_at = now() - interval '7 days' WHERE user_id = $1`, valid.ID)
	require.NoError(t, err)
	before := tt.token(t, valid).LastValidatedAt.Time

	require.NoError(t, tt.run(t, tt.fake, "", 7001))

	run := jobRunByRiverID(t, tt.q, 7001)
	assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
	assert.Nil(t, run.Error)
	detail := runDetail(t, run)
	assert.Equal(t, 3, detail.Tested, "the rejected applicant's token is left out")
	assert.Equal(t, 1, detail.Valid)
	assert.Equal(t, 2, detail.Revoked)

	tok := tt.token(t, valid)
	assert.False(t, tok.RevokedAt.Valid)
	assert.True(t, tok.LastValidatedAt.Time.After(before), "the check time is refreshed")
	require.True(t, tok.ExpiresAt.Valid)
	assert.True(t, expires.Equal(tok.ExpiresAt.Time), "the expiry Lichess reports is stored: %s", tok.ExpiresAt.Time)
	assert.Empty(t, tt.notices(t, valid))

	for _, user := range []gen.User{revoked, noScope} {
		assert.True(t, tt.token(t, user).RevokedAt.Valid, "%s's token is marked revoked", user.LichessUsername)
		ns := tt.notices(t, user)
		require.Len(t, ns, 1, "%s is told", user.LichessUsername)
		assert.Equal(t, string(notify.Token), ns[0].Category)
		assert.Equal(t, "/account", *ns[0].LinkUrl)
	}
	assert.False(t, tt.token(t, rejected).RevokedAt.Valid)
	assert.Empty(t, tt.notices(t, rejected))

	// The next week: the revoked tokens are not tested again, and nobody
	// is told twice.
	require.NoError(t, tt.run(t, tt.fake, "", 7002))
	detail = runDetail(t, jobRunByRiverID(t, tt.q, 7002))
	assert.Equal(t, 1, detail.Tested)
	assert.Equal(t, 0, detail.Revoked)
	assert.Len(t, tt.notices(t, revoked), 1)

	// Signing in again stores a new token and clears the revocation: it
	// is tested again from then on.
	tt.storeToken(t, revoked, "tok-revoked-2")
	tt.fake.TokenInfo["tok-revoked-2"] = lichess.TokenInfo{UserID: "revoked", Scopes: []string{lichess.ScopeChallengeWrite}}
	require.NoError(t, tt.run(t, tt.fake, "", 7003))
	detail = runDetail(t, jobRunByRiverID(t, tt.q, 7003))
	assert.Equal(t, 2, detail.Tested)
	assert.Equal(t, 2, detail.Valid)
	assert.False(t, tt.token(t, revoked).RevokedAt.Valid)
}

// signInDuringTest is a Lichess whose token test runs while the player
// signs in again: it answers that the old token is invalid, but by then
// the stored row holds a new one.
type signInDuringTest struct {
	*lichess.Fake
	signIn func()
}

func (s signInDuringTest) TestTokens(
	ctx context.Context,
	tokens []tokencrypt.Secret,
) (map[tokencrypt.Secret]*lichess.TokenInfo, error) {
	s.signIn()
	return s.Fake.TestTokens(ctx, tokens)
}

func TestRunValidateTokens_LeavesATokenReplacedDuringTheRunAlone(t *testing.T) {
	tt := newTokenTest(t)
	player := tt.player(t, "player", "tok-old", nil) // Lichess no longer knows the old one

	client := signInDuringTest{Fake: tt.fake, signIn: func() { tt.storeToken(t, player, "tok-new") }}
	require.NoError(t, tt.run(t, client, "", 7011))

	assert.Equal(t, 0, runDetail(t, jobRunByRiverID(t, tt.q, 7011)).Revoked)
	assert.False(t, tt.token(t, player).RevokedAt.Valid, "the new token was never tested")
	assert.Empty(t, tt.notices(t, player))
}

// unanswering is a Lichess whose answer leaves one token out.
type unanswering struct {
	*lichess.Fake
	skip tokencrypt.Secret
}

func (u unanswering) TestTokens(
	ctx context.Context,
	tokens []tokencrypt.Secret,
) (map[tokencrypt.Secret]*lichess.TokenInfo, error) {
	answers, err := u.Fake.TestTokens(ctx, tokens)
	delete(answers, u.skip)
	return answers, err
}

func TestRunValidateTokens_OnlyRevokesWhatLichessCallsInvalid(t *testing.T) {
	// Absent from the answer is not the same as null (internal/lichess):
	// a token Lichess said nothing about is not revoked.
	tt := newTokenTest(t)
	player := tt.player(t, "player", "tok-player", nil)

	require.NoError(t, tt.run(t, unanswering{Fake: tt.fake, skip: "tok-player"}, "", 7021))

	detail := runDetail(t, jobRunByRiverID(t, tt.q, 7021))
	assert.Equal(t, 1, detail.Unanswered)
	assert.Equal(t, 0, detail.Revoked)
	assert.False(t, tt.token(t, player).RevokedAt.Valid)
}

func TestRunValidateTokens_RecordsTheOrganiserToken(t *testing.T) {
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	tests := []struct {
		name      string
		organiser tokencrypt.Secret
		info      *lichess.TokenInfo
		want      tokenhealth.Organiser
	}{
		{
			name: "not set",
			want: tokenhealth.Organiser{State: tokenhealth.OrganiserNotSet},
		},
		{
			name:      "valid, with its account and expiry",
			organiser: "tok-org",
			info:      &lichess.TokenInfo{UserID: "league", Scopes: []string{"challenge:bulk"}, Expires: expires},
			want:      tokenhealth.Organiser{State: tokenhealth.OrganiserValid, Account: "league", Expires: &expires},
		},
		{
			name:      "without challenge:bulk",
			organiser: "tok-org",
			info:      &lichess.TokenInfo{UserID: "league", Scopes: []string{"challenge:write"}},
			want:      tokenhealth.Organiser{State: tokenhealth.OrganiserMissingScope, Account: "league"},
		},
		{
			name:      "unknown to Lichess",
			organiser: "tok-org",
			want:      tokenhealth.Organiser{State: tokenhealth.OrganiserInvalid},
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tt := newTokenTest(t)
			if tc.info != nil {
				tt.fake.TokenInfo[tc.organiser] = *tc.info
			}
			riverJobID := int64(7031 + i)
			require.NoError(t, tt.run(t, tt.fake, tc.organiser, riverJobID))

			detail := runDetail(t, jobRunByRiverID(t, tt.q, riverJobID))
			require.NotNil(t, detail.Organiser)
			assert.Equal(t, tc.want.State, detail.Organiser.State)
			assert.Equal(t, tc.want.Account, detail.Organiser.Account)
			if tc.want.Expires == nil {
				assert.Nil(t, detail.Organiser.Expires)
			} else {
				require.NotNil(t, detail.Organiser.Expires)
				assert.True(t, tc.want.Expires.Equal(*detail.Organiser.Expires))
			}
		})
	}
}

func TestRunValidateTokens_LichessDownChangesNothing(t *testing.T) {
	tt := newTokenTest(t)
	player := tt.player(t, "player", "tok-player", nil)
	tt.fake.TestTokensErr = errors.New("lichess: 503")

	err := tt.run(t, tt.fake, "tok-org", 7041)
	require.Error(t, err, "river retries a failed run")

	run := jobRunByRiverID(t, tt.q, 7041)
	assert.Equal(t, gen.JobRunStatusFailed, run.Status)
	require.NotNil(t, run.Error)
	assert.Contains(t, *run.Error, "503")
	detail := runDetail(t, run)
	assert.Nil(t, detail.Organiser, "nothing learned about the organiser token")
	assert.False(t, tt.token(t, player).RevokedAt.Valid)
	assert.Empty(t, tt.notices(t, player))
}

func TestRunValidateTokens_UnreadableTokens(t *testing.T) {
	// A token that does not decrypt is never revoked: with the wrong
	// TOKEN_ENCRYPTION_KEY, every token would look unusable and the
	// whole league would be unpaired over a configuration mistake.
	unreadable := func(t *testing.T, tt tokenTest, name string) gen.User {
		t.Helper()
		user := createUser(t, tt.q, name)
		require.NoError(t, tt.q.UpsertOAuthToken(context.Background(), gen.UpsertOAuthTokenParams{
			UserID:      user.ID,
			AccessToken: []byte("sealed under another key"),
			Scopes:      []string{lichess.ScopeChallengeWrite},
		}))
		return user
	}

	t.Run("beside readable ones: named, and the run succeeds", func(t *testing.T) {
		tt := newTokenTest(t)
		tt.player(t, "readable", "tok-readable", []string{lichess.ScopeChallengeWrite})
		garbled := unreadable(t, tt, "garbled")

		require.NoError(t, tt.run(t, tt.fake, "", 7051))
		run := jobRunByRiverID(t, tt.q, 7051)
		assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
		require.NotNil(t, run.Error)
		assert.Contains(t, *run.Error, "garbled")
		assert.Equal(t, []string{"garbled"}, runDetail(t, run).Unreadable)
		assert.False(t, tt.token(t, garbled).RevokedAt.Valid)
	})

	t.Run("all of them: the run fails and names the key", func(t *testing.T) {
		tt := newTokenTest(t)
		first := unreadable(t, tt, "first")
		unreadable(t, tt, "second")

		require.Error(t, tt.run(t, tt.fake, "", 7052))
		run := jobRunByRiverID(t, tt.q, 7052)
		assert.Equal(t, gen.JobRunStatusFailed, run.Status)
		require.NotNil(t, run.Error)
		assert.Contains(t, *run.Error, "TOKEN_ENCRYPTION_KEY")
		assert.Empty(t, tt.fake.Calls, "Lichess is not asked")
		assert.False(t, tt.token(t, first).RevokedAt.Valid)
	})
}

func TestRunValidateTokens_NothingToTest(t *testing.T) {
	tt := newTokenTest(t)
	require.NoError(t, tt.run(t, tt.fake, "", 7061))
	run := jobRunByRiverID(t, tt.q, 7061)
	assert.Equal(t, gen.JobRunStatusSucceeded, run.Status)
	assert.Empty(t, tt.fake.Calls, "no call without a token to test")
	assert.Equal(t, tokenhealth.OrganiserNotSet, runDetail(t, run).Organiser.State)
}
