package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
	"github.com/nairwolf/4545-correspondence/internal/tokenhealth"
)

// validateTokensResult is what ends up in job_runs.detail. The
// organiser token's state is read back from here by /health and the
// admin banner (spec §8.5), so its key is part of that contract.
type validateTokensResult struct {
	// Tested counts the player tokens sent to Lichess.
	Tested int `json:"tested"`
	Valid  int `json:"valid"`
	// Revoked counts the tokens this run marked revoked, each of whose
	// players was told.
	Revoked int `json:"revoked"`
	// Unanswered counts tokens Lichess's answer did not mention. They
	// are left as they are: only a token Lichess calls invalid is
	// revoked.
	Unanswered int `json:"unanswered,omitempty"`
	// Unreadable names the players whose stored token could not be
	// decrypted.
	Unreadable []string `json:"unreadable,omitempty"`

	Organiser *tokenhealth.Organiser `json:"organiser_token,omitempty"`
}

// runValidateTokens implements the weekly validate-tokens job (spec §7):
// it tests every stored player token, and the organiser's, with
// POST /api/token/test, so that the pairing pool generated an hour
// later knows whom the site can create games for (spec §3.3, §5.7). It
// also backs `ic validate-tokens`.
//
// A token Lichess calls invalid, or one without challenge:write, is
// marked revoked and its player is told once, on-site. A token Lichess
// accepts has its check time and expiry refreshed. The organiser
// token's state goes in the run's detail.
//
// The run fails if the call to Lichess fails or a database write does
// (spec §7.2). A token that cannot be decrypted is left as it is and
// named in the run's error. The run fails only if no stored token could
// be decrypted at all: TOKEN_ENCRYPTION_KEY is then not the key the
// tokens were sealed with, and marking them all revoked would unpair
// the whole league over a configuration mistake.
//
// db is a pool in production; tests pass a transaction, whose Begin
// opens a savepoint, so the run rolls back with the test.
func runValidateTokens(
	ctx context.Context,
	db txBeginner,
	client lichess.API,
	key tokencrypt.Key,
	organiser tokencrypt.Secret,
	riverJobID *int64,
) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "validate-tokens", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("validate-tokens: create job run: %w", err)
	}

	result, jobErr := doValidateTokens(ctx, db, client, key, organiser)
	status, errMsg := validateTokensOutcome(result, jobErr)

	detail, _ := json.Marshal(result)
	// The outcome row is written under a context that cannot be
	// cancelled: the work may have ended *because* ctx was cancelled
	// (river's job timeout, or shutdown), and a run that leaves its row
	// at "running" is exactly the silent failure spec §7 rules out.
	if finishErr := q.FinishJobRun(context.WithoutCancel(ctx), gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: int32(result.Tested),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("validate-tokens: record job outcome", "error", finishErr)
	}

	if status == gen.JobRunStatusFailed {
		return fmt.Errorf("validate-tokens: %s", *errMsg)
	}
	slog.Info("validate-tokens complete",
		"tested", result.Tested,
		"valid", result.Valid,
		"revoked", result.Revoked,
		"organiser", organiserStateForLog(result.Organiser),
	)
	return nil
}

// errNoReadableToken fails a run whose every stored token failed to
// decrypt.
var errNoReadableToken = errors.New(
	"no stored token could be decrypted: is TOKEN_ENCRYPTION_KEY the key the tokens were sealed with?",
)

// validateTokensOutcome decides how a run is recorded (spec §7.2).
// Unreadable tokens alongside readable ones leave the run succeeded,
// with their players named in the error text, so /jobs shows them
// without /health turning red for one bad row.
func validateTokensOutcome(result validateTokensResult, fatal error) (gen.JobRunStatus, *string) {
	var problems []string
	status := gen.JobRunStatusSucceeded
	if fatal != nil {
		status = gen.JobRunStatusFailed
		problems = append(problems, fatal.Error())
	}
	if len(result.Unreadable) > 0 && !errors.Is(fatal, errNoReadableToken) {
		problems = append(problems, fmt.Sprintf(
			"%d stored tokens could not be decrypted and were left as they are: %s",
			len(result.Unreadable),
			strings.Join(result.Unreadable, ", "),
		))
	}
	if len(problems) == 0 {
		return status, nil
	}
	msg := strings.Join(problems, "; ")
	return status, &msg
}

// playerToken is one stored token read back and decrypted.
type playerToken struct {
	row    gen.ListTokensToValidateRow
	secret tokencrypt.Secret
}

func doValidateTokens(
	ctx context.Context,
	db txBeginner,
	client lichess.API,
	key tokencrypt.Key,
	organiser tokencrypt.Secret,
) (validateTokensResult, error) {
	var result validateTokensResult

	rows, err := gen.New(db).ListTokensToValidate(ctx)
	if err != nil {
		return result, fmt.Errorf("list tokens: %w", err)
	}
	var players []playerToken
	for _, row := range rows {
		secret, err := key.Open(row.AccessToken)
		if err != nil {
			result.Unreadable = append(result.Unreadable, row.LichessUsername)
			continue
		}
		players = append(players, playerToken{row: row, secret: secret})
	}
	if len(rows) > 0 && len(players) == 0 {
		return result, errNoReadableToken
	}

	tokens := make([]tokencrypt.Secret, 0, len(players)+1)
	for _, p := range players {
		tokens = append(tokens, p.secret)
	}
	if organiser != "" {
		tokens = append(tokens, organiser)
	}

	var answers map[tokencrypt.Secret]*lichess.TokenInfo
	if len(tokens) > 0 {
		answers, err = client.TestTokens(ctx, tokens)
		if err != nil {
			return result, fmt.Errorf("test tokens: %w", err)
		}
	}
	result.Tested = len(players)
	result.Organiser = organiserState(organiser, answers)

	for _, p := range players {
		info, answered := answers[p.secret]
		switch {
		case !answered:
			result.Unanswered++
		case info == nil || !info.HasScope(lichess.ScopeChallengeWrite):
			revoked, err := revokeToken(ctx, db, p.row)
			if err != nil {
				return result, err
			}
			if revoked {
				result.Revoked++
			}
		default:
			if _, err := gen.New(db).MarkTokenValidated(ctx, gen.MarkTokenValidatedParams{
				UserID:      p.row.UserID,
				AccessToken: p.row.AccessToken,
				ExpiresAt:   optionalTimestamp(info.Expires),
			}); err != nil {
				return result, fmt.Errorf("record %s's token as valid: %w", p.row.LichessUsername, err)
			}
			result.Valid++
		}
	}
	return result, nil
}

// revokeToken marks one token revoked and tells its player, in one
// transaction, so the notice exists if and only if the mark does. It
// reports false when the row had already changed: marked by an earlier
// run, or replaced by a sign-in since it was read.
func revokeToken(ctx context.Context, db txBeginner, row gen.ListTokensToValidateRow) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("revoke %s's token: begin: %w", row.LichessUsername, err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	marked, err := q.MarkTokenRevoked(ctx, gen.MarkTokenRevokedParams{
		UserID:      row.UserID,
		AccessToken: row.AccessToken,
	})
	if err != nil {
		return false, fmt.Errorf("revoke %s's token: %w", row.LichessUsername, err)
	}
	if marked == 0 {
		return false, nil
	}
	if err := notify.Send(ctx, q, tokenLapsedNotice(row)); err != nil {
		return false, fmt.Errorf("revoke %s's token: %w", row.LichessUsername, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("revoke %s's token: commit: %w", row.LichessUsername, err)
	}
	return true, nil
}

// tokenLapsedNotice is the token notification of spec §10. It is
// account-critical, so it reaches the player whatever their
// preferences, and keyed by the token's issue time, so a retried run
// never tells them twice about the same token. It is worded for both
// values of pairing.game_creation: the consequence in numbers (the
// grace period) is on the dashboard banner, which follows the setting.
func tokenLapsedNotice(row gen.ListTokensToValidateRow) notify.Notice {
	return notify.Notice{
		User:     row.UserID,
		Category: notify.Token,
		Title:    "Your Lichess authorisation has lapsed",
		Body: "Lichess no longer accepts the authorisation you gave the league, which it needs to create your games. " +
			"Re-authorise from your account page: it takes one click.",
		Link: "/account",
		Key:  "token:revoked:" + row.IssuedAt.Time.UTC().Format(time.RFC3339Nano),
	}
}

// organiserState words Lichess's answer about the organiser token.
func organiserState(
	organiser tokencrypt.Secret,
	answers map[tokencrypt.Secret]*lichess.TokenInfo,
) *tokenhealth.Organiser {
	if organiser == "" {
		return &tokenhealth.Organiser{State: tokenhealth.OrganiserNotSet}
	}
	info, answered := answers[organiser]
	switch {
	case !answered:
		return nil // nothing learned: /health keeps the previous state
	case info == nil:
		return &tokenhealth.Organiser{State: tokenhealth.OrganiserInvalid}
	}
	o := &tokenhealth.Organiser{State: tokenhealth.OrganiserValid, Account: info.UserID}
	if !info.HasScope(tokenhealth.ScopeChallengeBulk) {
		o.State = tokenhealth.OrganiserMissingScope
	}
	if !info.Expires.IsZero() {
		expires := info.Expires.UTC()
		o.Expires = &expires
	}
	return o
}

func organiserStateForLog(o *tokenhealth.Organiser) string {
	if o == nil {
		return "unanswered"
	}
	return string(o.State)
}

// optionalTimestamp is NULL for the zero time: a token that never
// expires.
func optionalTimestamp(t time.Time) pgtype.Timestamptz {
	if t.IsZero() {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: t, Valid: true}
}
