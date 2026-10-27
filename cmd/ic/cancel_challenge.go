package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
	"github.com/nairwolf/4545-correspondence/internal/tokenhealth"
)

// cancelChallengeResult is what ends up in job_runs.detail.
type cancelChallengeResult struct {
	Challenge  string `json:"challenge,omitempty"`
	Challenger string `json:"challenger,omitempty"`
	// Outcome is "withdrawn"; "gone" when Lichess no longer had the
	// challenge; or "skipped", with the reason.
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// errCannotWithdraw fails a run that has no working token to withdraw
// the challenge with. A retry would fail the same way.
var errCannotWithdraw = errors.New("cannot withdraw the challenge")

// runCancelChallenge implements the cancel-challenge job (spec §6.4):
// it withdraws, with its sender's token, the challenge of a pairing
// that will never be played — a missed start, or an admin's Mark
// failed — so a late acceptance cannot create a game no pairing
// counts. If the challenge was accepted meanwhile, Lichess aborts the
// game instead, as long as neither player has moved; that game would
// not have counted either.
//
// A challenge Lichess no longer has (404: declined, withdrawn already,
// or a game whose players have moved) needs nothing more. When the
// challenger's token no longer works, the run fails and is not retried:
// the challenge stays open on Lichess until it is answered. Any other
// failure is retried.
func runCancelChallenge(
	ctx context.Context,
	db gen.DBTX,
	client lichess.API,
	key tokencrypt.Key,
	pairingID pgtype.UUID,
	riverJobID *int64,
) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "cancel-challenge", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("cancel-challenge: create job run: %w", err)
	}

	result, jobErr := doCancelChallenge(ctx, q, client, key, pairingID)

	detail, _ := json.Marshal(result)
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	if jobErr != nil {
		status = gen.JobRunStatusFailed
		msg := jobErr.Error()
		errMsg = &msg
	}
	var withdrawn int32
	if result.Outcome == "withdrawn" {
		withdrawn = 1
	}
	if finishErr := q.FinishJobRun(context.WithoutCancel(ctx), gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: withdrawn,
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("cancel-challenge: record job outcome", "error", finishErr)
	}

	switch {
	case errors.Is(jobErr, errCannotWithdraw):
		return river.JobCancel(fmt.Errorf("cancel-challenge: %w", jobErr))
	case jobErr != nil:
		return fmt.Errorf("cancel-challenge: %w", jobErr)
	}
	slog.Info("cancel-challenge complete", "challenge", result.Challenge, "outcome", result.Outcome)
	return nil
}

func doCancelChallenge(
	ctx context.Context,
	q *gen.Queries,
	client lichess.API,
	key tokencrypt.Key,
	pairingID pgtype.UUID,
) (cancelChallengeResult, error) {
	var result cancelChallengeResult

	p, err := q.GetChallengeToWithdraw(ctx, pairingID)
	if errors.Is(err, pgx.ErrNoRows) {
		result.Outcome, result.Reason = "skipped", "not a challenge pairing"
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("get pairing %s: %w", pairingID.String(), err)
	}
	result.Challenger = p.ChallengerUsername
	if p.LichessGameID != nil {
		result.Challenge = *p.LichessGameID
	}
	if p.Status != gen.PairingStatusFailed || p.LichessGameID == nil {
		result.Outcome, result.Reason = "skipped", fmt.Sprintf("the pairing is %s, not failed", p.Status)
		return result, nil
	}

	tok, err := q.GetOAuthToken(ctx, p.ChallengerUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, fmt.Errorf("%w: %s has no stored token", errCannotWithdraw, p.ChallengerUsername)
	}
	if err != nil {
		return result, fmt.Errorf("get %s's token: %w", p.ChallengerUsername, err)
	}
	if !tokenhealth.PlayerOf(tok.IssuedAt, tok.RevokedAt, tok.ExpiresAt, time.Now()).Valid() {
		return result, fmt.Errorf("%w: %s's token no longer works", errCannotWithdraw, p.ChallengerUsername)
	}
	secret, err := key.Open(tok.AccessToken)
	if err != nil {
		return result, fmt.Errorf("%w: %s's token cannot be decrypted", errCannotWithdraw, p.ChallengerUsername)
	}

	err = client.CancelChallenge(ctx, secret, result.Challenge)
	var apiErr *lichess.APIError
	switch {
	case err == nil:
		result.Outcome = "withdrawn"
	case errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound:
		result.Outcome = "gone"
	default:
		return result, fmt.Errorf("withdraw challenge %s: %w", result.Challenge, err)
	}
	return result, nil
}
