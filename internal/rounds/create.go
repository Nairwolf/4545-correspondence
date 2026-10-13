package rounds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
	"github.com/nairwolf/4545-correspondence/internal/tokenhealth"
)

// Creating a published round's games on Lichess: the create-games job
// of spec §6.3. Publication under pairing.game_creation = lichess
// marks the round "creating" and queues the job in its transaction;
// the job then calls Lichess outside any transaction and records each
// result in a short transaction of its own, right after the call. It
// may run more than once for a round — river retries a failed attempt,
// and an admin can retry — so every step is safe to repeat: it starts
// from the pairings still waiting for a game, and adopts a bulk an
// earlier attempt created but never recorded rather than create the
// games twice.

// ErrNoOrganiserToken fails a run that has games to create without the
// organiser's token (spec §2.2).
var ErrNoOrganiserToken = errors.New(
	"LICHESS_ORG_TOKEN is not set: creating games needs the organiser's challenge:bulk token",
)

// Errors of RetryGameCreation, which the round page words for the admin.
var (
	ErrGameCreationOff  = errors.New("game creation is switched off (pairing.game_creation = manual)")
	ErrNotCreatingGames = errors.New("this round was not published for the league to create its games")
	ErrAlreadyQueued    = errors.New("a game-creation job for this round is already waiting or running")
)

// maxBulkSubmissions bounds one run's bulk requests (spec §6.3 step 5).
// Lichess refusing tokens that passed the probe moments earlier takes a
// second submission at most, in practice; a third is the margin.
const maxBulkSubmissions = 3

// clockSkew is how much earlier than the round's publication a bulk
// may seem to have been created, by Lichess's clock, and still be taken
// for one of this round's (reconciliation, spec §6.3 step 2).
const clockSkew = 5 * time.Minute

// leagueName starts the bulk's message. It is the name every page of
// the site carries (templates/layout.html); a second deployment for
// another community changes both together.
const leagueName = "Infinite Correspondence"

// GameCreator is what create-games needs from outside the database.
type GameCreator struct {
	Lichess lichess.API
	// Key opens the players' stored tokens (TOKEN_ENCRYPTION_KEY).
	Key tokencrypt.Key
	// Organiser is LICHESS_ORG_TOKEN, the league account's
	// challenge:bulk token.
	Organiser tokencrypt.Secret
	// Scheduler queues the sync-games run that follows a creation.
	Scheduler Scheduler
}

// DB is what CreateGames needs from the database: reads outside any
// transaction, and short transactions of its own, none of them held
// open across a Lichess call. A *pgxpool.Pool in production; in tests
// a pgx.Tx, whose Begin opens a savepoint.
type DB interface {
	gen.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// CreationReport is what one create-games run did, for its job_runs
// detail. Players are named by username, never by token.
type CreationReport struct {
	Round int32 `json:"round_number"`
	// State is the round's game creation when the run ended.
	State gen.GameCreationState `json:"state"`
	// Skipped says why the run did not try to create anything.
	Skipped string `json:"skipped,omitempty"`
	// Pending counts the pairings waiting for a game when the run began.
	Pending int `json:"pending"`
	// Adopted counts games found in a bulk an earlier attempt created
	// but never recorded.
	Adopted int `json:"adopted,omitempty"`
	// Created counts the games this run's bulk created.
	Created int `json:"created"`
	// ByHand counts the pairings left for their players to start.
	ByHand int `json:"by_hand"`
	// Bulks names the bulks this run created or adopted.
	Bulks []string `json:"bulks,omitempty"`
	// Submissions counts the bulk requests sent. More than one means
	// Lichess refused tokens the probe had passed.
	Submissions int `json:"submissions,omitempty"`
	// Lapsed names the players whose token was found not to work, and
	// was marked revoked.
	Lapsed []string `json:"lapsed_tokens,omitempty"`
	// Unreadable names the players whose stored token could not be
	// decrypted. Their games are started by hand; the token is kept.
	Unreadable []string `json:"unreadable_tokens,omitempty"`
	// Unrecorded lists games Lichess created that no pairing took: one
	// marked failed by an admin while the job ran.
	Unrecorded []string `json:"unrecorded_games,omitempty"`
}

// CreateGames runs create-games for one round (spec §6.3). cfg is the
// settings as they are now: switching pairing.game_creation back to
// manual stops a round still waiting for its games, which its players
// then start by hand. retry is an admin's retry, which changes only how
// the players of the games it creates are told. now is when the run
// started; a stored token expired by then is not used.
//
// Pairings whose two players both have a working token go into one
// bulk; the others are left to their players. The returned error fails
// the run and river retries it; the report says how far it got.
func (c GameCreator) CreateGames(
	ctx context.Context,
	db DB,
	roundID int32,
	cfg settings.Settings,
	retry bool,
	now time.Time,
) (CreationReport, error) {
	q := gen.New(db)

	round, err := q.GetRoundByID(ctx, roundID)
	if err != nil {
		return CreationReport{}, fmt.Errorf("get round: %w", err)
	}
	report := CreationReport{Round: round.Number, State: round.GameCreation}

	switch {
	case round.State != gen.RoundStatePublished || round.GameCreation != gen.GameCreationStateCreating:
		report.Skipped = "the round is not waiting for its games to be created"
		return report, nil
	case cfg.GameCreation != settings.GameCreationLichess:
		report.Skipped = "game creation is switched off (pairing.game_creation = manual)"
		round, err := giveUpInTx(ctx, db, roundID)
		report.State = round.GameCreation
		return report, err
	case c.Organiser == "":
		return report, ErrNoOrganiserToken
	}
	snap, ok := SnapshotOf(round)
	if !ok || snap.DaysPerMove == 0 {
		return report, fmt.Errorf("round %d has no settings snapshot to create its games from", round.Number)
	}

	pending, err := q.ListPairingsToCreate(ctx, round.ID)
	if err != nil {
		return report, fmt.Errorf("list pairings to create: %w", err)
	}
	report.Pending = len(pending)

	if len(pending) > 0 {
		if pending, err = c.adopt(ctx, db, round, snap, pending, retry, &report); err != nil {
			return report, err
		}
	}
	if len(pending) > 0 {
		working, err := c.probe(ctx, db, round.ID, pending, now, &report)
		if err != nil {
			return report, err
		}
		if bulk := bothWorking(pending, working); len(bulk) > 0 {
			if err := c.createBulk(ctx, db, round, snap, bulk, working, retry, &report); err != nil {
				return report, err
			}
		}
	}

	round, err = c.finish(ctx, db, round, snap, now, &report)
	if err != nil {
		return report, err
	}
	report.State = round.GameCreation
	return report, nil
}

// adopt is spec §6.3 step 2: a bulk created since the round was
// published, with the round's time control, may hold games for these
// pairings that an earlier attempt created but did not record — it
// crashed, or its context was cancelled, between the call and the
// commit. Those games are recorded now instead of created again. A game
// already attached to a pairing is someone else's. It returns the
// pairings still waiting.
func (c GameCreator) adopt(
	ctx context.Context,
	db DB,
	round gen.Round,
	snap Snapshot,
	pending []gen.ListPairingsToCreateRow,
	retry bool,
	report *CreationReport,
) ([]gen.ListPairingsToCreateRow, error) {
	bulks, err := c.Lichess.ListBulkPairings(ctx, c.Organiser)
	if err != nil {
		return nil, fmt.Errorf("list the organiser's bulks: %w", err)
	}
	attached, err := gen.New(db).ListAttachedGameIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list attached game ids: %w", err)
	}
	taken := make(map[string]bool, len(attached))
	for _, id := range attached {
		if id != nil {
			taken[*id] = true
		}
	}

	since := round.PublishedAt.Time.Add(-clockSkew)
	for _, bulk := range bulks {
		if bulk.ScheduledAtTime().Before(since) || !sameTimeControl(bulk, snap) {
			continue
		}
		var games []lichess.BulkGame
		for _, g := range bulk.Games {
			if !taken[g.ID] {
				games = append(games, g)
			}
		}
		bulk.Games = games
		n, err := c.record(ctx, db, round, snap, bulk, pending, retry, report)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			report.Adopted += n
			report.Bulks = append(report.Bulks, bulk.ID)
		}
	}

	return gen.New(db).ListPairingsToCreate(ctx, round.ID)
}

func sameTimeControl(bulk lichess.BulkPairing, snap Snapshot) bool {
	return bulk.Correspondence != nil &&
		bulk.Correspondence.DaysPerTurn == snap.DaysPerMove &&
		bulk.Rated == snap.Rated
}

// probe is spec §6.3 step 3: one POST /api/token/test over the tokens
// of the players still waiting for a game, since validate-tokens can be
// a day stale by publication and a bulk is all-or-nothing. It returns
// each player whose token works, with the token. A token works when it
// is stored, not revoked, not expired, can be decrypted, and Lichess
// does not refuse it; one Lichess calls invalid, or without
// challenge:write, is marked revoked and its player told. One Lichess's
// answer does not mention is kept: the bulk will say.
func (c GameCreator) probe(
	ctx context.Context,
	db DB,
	roundID int32,
	pending []gen.ListPairingsToCreateRow,
	now time.Time,
	report *CreationReport,
) (map[pgtype.UUID]workingToken, error) {
	names := playerNames(pending)
	rows, err := gen.New(db).ListRoundPlayerTokens(ctx, roundID)
	if err != nil {
		return nil, fmt.Errorf("list the players' tokens: %w", err)
	}

	var candidates []workingToken
	for _, row := range rows {
		if _, waiting := names[row.UserID]; !waiting {
			continue
		}
		if !tokenhealth.PlayerOf(row.IssuedAt, row.RevokedAt, row.ExpiresAt, now).Valid() {
			continue
		}
		secret, err := c.Key.Open(row.AccessToken)
		if err != nil {
			report.Unreadable = append(report.Unreadable, names[row.UserID])
			continue
		}
		candidates = append(candidates, workingToken{row: row, secret: secret})
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	secrets := make([]tokencrypt.Secret, 0, len(candidates))
	for _, cand := range candidates {
		secrets = append(secrets, cand.secret)
	}
	answers, err := c.Lichess.TestTokens(ctx, secrets)
	if err != nil {
		return nil, fmt.Errorf("test the players' tokens: %w", err)
	}

	working := make(map[pgtype.UUID]workingToken, len(candidates))
	for _, cand := range candidates {
		info, answered := answers[cand.secret]
		if answered && (info == nil || !info.HasScope(lichess.ScopeChallengeWrite)) {
			if err := revokeToken(ctx, db, cand.row); err != nil {
				return nil, fmt.Errorf("revoke %s's token: %w", names[cand.row.UserID], err)
			}
			report.Lapsed = append(report.Lapsed, names[cand.row.UserID])
			continue
		}
		working[cand.row.UserID] = cand
	}
	return working, nil
}

// workingToken is a player's stored token, as read, and the secret it
// opens to.
type workingToken struct {
	row    gen.OauthToken
	secret tokencrypt.Secret
}

// bothWorking is spec §3.3's partition, as far as this step goes: the
// pairings whose two players' tokens work go into the bulk.
func bothWorking(
	pending []gen.ListPairingsToCreateRow,
	working map[pgtype.UUID]workingToken,
) []gen.ListPairingsToCreateRow {
	var bulk []gen.ListPairingsToCreateRow
	for _, p := range pending {
		_, white := working[p.WhiteUserID]
		_, black := working[p.BlackUserID]
		if white && black {
			bulk = append(bulk, p)
		}
	}
	return bulk
}

// createBulk is spec §6.3 step 5: one request for every pairing whose
// players' tokens work, the double-game volunteer's two games included,
// with the round's time control and no pairAt, so the games start at
// once. A refusal naming tokens marks them revoked and resubmits
// without their pairings, at most maxBulkSubmissions times in all; any
// other refusal fails the run.
func (c GameCreator) createBulk(
	ctx context.Context,
	db DB,
	round gen.Round,
	snap Snapshot,
	pairings []gen.ListPairingsToCreateRow,
	working map[pgtype.UUID]workingToken,
	retry bool,
	report *CreationReport,
) error {
	for len(pairings) > 0 {
		req := lichess.BulkPairingRequest{
			Days:    snap.DaysPerMove,
			Rated:   snap.Rated,
			Message: bulkMessage(round),
		}
		for _, p := range pairings {
			req.Pairs = append(req.Pairs, lichess.BulkPair{
				White: working[p.WhiteUserID].secret,
				Black: working[p.BlackUserID].secret,
			})
		}

		report.Submissions++
		bulk, err := c.Lichess.CreateBulkPairing(ctx, c.Organiser, req)
		if err == nil {
			// Recorded even if the run's context is cancelled meanwhile:
			// the games exist now, and losing their record would leave
			// them for the next attempt to adopt at best.
			n, err := c.record(context.WithoutCancel(ctx), db, round, snap, bulk, pairings, retry, report)
			report.Created += n
			report.Bulks = append(report.Bulks, bulk.ID)
			return err
		}

		var rejection *lichess.BulkRejection
		if !errors.As(err, &rejection) || len(rejection.Tokens) == 0 {
			return fmt.Errorf("create the bulk: %w", err)
		}
		refused, err := revokeRefused(ctx, db, rejection, pairings, working, report)
		if err != nil {
			return err
		}
		if !refused || report.Submissions >= maxBulkSubmissions {
			return fmt.Errorf("create the bulk: %w", rejection)
		}
		pairings = bothWorking(pairings, working)
	}
	return nil
}

// revokeRefused marks revoked the tokens a bulk rejection names, and
// takes their players out of working. It reports whether the rejection
// named any token of this bulk: one that names none would only be
// refused again.
func revokeRefused(
	ctx context.Context,
	db DB,
	rejection *lichess.BulkRejection,
	pairings []gen.ListPairingsToCreateRow,
	working map[pgtype.UUID]workingToken,
	report *CreationReport,
) (bool, error) {
	refused := false
	for _, p := range pairings {
		players := [2]struct {
			id   pgtype.UUID
			name string
		}{{p.WhiteUserID, p.WhiteUsername}, {p.BlackUserID, p.BlackUsername}}
		for _, player := range players {
			tok, ok := working[player.id]
			if !ok {
				continue // refused already, in the volunteer's other game
			}
			if _, named := rejection.Tokens[tok.secret]; !named {
				continue
			}
			refused = true
			delete(working, player.id)
			if err := revokeToken(ctx, db, tok.row); err != nil {
				return false, fmt.Errorf("revoke %s's token: %w", player.name, err)
			}
			report.Lapsed = append(report.Lapsed, player.name)
		}
	}
	return refused, nil
}

// record stores the games of a bulk, in one transaction with their
// notices: the bulk's id on the round, and on each pairing it holds a
// game for, creation_method = bulk, status = created and the game id.
// A game is matched to its pairing by its two players and their
// colours, which are unique within a round. A pairing that is no
// longer pending — marked failed meanwhile — keeps its status, and the
// game is reported as unrecorded. It returns how many games it
// recorded.
func (c GameCreator) record(
	ctx context.Context,
	db DB,
	round gen.Round,
	snap Snapshot,
	bulk lichess.BulkPairing,
	pairings []gen.ListPairingsToCreateRow,
	retry bool,
	report *CreationReport,
) (int, error) {
	byPlayers := make(map[[2]string]gen.ListPairingsToCreateRow, len(pairings))
	for _, p := range pairings {
		byPlayers[[2]string{p.WhiteLichessID, p.BlackLichessID}] = p
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("record bulk %s: begin: %w", bulk.ID, err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	recorded := 0
	for _, g := range bulk.Games {
		p, ok := byPlayers[[2]string{g.White, g.Black}]
		if !ok {
			continue // a game of another round's bulk, or of another pairing of this one
		}
		gameID := g.ID
		n, err := q.RecordCreatedGame(ctx, gen.RecordCreatedGameParams{
			ID:             p.PairingID,
			CreationMethod: gen.PairingMethodBulk,
			Status:         gen.PairingStatusCreated,
			LichessGameID:  &gameID,
		})
		if err != nil {
			return 0, fmt.Errorf("record game %s: %w", g.ID, err)
		}
		if n == 0 {
			report.Unrecorded = append(report.Unrecorded, g.ID)
			continue
		}
		recorded++
		for _, s := range sidesOf(p.WhiteUserID, p.BlackUserID, p.WhiteUsername, p.BlackUsername) {
			if err := notify.Send(ctx, q, createdNotice(round, snap, p.PairingID, g.ID, s, retry)); err != nil {
				return 0, err
			}
		}
	}
	if recorded == 0 {
		return 0, nil // nothing of this bulk was ours: leave the round's bulk id alone
	}

	bulkID := bulk.ID
	if err := q.SetRoundBulkPairing(ctx, gen.SetRoundBulkPairingParams{ID: round.ID, BulkPairingID: &bulkID}); err != nil {
		return 0, fmt.Errorf("record bulk %s on round %d: %w", bulk.ID, round.Number, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("record bulk %s: commit: %w", bulk.ID, err)
	}
	return recorded, nil
}

// finish is spec §6.3 step 7: the round's games are created, the
// players of the pairings left are told to start theirs by hand, and a
// sync-games run is queued so the new games reach the site within
// minutes.
func (c GameCreator) finish(
	ctx context.Context,
	db DB,
	round gen.Round,
	snap Snapshot,
	now time.Time,
	report *CreationReport,
) (gen.Round, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return gen.Round{}, fmt.Errorf("finish: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	left, err := notifyByHand(ctx, q, round, snap)
	if err != nil {
		return gen.Round{}, err
	}
	report.ByHand = left

	finished, err := q.FinishGameCreation(ctx, gen.FinishGameCreationParams{
		ID:             round.ID,
		Outcome:        gen.GameCreationStateCreated,
		GamesCreatedAt: timestamp(now),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Given up or finished by another run meanwhile: its record stands.
		return q.GetRoundByID(ctx, round.ID)
	}
	if err != nil {
		return gen.Round{}, fmt.Errorf("mark round %d's games created: %w", round.Number, err)
	}

	if report.Created+report.Adopted > 0 && c.Scheduler != nil {
		if err := c.Scheduler.ScheduleSyncGames(ctx, tx); err != nil {
			return gen.Round{}, fmt.Errorf("queue sync-games: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return gen.Round{}, fmt.Errorf("finish: commit: %w", err)
	}
	return finished, nil
}

// GiveUp ends a round's game creation without the games still missing
// (spec §6.3): on create-games' last failed attempt, or when it finds
// game creation switched off. The round is marked gave_up and the
// players of every pairing still waiting are told to start their game
// by hand. A round no longer creating its games is left as it is.
func GiveUp(ctx context.Context, tx pgx.Tx, roundID int32) (gen.Round, error) {
	q := gen.New(tx)

	round, err := q.FinishGameCreation(ctx, gen.FinishGameCreationParams{
		ID:      roundID,
		Outcome: gen.GameCreationStateGaveUp,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return q.GetRoundByID(ctx, roundID)
	}
	if err != nil {
		return gen.Round{}, fmt.Errorf("mark game creation given up: %w", err)
	}
	snap, _ := SnapshotOf(round)
	if _, err := notifyByHand(ctx, q, round, snap); err != nil {
		return gen.Round{}, err
	}
	return round, nil
}

func giveUpInTx(ctx context.Context, db DB, roundID int32) (gen.Round, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return gen.Round{}, fmt.Errorf("give up: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	round, err := GiveUp(ctx, tx, roundID)
	if err != nil {
		return gen.Round{}, err
	}
	return round, tx.Commit(ctx)
}

// RetryGameCreation is an admin's retry (spec §8.5): a round published
// for the league to create its games — whether creation gave up, left
// pairings to be started by hand, or is stuck — goes back to
// "creating", and a create-games job is queued that first looks for
// games players have started by hand. It needs game creation switched
// on. ErrAlreadyQueued means a job for the round is already waiting or
// running; the caller rolls back, leaving the round as it was.
func RetryGameCreation(
	ctx context.Context,
	tx pgx.Tx,
	roundID int32,
	cfg settings.Settings,
	actor pgtype.UUID,
	sched Scheduler,
) (gen.Round, error) {
	if cfg.GameCreation != settings.GameCreationLichess {
		return gen.Round{}, ErrGameCreationOff
	}
	if sched == nil {
		return gen.Round{}, errNoScheduler
	}
	q := gen.New(tx)

	before, err := q.GetRoundByID(ctx, roundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Round{}, ErrNotCreatingGames
	}
	if err != nil {
		return gen.Round{}, fmt.Errorf("get round: %w", err)
	}
	round, err := q.ClaimGameCreation(ctx, roundID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.Round{}, ErrNotCreatingGames
	}
	if err != nil {
		return gen.Round{}, fmt.Errorf("claim game creation: %w", err)
	}

	queued, err := sched.ScheduleCreateGames(ctx, tx, round.ID, true)
	if err != nil {
		return gen.Round{}, fmt.Errorf("queue game creation of round %d: %w", round.Number, err)
	}
	if !queued {
		return gen.Round{}, ErrAlreadyQueued
	}

	err = auditRound(
		ctx,
		q,
		"round.retry_game_creation",
		actor,
		round,
		map[string]any{"game_creation": before.GameCreation},
		map[string]any{"game_creation": round.GameCreation},
	)
	if err != nil {
		return gen.Round{}, err
	}
	return round, nil
}

// notifyByHand tells the players of every pairing of the round still
// waiting for a game to start it themselves, and returns how many such
// pairings there are. Each pairing's notice is keyed by its "paired"
// event, so players already told how their game is made — at an
// earlier run, or before a retry — are not told again.
func notifyByHand(ctx context.Context, q *gen.Queries, round gen.Round, snap Snapshot) (int, error) {
	left, err := q.ListPairingsToCreate(ctx, round.ID)
	if err != nil {
		return 0, fmt.Errorf("list pairings left: %w", err)
	}
	for _, p := range left {
		for _, s := range sidesOf(p.WhiteUserID, p.BlackUserID, p.WhiteUsername, p.BlackUsername) {
			if err := notify.Send(ctx, q, byHandNotice(round, snap, p.PairingID, s, notCreated)); err != nil {
				return 0, err
			}
		}
	}
	return len(left), nil
}

// revokeToken marks a token Lichess refused as revoked and tells its
// player, in one transaction, so the notice exists if and only if the
// mark does. The update is guarded on the token's sealed bytes: a
// player who signed in since keeps their new token, and a token already
// marked is not announced twice. validate-tokens does the same with the
// same notice.
func revokeToken(ctx context.Context, db DB, row gen.OauthToken) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	marked, err := q.MarkTokenRevoked(ctx, gen.MarkTokenRevokedParams{
		UserID:      row.UserID,
		AccessToken: row.AccessToken,
	})
	if err != nil {
		return err
	}
	if marked == 0 {
		return nil
	}
	if err := notify.Send(ctx, q, notify.TokenLapsed(row.UserID, row.IssuedAt.Time)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// bulkMessage is the message Lichess sends each player from the
// organiser account when their game is created (spec §3.2, §10): it
// names the league and the round, and must contain {game}.
func bulkMessage(round gen.Round) string {
	return fmt.Sprintf("%s, round %d: your game with {opponent} is ready: {game}", leagueName, round.Number)
}

// playerNames maps each player of the pairings to their username, for
// the run's report and its errors.
func playerNames(pairings []gen.ListPairingsToCreateRow) map[pgtype.UUID]string {
	names := make(map[pgtype.UUID]string, 2*len(pairings))
	for _, p := range pairings {
		names[p.WhiteUserID] = p.WhiteUsername
		names[p.BlackUserID] = p.BlackUsername
	}
	return names
}
