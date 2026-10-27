// Package activity is evaluate-activity (spec §6.4), the first step of
// every generation. A pairing of a published round that never became
// a game is retired — marked failed — so it stops counting towards its
// players' games in flight. When the pairing was a challenge, it is
// also a missed start for the player who had to accept it, and the
// challenge's withdrawal on Lichess is queued; a run of missed starts
// warns that player, then pauses them. The package also sends the
// 48-hour "not started yet" reminder, for sync-games (Remind).
//
// What a run of missed starts leads to is a pure rule (streak.go).
// Evaluate asks Lichess one question, outside any transaction, and
// makes every change in one transaction after it.
package activity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// StartGrace is how long a published pairing has to become a game
// before it counts as unstarted (spec §6.4): its players are reminded
// then, and the first generation after retires it. A generation run
// off-cycle soon after a publication leaves that round's pairings be.
const StartGrace = 48 * time.Hour

// Scheduler queues the withdrawal of a missed challenge on Lichess, in
// the transaction that records the missed start (spec §6.3's "database
// first, Lichess after"). A rounds.Scheduler is one.
type Scheduler interface {
	ScheduleCancelChallenge(ctx context.Context, tx pgx.Tx, pairingID pgtype.UUID) error
}

// errNoScheduler fails an evaluation that would retire a challenge
// pairing with no job queue: the challenge would stay open on Lichess.
var errNoScheduler = errors.New(
	"no job queue to withdraw missed challenges on Lichess: retiring a challenge pairing needs one",
)

// Evaluator is what evaluate-activity needs from outside the database.
type Evaluator struct {
	// Lichess is asked whether the challenges about to be retired were
	// accepted since sync-games last looked.
	Lichess lichess.API
	// Scheduler queues each missed challenge's withdrawal.
	Scheduler Scheduler
}

// Report is what one evaluation did, for its job_runs detail.
type Report struct {
	// Skipped says why the evaluation changed nothing.
	Skipped string `json:"skipped,omitempty"`
	// Retired counts the pairings marked failed.
	Retired int `json:"retired"`
	// Accepted counts the challenges found accepted on Lichess, left for
	// sync-games to pick up as games.
	Accepted int `json:"accepted,omitempty"`
	// MissedStarts names each player who missed one, and the round.
	MissedStarts []string `json:"missed_starts,omitempty"`
	// Warned names the players now one missed start from a pause.
	Warned []string `json:"warned,omitempty"`
	// Paused names the players this evaluation auto-paused.
	Paused []string `json:"paused,omitempty"`
}

// Run is evaluate-activity as a job run of its own (spec §6.4, §7):
// every generation path — the scheduled job, the admin's Generate now,
// `ic generate-round` — calls it first, and `ic evaluate-activity`
// alone. It records its outcome in job_runs; a failed evaluation is
// visible there and on /health, and the caller generates anyway.
// riverJobID is the river job it runs in, if any.
func (e Evaluator) Run(ctx context.Context, db rounds.DB, riverJobID *int64) error {
	q := gen.New(db)

	jobRun, err := q.CreateJobRun(ctx, gen.CreateJobRunParams{JobName: "evaluate-activity", RiverJobID: riverJobID})
	if err != nil {
		return fmt.Errorf("evaluate-activity: create job run: %w", err)
	}

	var report Report
	cfg, jobErr := settings.Load(ctx, q)
	if jobErr != nil {
		jobErr = fmt.Errorf("load settings: %w", jobErr)
	} else {
		report, jobErr = e.Evaluate(ctx, db, cfg, time.Now())
	}

	detail, _ := json.Marshal(report)
	status := gen.JobRunStatusSucceeded
	var errMsg *string
	if jobErr != nil {
		status = gen.JobRunStatusFailed
		msg := jobErr.Error()
		errMsg = &msg
	}
	// Written under a context that cannot be cancelled, like every job's
	// outcome: a run left at "running" is the silent failure spec §7
	// rules out.
	if finishErr := q.FinishJobRun(context.WithoutCancel(ctx), gen.FinishJobRunParams{
		ID:             jobRun.ID,
		Status:         status,
		ItemsProcessed: int32(report.Retired),
		Error:          errMsg,
		Detail:         detail,
	}); finishErr != nil {
		slog.Error("evaluate-activity: record job outcome", "error", finishErr)
	}

	if jobErr != nil {
		return fmt.Errorf("evaluate-activity: %w", jobErr)
	}
	slog.Info("evaluate-activity complete",
		"retired", report.Retired,
		"missed_starts", len(report.MissedStarts),
		"warned", len(report.Warned),
		"paused", len(report.Paused),
	)
	return nil
}

// Evaluate retires the unstarted pairings and applies the missed-start
// rule (spec §6.4). cfg is the settings now; now is when the evaluation
// runs, which the 48-hour grace is measured from and which is recorded
// on each missed start and pause.
//
// Nothing changes while a draft is waiting for review: it was paired
// after the last evaluation, and a player paused now would still be in
// it. If Lichess cannot say whether the challenges were accepted, the
// evaluation fails and changes nothing either: one of them may be a
// game in progress.
func (e Evaluator) Evaluate(
	ctx context.Context,
	db rounds.DB,
	cfg settings.Settings,
	now time.Time,
) (Report, error) {
	var report Report
	q := gen.New(db)

	switch _, err := q.GetDraftRound(ctx); {
	case err == nil:
		report.Skipped = "a draft is waiting for review: it was paired after the last evaluation"
		return report, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return report, fmt.Errorf("look for a draft: %w", err)
	}

	unstarted, err := q.ListUnstartedPairings(ctx, timestamp(now.Add(-StartGrace)))
	if err != nil {
		return report, fmt.Errorf("list unstarted pairings: %w", err)
	}
	if len(unstarted) == 0 {
		return report, nil
	}
	accepted, err := e.accepted(ctx, unstarted)
	if err != nil {
		return report, err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return report, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once committed
	q = gen.New(tx)

	// Each player who missed a start, once, with their latest miss: a
	// player who missed two rounds' challenges (an evaluation failed in
	// between) is paused once, not paused and then told again.
	var missed []missedStart
	index := map[pgtype.UUID]int{}
	for _, p := range unstarted {
		challenge := isChallenge(p)
		if challenge && accepted[*p.LichessGameID] {
			report.Accepted++
			continue
		}
		n, err := q.RetireUnstartedPairing(ctx, gen.RetireUnstartedPairingParams{
			ID:            p.PairingID,
			LichessGameID: p.LichessGameID,
		})
		if err != nil {
			return report, fmt.Errorf("retire pairing %s: %w", p.PairingID.String(), err)
		}
		if n == 0 {
			continue // started, or marked failed, since it was listed
		}
		report.Retired++
		if !challenge {
			// Either player could have started it: nobody's missed start.
			continue
		}

		m := missedStartOf(p)
		if err := e.Scheduler.ScheduleCancelChallenge(ctx, tx, p.PairingID); err != nil {
			return report, fmt.Errorf("queue the withdrawal of %s's challenge: %w", m.challenger, err)
		}
		inserted, err := q.InsertMissedStart(ctx, gen.InsertMissedStartParams{
			UserID:     m.user,
			RoundID:    p.RoundID,
			PairingID:  p.PairingID,
			RecordedAt: timestamp(now),
		})
		if err != nil {
			return report, fmt.Errorf("record %s's missed start: %w", m.name, err)
		}
		if inserted == 0 {
			continue // the volunteer's other challenge of the round
		}
		report.MissedStarts = append(report.MissedStarts, fmt.Sprintf("%s (round %d)", m.name, m.round))
		if i, ok := index[m.user]; ok {
			missed[i] = m
		} else {
			index[m.user] = len(missed)
			missed = append(missed, m)
		}
	}

	for _, m := range missed {
		if err := consequences(ctx, q, cfg, m, now, &report); err != nil {
			return report, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return report, fmt.Errorf("commit: %w", err)
	}
	return report, nil
}

// accepted asks Lichess, in one call, which of the challenges about to
// be retired are games now: accepted since sync-games last looked.
// Those pairings are left pending, and sync-games finds their games by
// id within the hour. It also refuses to go on without a job queue to
// withdraw the others.
func (e Evaluator) accepted(
	ctx context.Context,
	unstarted []gen.ListUnstartedPairingsRow,
) (map[string]bool, error) {
	var ids []string
	for _, p := range unstarted {
		if isChallenge(p) {
			ids = append(ids, *p.LichessGameID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if e.Scheduler == nil {
		return nil, errNoScheduler
	}

	stream, err := e.Lichess.GamesByID(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("ask Lichess whether the challenges were accepted: %w", err)
	}
	defer stream.Close()
	games := map[string]bool{}
	for stream.Next() {
		games[stream.Game().ID] = true
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("ask Lichess whether the challenges were accepted: %w", err)
	}
	return games, nil
}

// consequences applies the missed-start rule to a player who has just
// missed one (spec §6.4): warned one miss before the threshold, paused
// at it. A pause also refreshes the player's standing, which carries
// their eligibility, as resuming does.
func consequences(
	ctx context.Context,
	q *gen.Queries,
	cfg settings.Settings,
	m missedStart,
	now time.Time,
	report *Report,
) error {
	run, err := Current(ctx, q, m.user)
	if err != nil {
		return fmt.Errorf("%s's missed starts: %w", m.name, err)
	}

	switch Decide(len(run), cfg.MissedStartsToPause) {
	case Warn:
		report.Warned = append(report.Warned, m.name)
		return notify.Send(ctx, q, warningNotice(m))
	case Pause:
		n, err := q.SetAutoPaused(ctx, gen.SetAutoPausedParams{UserID: m.user, AutoPausedAt: timestamp(now)})
		if err != nil {
			return fmt.Errorf("pause %s: %w", m.name, err)
		}
		if n == 0 {
			return nil // already paused
		}
		if _, err := standings.Recompute(ctx, q, m.user, cfg); err != nil {
			return fmt.Errorf("recompute %s's standing: %w", m.name, err)
		}
		report.Paused = append(report.Paused, m.name)
		return notify.Send(ctx, q, pausedNotice(m, len(run)))
	}
	return nil
}

// Current is a player's run of consecutive missed starts as it stands
// (Streak), oldest first; empty when there is none.
func Current(ctx context.Context, q *gen.Queries, user pgtype.UUID) ([]MissedStart, error) {
	rows, err := q.ListMissedStartsForUser(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("list missed starts: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	missed := make([]MissedStart, len(rows))
	for i, row := range rows {
		missed[i] = MissedStart{At: row.RecordedAt.Time, Round: row.RoundNumber}
	}

	var lastGame time.Time
	started, err := q.GetLatestGameStartForUser(ctx, user)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// never started a league game
	case err != nil:
		return nil, fmt.Errorf("latest game: %w", err)
	default:
		lastGame = started.Time
	}

	profile, err := q.GetPlayerProfile(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	var resumed time.Time
	if profile.ResumedAt.Valid {
		resumed = profile.ResumedAt.Time
	}
	return Streak(missed, lastGame, resumed), nil
}

// missedStart is a player who missed a start: who, whose challenge,
// and in which round.
type missedStart struct {
	user       pgtype.UUID
	name       string
	challenger string
	round      int32
}

// missedStartOf names the player of a challenge pairing who had to
// accept it: the one who did not send it.
func missedStartOf(p gen.ListUnstartedPairingsRow) missedStart {
	m := missedStart{user: p.WhiteUserID, name: p.WhiteUsername, challenger: p.BlackUsername, round: p.RoundNumber}
	if p.ChallengerUserID == p.WhiteUserID {
		m.user, m.name, m.challenger = p.BlackUserID, p.BlackUsername, p.WhiteUsername
	}
	return m
}

// isChallenge reports whether a pairing waits on a challenge, whose id
// is the game's once accepted. create-games records the two together.
func isChallenge(p gen.ListUnstartedPairingsRow) bool {
	return p.CreationMethod == gen.PairingMethodChallenge && p.LichessGameID != nil
}

func timestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
