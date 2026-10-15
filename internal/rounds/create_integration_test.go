//go:build integration

package rounds_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/notify"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

// creation is one create-games test's world: the database, the key the
// players' tokens are sealed with, the Fake that plays Lichess, and the
// settings with game creation switched on.
type creation struct {
	tx    pgx.Tx
	q     *gen.Queries
	key   tokencrypt.Key
	fake  *lichess.Fake
	sched *fakeScheduler
	cfg   settings.Settings
}

const organiser tokencrypt.Secret = "tok-organiser"

func newCreation(t *testing.T) creation {
	t.Helper()
	tx := testTx(t)
	key, err := tokencrypt.ParseKey(strings.Repeat("ab", 32))
	require.NoError(t, err)
	cfg := settings.Defaults()
	cfg.GameCreation = settings.GameCreationLichess
	return creation{
		tx:    tx,
		q:     gen.New(tx),
		key:   key,
		fake:  lichess.NewFake(),
		sched: &fakeScheduler{},
		cfg:   cfg,
	}
}

// player adds an approved player whose token works: stored, and known
// to Lichess with challenge:write.
func (c creation) player(t *testing.T, name string, rating int32) gen.User {
	t.Helper()
	user := addPlayer(t, c.tx, name, rating)
	c.storeToken(t, user, secretOf(name))
	c.fake.TokenInfo[secretOf(name)] = lichess.TokenInfo{
		UserID: name,
		Scopes: []string{lichess.ScopeChallengeWrite},
	}
	return user
}

func secretOf(name string) tokencrypt.Secret { return tokencrypt.Secret("tok-" + name) }

func (c creation) storeToken(t *testing.T, user gen.User, secret tokencrypt.Secret) {
	t.Helper()
	sealed, err := c.key.Seal(secret)
	require.NoError(t, err)
	require.NoError(t, c.q.UpsertOAuthToken(context.Background(), gen.UpsertOAuthTokenParams{
		UserID:      user.ID,
		AccessToken: sealed,
		Scopes:      []string{lichess.ScopeChallengeWrite},
	}))
}

// creatingRound writes a round as publication under lichess leaves it
// — published, its games being created — with exactly these pairings,
// so a test controls who plays whom. snap is the settings it was
// generated under.
func (c creation) creatingRound(t *testing.T, snap rounds.Snapshot, pairs ...[2]gen.User) (gen.Round, []gen.Pairing) {
	t.Helper()
	ctx := context.Background()
	number, err := c.q.NextRoundNumber(ctx)
	require.NoError(t, err)
	used, err := json.Marshal(snap)
	require.NoError(t, err)
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	round, err := c.q.CreateGeneratedRound(ctx, gen.CreateGeneratedRoundParams{
		Number:       number,
		State:        gen.RoundStatePublished,
		PublishAt:    now,
		PublishedAt:  now,
		PairAt:       now,
		GeneratedBy:  gen.RoundSourceSchedule,
		SettingsUsed: used,
	})
	require.NoError(t, err)

	var pairings []gen.Pairing
	for i, pair := range pairs {
		position := int32(i + 1)
		p, err := c.q.InsertGeneratedPairing(ctx, gen.InsertGeneratedPairingParams{
			RoundID:     round.ID,
			WhiteUserID: pair[0].ID,
			BlackUserID: pair[1].ID,
			Position:    &position,
		})
		require.NoError(t, err)
		pairings = append(pairings, p)
	}
	round, err = c.q.StartGameCreation(ctx, gen.StartGameCreationParams{
		ID:           round.ID,
		GameCreation: gen.GameCreationStateCreating,
	})
	require.NoError(t, err)
	return round, pairings
}

var twoDaysRated = rounds.Snapshot{DaysPerMove: 2, Rated: true}

func (c creation) creator(api lichess.API) rounds.GameCreator {
	return rounds.GameCreator{Lichess: api, Key: c.key, Organiser: organiser, Scheduler: c.sched}
}

func (c creation) run(t *testing.T, round gen.Round, retry bool) rounds.CreationReport {
	t.Helper()
	report, err := c.creator(c.fake).CreateGames(context.Background(), c.tx, round.ID, c.cfg, retry, time.Now())
	require.NoError(t, err)
	return report
}

func (c creation) round(t *testing.T, round gen.Round) gen.Round {
	t.Helper()
	got, err := c.q.GetRoundByID(context.Background(), round.ID)
	require.NoError(t, err)
	return got
}

func (c creation) pairing(t *testing.T, p gen.Pairing) gen.Pairing {
	t.Helper()
	got, err := c.q.GetPairingByID(context.Background(), gen.GetPairingByIDParams{ID: p.ID, RoundID: p.RoundID})
	require.NoError(t, err)
	return got
}

// calls lists the methods of the Fake's recorded calls, in order.
func (c creation) calls() []string {
	var methods []string
	for _, call := range c.fake.Calls {
		methods = append(methods, call.Method)
	}
	return methods
}

func noticeKeyed(t *testing.T, tx pgx.Tx, user gen.User, key string) (gen.Notification, int) {
	t.Helper()
	var found gen.Notification
	n := 0
	for _, notice := range noticesOf(t, tx, user) {
		if notice.DedupeKey != nil && *notice.DedupeKey == key {
			found = notice
			n++
		}
	}
	return found, n
}

func pairedKey(round gen.Round, p gen.Pairing) string {
	return "round:" + itoa(round.Number) + ":paired:" + p.ID.String()
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

// --- publication ---------------------------------------------------

func TestPublish_UnderLichessQueuesTheRoundsGameCreation(t *testing.T) {
	// Spec §6.3: publication records how the games will be created and,
	// when the site creates them, queues create-games in the same
	// transaction. The paired players hear when create-games knows how
	// their game was made, not before.
	c := newCreation(t)
	ctx := context.Background()
	alpha := c.player(t, "alpha", 2000)
	c.player(t, "bravo", 1900)

	draft := generate(t, c.tx, c.cfg, c.sched).Round
	round, published, err := rounds.Publish(ctx, c.tx, draft.ID, c.cfg, time.Now(), pgtype.UUID{}, c.sched)
	require.NoError(t, err)
	require.True(t, published)

	assert.Equal(t, gen.GameCreationStateCreating, round.GameCreation)
	assert.Equal(t, []scheduledCreation{{roundID: round.ID, retry: false}}, c.sched.create)
	assert.Empty(t, noticesOf(t, c.tx, alpha), "told once the game is made")
	assert.Empty(t, c.fake.Calls, "publication itself never calls Lichess")
}

func TestPublish_UnderManualRecordsItAndQueuesNothing(t *testing.T) {
	c := newCreation(t)
	ctx := context.Background()
	c.cfg.GameCreation = settings.GameCreationManual
	alpha := c.player(t, "alpha", 2000)
	c.player(t, "bravo", 1900)

	draft := generate(t, c.tx, c.cfg, c.sched).Round
	round, _, err := rounds.Publish(ctx, c.tx, draft.ID, c.cfg, time.Now(), pgtype.UUID{}, c.sched)
	require.NoError(t, err)

	assert.Equal(t, gen.GameCreationStateManual, round.GameCreation)
	assert.Empty(t, c.sched.create)
	assert.Len(t, noticesOf(t, c.tx, alpha), 1, "told at once to start the game by hand")
}

func TestPublish_UnderLichessNeedsAJobQueue(t *testing.T) {
	// Without one, nothing would ever create the games: the round is
	// not published at all.
	c := newCreation(t)
	c.player(t, "alpha", 2000)
	c.player(t, "bravo", 1900)
	draft := generate(t, c.tx, settings.Defaults(), nil).Round

	_, _, err := rounds.Publish(context.Background(), c.tx, draft.ID, c.cfg, time.Now(), pgtype.UUID{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "job queue")
}

func TestGenerate_AutoPublishUnderLichessQueuesTheGameCreation(t *testing.T) {
	c := newCreation(t)
	c.cfg.PairingMode = settings.PairingModeAutoPublish
	c.player(t, "alpha", 2000)
	c.player(t, "bravo", 1900)

	round := generate(t, c.tx, c.cfg, c.sched).Round
	assert.Equal(t, gen.RoundStatePublished, round.State)
	assert.Equal(t, gen.GameCreationStateCreating, round.GameCreation)
	assert.Equal(t, []scheduledCreation{{roundID: round.ID}}, c.sched.create)
}

func TestPublish_UnderLichessTheVolunteerIsToldTheGamesAreBeingCreated(t *testing.T) {
	c := newCreation(t)
	ctx := context.Background()
	c.player(t, "alpha", 2000)
	c.player(t, "bravo", 1900)
	c.player(t, "charlie", 1800)

	draft := generate(t, c.tx, c.cfg, c.sched).Round
	round, _, err := rounds.Publish(ctx, c.tx, draft.ID, c.cfg, time.Now(), pgtype.UUID{}, c.sched)
	require.NoError(t, err)
	doubles, err := c.q.ListDoubleGamesForRound(ctx, round.ID)
	require.NoError(t, err)
	require.Len(t, doubles, 1)

	notices := noticesOf(t, c.tx, gen.User{ID: doubles[0].UserID})
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Body, "you're playing 2 games")
	assert.Contains(t, notices[0].Body, "The league is creating them on Lichess")
}

// --- create-games -------------------------------------------------

func TestCreateGames_OneBulkWithTheRoundsTimeControl(t *testing.T) {
	c := newCreation(t)
	ctx := context.Background()
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	charlie := c.player(t, "charlie", 1800)
	delta := c.player(t, "delta", 1700)
	snap := rounds.Snapshot{DaysPerMove: 3, Rated: false}
	round, pairings := c.creatingRound(t, snap, [2]gen.User{alpha, bravo}, [2]gen.User{charlie, delta})

	// Today's settings differ from the round's: the round's win.
	c.cfg.DaysPerMove = 7
	c.cfg.Rated = true
	report := c.run(t, round, false)

	require.Len(t, c.fake.BulkRequests, 1)
	req := c.fake.BulkRequests[0]
	assert.Equal(t, 3, req.Days)
	assert.False(t, req.Rated)
	assert.Equal(t, []lichess.BulkPair{
		{White: secretOf("alpha"), Black: secretOf("bravo")},
		{White: secretOf("charlie"), Black: secretOf("delta")},
	}, req.Pairs)
	assert.Contains(t, req.Message, "round "+itoa(round.Number))
	assert.Contains(t, req.Message, "{game}")
	assert.Equal(t, []string{"ListBulkPairings", "TestTokens", "CreateBulkPairing"}, c.calls())
	assert.Equal(t, organiser, c.fake.Calls[2].Token, "the bulk is the organiser's")

	bulk := c.fake.Bulks[0]
	for i, p := range pairings {
		got := c.pairing(t, p)
		assert.Equal(t, gen.PairingMethodBulk, got.CreationMethod)
		assert.Equal(t, gen.PairingStatusCreated, got.Status)
		require.NotNil(t, got.LichessGameID)
		assert.Equal(t, bulk.Games[i].ID, *got.LichessGameID)
	}

	after := c.round(t, round)
	assert.Equal(t, gen.GameCreationStateCreated, after.GameCreation)
	assert.True(t, after.GamesCreatedAt.Valid)
	require.NotNil(t, after.BulkPairingID)
	assert.Equal(t, bulk.ID, *after.BulkPairingID)
	assert.Equal(t, 1, c.sched.syncs, "a sync-games run follows")

	assert.Equal(t, gen.GameCreationStateCreated, report.State)
	assert.Equal(t, 2, report.Pending)
	assert.Equal(t, 2, report.Created)
	assert.Zero(t, report.ByHand)
	assert.Equal(t, []string{bulk.ID}, report.Bulks)

	notice, n := noticeKeyed(t, c.tx, alpha, pairedKey(round, pairings[0]))
	require.Equal(t, 1, n)
	assert.Equal(t, "Round "+itoa(round.Number)+": your game against bravo is ready", notice.Title)
	assert.Contains(t, notice.Body, "a casual correspondence game at 3 days per move, you with white")
	require.NotNil(t, notice.LinkUrl)
	assert.Equal(t, "https://lichess.org/"+bulk.Games[0].ID, *notice.LinkUrl)
	_, n = noticeKeyed(t, c.tx, bravo, pairedKey(round, pairings[0]))
	assert.Equal(t, 1, n)

	// Every player has the game in flight from now, before sync-games
	// has fetched it (spec §5.8).
	inFlight, err := c.q.CountInFlightGamesForUser(ctx, alpha.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, inFlight)
}

func TestCreateGames_TheDoubleGameGoesInTheSameBulk(t *testing.T) {
	c := newCreation(t)
	vol := c.player(t, "volunteer", 2000)
	a := c.player(t, "alpha", 1900)
	b := c.player(t, "bravo", 1800)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{vol, a}, [2]gen.User{b, vol})

	c.run(t, round, false)

	require.Len(t, c.fake.BulkRequests, 1, "one bulk for both of the volunteer's games")
	assert.Equal(t, []lichess.BulkPair{
		{White: secretOf("volunteer"), Black: secretOf("alpha")},
		{White: secretOf("bravo"), Black: secretOf("volunteer")},
	}, c.fake.BulkRequests[0].Pairs)
	for _, p := range pairings {
		assert.Equal(t, gen.PairingStatusCreated, c.pairing(t, p).Status)
	}
}

func TestCreateGames_PartitionsThePairingsByToken(t *testing.T) {
	// Spec §3.3: both tokens working → the bulk; one → a challenge from
	// that player; neither → started by hand. A token works when it is
	// stored, not revoked, not expired and not refused by the probe. A
	// player who never authorised the league is never challenged.
	c := newCreation(t)
	ctx := context.Background()
	ok1 := c.player(t, "ok1", 2000)
	ok2 := c.player(t, "ok2", 1990)
	noToken := addPlayer(t, c.tx, "notoken", 1980)
	ok3 := c.player(t, "ok3", 1970)
	revoked := c.player(t, "revoked", 1960)
	exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, revoked.ID)
	ok4 := c.player(t, "ok4", 1950)
	expired := c.player(t, "expired", 1940)
	exec(t, c.tx, `UPDATE oauth_tokens SET expires_at = now() - interval '1 day' WHERE user_id = $1`, expired.ID)
	ok5 := c.player(t, "ok5", 1930)
	refused := c.player(t, "refused", 1920)
	delete(c.fake.TokenInfo, secretOf("refused")) // Lichess no longer knows it
	gone1 := c.player(t, "gone1", 1910)
	exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, gone1.ID)
	gone2 := c.player(t, "gone2", 1900)
	delete(c.fake.TokenInfo, secretOf("gone2"))

	round, pairings := c.creatingRound(t, twoDaysRated,
		[2]gen.User{ok1, ok2},     // bulk
		[2]gen.User{noToken, ok3}, // by hand: notoken never authorised
		[2]gen.User{ok4, revoked}, // ok4 challenges, with white
		[2]gen.User{expired, ok5}, // ok5 challenges, with black
		[2]gen.User{refused, ok1}, // ok1 challenges, with black
		[2]gen.User{gone1, gone2}, // by hand: neither works
	)
	report := c.run(t, round, false)

	require.Len(t, c.fake.BulkRequests, 1)
	assert.Equal(t, []lichess.BulkPair{{White: secretOf("ok1"), Black: secretOf("ok2")}}, c.fake.BulkRequests[0].Pairs)
	assert.Equal(t, gen.PairingStatusCreated, c.pairing(t, pairings[0]).Status)

	challengers := map[int]gen.User{2: ok4, 3: ok5, 4: ok1}
	require.Len(t, c.fake.Challenges, 3)
	for i, ch := range c.fake.Challenges {
		p := c.pairing(t, pairings[i+2])
		challenger := challengers[i+2]
		assert.Equal(t, gen.PairingMethodChallenge, p.CreationMethod)
		assert.Equal(t, gen.PairingStatusPending, p.Status, "until the opponent accepts")
		require.NotNil(t, p.LichessGameID)
		assert.Equal(t, ch.ID, *p.LichessGameID)
		assert.Equal(t, challenger.ID, p.ChallengerUserID)
	}
	var sent []string
	for _, ch := range c.fake.Challenges {
		sent = append(sent, ch.Challenger.ID+" to "+ch.DestUser.ID+" with "+ch.Color)
	}
	assert.Equal(t, []string{"ok4 to revoked with white", "ok5 to expired with black", "ok1 to refused with black"}, sent)
	for _, p := range []gen.Pairing{pairings[1], pairings[5]} {
		got := c.pairing(t, p)
		assert.Equal(t, gen.PairingMethodManualExternal, got.CreationMethod)
		assert.Equal(t, gen.PairingStatusPending, got.Status)
		assert.Nil(t, got.LichessGameID)
		assert.False(t, got.ChallengerUserID.Valid)
	}
	assert.Equal(t, 1, report.Created)
	assert.Equal(t, 3, report.Challenges)
	assert.Equal(t, 2, report.ByHand)
	assert.ElementsMatch(t, []string{"refused", "gone2"}, report.Lapsed)
	assert.Equal(t, gen.GameCreationStateCreated, c.round(t, round).GameCreation)

	// The token the probe found refused is marked revoked, and its
	// player told — the same notice validate-tokens sends.
	tok, err := c.q.GetOAuthToken(ctx, refused.ID)
	require.NoError(t, err)
	assert.True(t, tok.RevokedAt.Valid)
	_, n := noticeKeyed(t, c.tx, refused, "token:revoked:"+tok.IssuedAt.Time.UTC().Format(time.RFC3339Nano))
	assert.Equal(t, 1, n)

	// Both players of a pairing left by hand are told to start it.
	for _, user := range []gen.User{noToken, ok3, gone1, gone2} {
		p := pairings[1]
		if user.ID == gone1.ID || user.ID == gone2.ID {
			p = pairings[5]
		}
		notice, n := noticeKeyed(t, c.tx, user, pairedKey(round, p))
		require.Equal(t, 1, n, user.LichessUsername)
		assert.Contains(t, notice.Body, "The league couldn't create this game on Lichess, so please start it by hand")
		assert.Equal(t, notify.Round, notify.Category(notice.Category))
	}
}

func TestCreateGames_ARefusedTokenIsRevokedAndTheRestResubmitted(t *testing.T) {
	// Spec §6.3 step 5: a token that passed the probe and is refused by
	// the bulk moments later costs one more submission, not the round.
	// Its pairing moves to the challenge partition: the other player's
	// token still works.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	charlie := c.player(t, "charlie", 1800)
	delta := c.player(t, "delta", 1700)
	c.fake.RejectTokens[secretOf("delta")] = "No such token"
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo}, [2]gen.User{charlie, delta})

	report := c.run(t, round, false)

	assert.Equal(t, 2, report.Submissions)
	assert.Equal(t, []string{"delta"}, report.Lapsed)
	require.Len(t, c.fake.BulkRequests, 1, "only the second submission was accepted")
	assert.Equal(t, []lichess.BulkPair{{White: secretOf("alpha"), Black: secretOf("bravo")}}, c.fake.BulkRequests[0].Pairs)
	assert.Equal(t, gen.PairingStatusCreated, c.pairing(t, pairings[0]).Status)
	challenged := c.pairing(t, pairings[1])
	assert.Equal(t, gen.PairingMethodChallenge, challenged.CreationMethod)
	assert.Equal(t, charlie.ID, challenged.ChallengerUserID)
	assert.Equal(t, gen.PairingStatusPending, challenged.Status)
	assert.Equal(t, 1, report.Challenges)

	tok, err := c.q.GetOAuthToken(context.Background(), delta.ID)
	require.NoError(t, err)
	assert.True(t, tok.RevokedAt.Valid)
}

func TestCreateGames_AChallengeHasTheRoundsTimeControlAndTellsBothPlayers(t *testing.T) {
	// Spec §6.3 step 6: one call with the challenger's token, in their
	// colour, with the round's days and rated — the settings it was
	// paired under, not today's. The pairing keeps the challenge's id as
	// its game id and stays pending until the opponent accepts. Both
	// players are told, the opponent in the challenge category.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, alpha.ID)
	round, pairings := c.creatingRound(t, rounds.Snapshot{DaysPerMove: 3, Rated: false}, [2]gen.User{alpha, bravo})

	report := c.run(t, round, false)

	assert.Equal(t, []string{"ListBulkPairings", "TestTokens", "CreateChallenge"}, c.calls())
	assert.Equal(t, secretOf("bravo"), c.fake.Calls[2].Token, "sent from bravo's account")
	require.Len(t, c.fake.Challenges, 1)
	ch := c.fake.Challenges[0]
	assert.Equal(t, "alpha", ch.DestUser.Name)
	assert.Equal(t, "black", ch.Color)
	assert.Equal(t, 3, ch.TimeControl.DaysPerTurn)
	assert.False(t, ch.Rated)

	p := c.pairing(t, pairings[0])
	assert.Equal(t, gen.PairingMethodChallenge, p.CreationMethod)
	assert.Equal(t, gen.PairingStatusPending, p.Status)
	require.NotNil(t, p.LichessGameID)
	assert.Equal(t, ch.ID, *p.LichessGameID)
	assert.Equal(t, bravo.ID, p.ChallengerUserID)

	assert.Equal(t, 1, report.Challenges)
	assert.Zero(t, report.Created)
	assert.Zero(t, report.ByHand)
	assert.Equal(t, gen.GameCreationStateCreated, c.round(t, round).GameCreation)
	assert.Zero(t, c.sched.syncs, "nothing to ingest until the challenge is accepted")

	notice, n := noticeKeyed(t, c.tx, bravo, pairedKey(round, pairings[0]))
	require.Equal(t, 1, n)
	assert.Equal(t, notify.Round, notify.Category(notice.Category))
	assert.Equal(t, "Round "+itoa(round.Number)+": you play alpha with black", notice.Title)
	assert.Contains(t, notice.Body,
		"it has challenged alpha from your Lichess account instead: a casual correspondence game at 3 days per move, you with black")
	require.NotNil(t, notice.LinkUrl)
	assert.Equal(t, "https://lichess.org/"+ch.ID, *notice.LinkUrl)

	notice, n = noticeKeyed(t, c.tx, alpha, pairedKey(round, pairings[0]))
	require.Equal(t, 1, n)
	assert.Equal(t, notify.Challenge, notify.Category(notice.Category))
	assert.Equal(t, "Round "+itoa(round.Number)+": accept bravo's challenge", notice.Title)
	assert.Contains(t, notice.Body,
		"a challenge from their account instead: a casual correspondence game at 3 days per move, you with white. Accept it on Lichess")
	require.NotNil(t, notice.LinkUrl)
	assert.Equal(t, "https://lichess.org/"+ch.ID, *notice.LinkUrl)
}

func TestCreateGames_ARefusedChallengeIsLeftByHandAndTheOthersAreSent(t *testing.T) {
	// Spec §6.3 step 6: a challenge Lichess refuses — the opponent takes
	// no challenges, say — leaves that pairing to its players, and the
	// run goes on.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	charlie := c.player(t, "charlie", 1800)
	delta := c.player(t, "delta", 1700)
	exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id IN ($1, $2)`, bravo.ID, delta.ID)
	c.fake.ChallengeErrFor["bravo"] = &lichess.APIError{StatusCode: 400, Message: "bravo does not accept challenges"}
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo}, [2]gen.User{charlie, delta})

	report := c.run(t, round, false)

	byHand := c.pairing(t, pairings[0])
	assert.Equal(t, gen.PairingMethodManualExternal, byHand.CreationMethod)
	assert.Nil(t, byHand.LichessGameID)
	assert.Equal(t, gen.PairingMethodChallenge, c.pairing(t, pairings[1]).CreationMethod)
	assert.Equal(t, 1, report.Challenges)
	assert.Equal(t, 1, report.ByHand)
	assert.Equal(t, []string{"alpha to bravo: lichess: 400: bravo does not accept challenges"}, report.Unchallenged)
	assert.Equal(t, gen.GameCreationStateCreated, c.round(t, round).GameCreation)

	for _, user := range []gen.User{alpha, bravo} {
		notice, n := noticeKeyed(t, c.tx, user, pairedKey(round, pairings[0]))
		require.Equal(t, 1, n, user.LichessUsername)
		assert.Contains(t, notice.Body, "please start it by hand")
	}
}

func TestCreateGames_RunningAgainSendsNoSecondChallenge(t *testing.T) {
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, bravo.ID)
	round, _ := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})

	c.run(t, round, false)
	exec(t, c.tx, `UPDATE rounds SET game_creation = 'creating' WHERE id = $1`, round.ID)
	report := c.run(t, round, false)

	assert.Len(t, c.fake.Challenges, 1)
	assert.Zero(t, report.Pending, "a challenged pairing is not waiting for a game any more")
	assert.Zero(t, report.Challenges)
}

// cancellingAtChallenge stands for the run's context being cancelled —
// river's timeout, or a shutdown — while a challenge call is under way.
type cancellingAtChallenge struct {
	*lichess.Fake
	cancel context.CancelFunc
}

func (c cancellingAtChallenge) CreateChallenge(
	ctx context.Context,
	challenger tokencrypt.Secret,
	opponent string,
	req lichess.ChallengeRequest,
) (lichess.Challenge, error) {
	c.cancel()
	return lichess.Challenge{}, ctx.Err()
}

func TestCreateGames_ACancelledRunStopsAtTheChallenges(t *testing.T) {
	// A challenge that failed because the run was cancelled was not
	// refused: the run fails, river retries it, and nobody is told to
	// start the game by hand meanwhile.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, bravo.ID)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := cancellingAtChallenge{Fake: c.fake, cancel: cancel}

	report, err := c.creator(api).CreateGames(ctx, c.tx, round.ID, c.cfg, false, time.Now())
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, report.Unchallenged)
	assert.Equal(t, gen.GameCreationStateCreating, c.round(t, round).GameCreation)
	p := c.pairing(t, pairings[0])
	assert.Equal(t, gen.PairingMethodManualExternal, p.CreationMethod)
	assert.Nil(t, p.LichessGameID)
	assert.Empty(t, noticesOf(t, c.tx, alpha))
}

// refusingOneTokenAtATime refuses, at each bulk, the first token of it
// Lichess has not refused yet: tokens lapsing one by one, faster than
// the run can resubmit.
type refusingOneTokenAtATime struct {
	*lichess.Fake
	attempts int
}

func (r *refusingOneTokenAtATime) CreateBulkPairing(
	ctx context.Context,
	organiser tokencrypt.Secret,
	req lichess.BulkPairingRequest,
) (lichess.BulkPairing, error) {
	r.attempts++
	return lichess.BulkPairing{}, &lichess.BulkRejection{
		Tokens: map[tokencrypt.Secret]string{req.Pairs[0].White: "No such token"},
	}
}

func TestCreateGames_StopsAfterThreeSubmissions(t *testing.T) {
	c := newCreation(t)
	var users []gen.User
	for _, name := range []string{"a1", "a2", "b1", "b2", "c1", "c2", "d1", "d2"} {
		users = append(users, c.player(t, name, 1500))
	}
	round, _ := c.creatingRound(t, twoDaysRated,
		[2]gen.User{users[0], users[1]},
		[2]gen.User{users[2], users[3]},
		[2]gen.User{users[4], users[5]},
		[2]gen.User{users[6], users[7]},
	)
	api := &refusingOneTokenAtATime{Fake: c.fake}

	report, err := c.creator(api).CreateGames(context.Background(), c.tx, round.ID, c.cfg, false, time.Now())
	var rejection *lichess.BulkRejection
	require.ErrorAs(t, err, &rejection)
	assert.Equal(t, 3, api.attempts)
	assert.Equal(t, 3, report.Submissions)
	assert.Equal(t, []string{"a1", "b1", "c1"}, report.Lapsed, "each refused token is revoked, the third too")
	assert.Equal(t, gen.GameCreationStateCreating, c.round(t, round).GameCreation, "river retries the run")
}

func TestCreateGames_AnyOtherRefusalFailsTheRun(t *testing.T) {
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
	c.fake.CreateBulkErr = &lichess.BulkRejection{Message: "Too many bulks queued"}

	_, err := c.creator(c.fake).CreateGames(context.Background(), c.tx, round.ID, c.cfg, false, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Too many bulks queued")
	assert.Equal(t, gen.GameCreationStateCreating, c.round(t, round).GameCreation)
	assert.Equal(t, gen.PairingStatusPending, c.pairing(t, pairings[0]).Status)
	assert.Empty(t, noticesOf(t, c.tx, alpha), "nobody is told anything until the games exist or creation gives up")
}

func TestCreateGames_RunningTwiceCreatesOneBulk(t *testing.T) {
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})

	c.run(t, round, false)
	report := c.run(t, round, false)
	assert.NotEmpty(t, report.Skipped, "the round is no longer waiting for its games")

	// Even back in "creating", as a retry leaves it, there is nothing
	// left to create.
	_, err := c.q.ClaimGameCreation(context.Background(), round.ID)
	require.NoError(t, err)
	report = c.run(t, round, true)
	assert.Zero(t, report.Pending)
	assert.Equal(t, gen.GameCreationStateCreated, report.State)

	assert.Len(t, c.fake.Bulks, 1)
	_, n := noticeKeyed(t, c.tx, alpha, pairedKey(round, pairings[0]))
	assert.Equal(t, 1, n, "told once")
}

// crashingAfterTheBulk creates the bulk on Lichess and then fails, as
// a process that died before it could record the result.
type crashingAfterTheBulk struct{ *lichess.Fake }

func (c crashingAfterTheBulk) CreateBulkPairing(
	ctx context.Context,
	organiser tokencrypt.Secret,
	req lichess.BulkPairingRequest,
) (lichess.BulkPairing, error) {
	if _, err := c.Fake.CreateBulkPairing(ctx, organiser, req); err != nil {
		return lichess.BulkPairing{}, err
	}
	return lichess.BulkPairing{}, errors.New("connection reset")
}

func TestCreateGames_AdoptsABulkAnEarlierAttemptNeverRecorded(t *testing.T) {
	// Spec §6.3 step 2: a retried run lists the organiser's bulks and
	// records the games it finds for its pairings, rather than create
	// them a second time.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	charlie := c.player(t, "charlie", 1800)
	delta := c.player(t, "delta", 1700)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo}, [2]gen.User{charlie, delta})

	_, err := c.creator(crashingAfterTheBulk{c.fake}).CreateGames(
		context.Background(),
		c.tx,
		round.ID,
		c.cfg,
		false,
		time.Now(),
	)
	require.Error(t, err)
	require.Len(t, c.fake.Bulks, 1, "the games exist on Lichess")
	assert.Equal(t, gen.PairingStatusPending, c.pairing(t, pairings[0]).Status, "but not in our record")

	report := c.run(t, round, false)
	assert.Len(t, c.fake.Bulks, 1, "no second bulk")
	assert.Equal(t, 2, report.Adopted)
	assert.Zero(t, report.Created)
	for i, p := range pairings {
		got := c.pairing(t, p)
		assert.Equal(t, gen.PairingStatusCreated, got.Status)
		require.NotNil(t, got.LichessGameID)
		assert.Equal(t, c.fake.Bulks[0].Games[i].ID, *got.LichessGameID)
	}
	assert.Equal(t, 1, c.sched.syncs)
	_, n := noticeKeyed(t, c.tx, delta, pairedKey(round, pairings[1]))
	assert.Equal(t, 1, n)
}

func TestCreateGames_NeverAdoptsAnotherRoundsGames(t *testing.T) {
	// A bulk from before this round's publication, one with another time
	// control, or a game already attached elsewhere is not this round's.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
	old := time.Now().Add(-time.Hour).UnixMilli()
	c.fake.Bulks = []lichess.BulkPairing{
		{
			ID: "old", ScheduledAt: old, Rated: true,
			Correspondence: &lichess.CorrespondenceTimeControl{DaysPerTurn: 2},
			Games:          []lichess.BulkGame{{ID: "oldgame", White: "alpha", Black: "bravo"}},
		},
		{
			ID: "slower", ScheduledAt: time.Now().UnixMilli(), Rated: true,
			Correspondence: &lichess.CorrespondenceTimeControl{DaysPerTurn: 7},
			Games:          []lichess.BulkGame{{ID: "slowgame", White: "alpha", Black: "bravo"}},
		},
	}

	report := c.run(t, round, false)
	assert.Zero(t, report.Adopted)
	assert.Equal(t, 1, report.Created)
	got := c.pairing(t, pairings[0])
	require.NotNil(t, got.LichessGameID)
	assert.NotContains(t, []string{"oldgame", "slowgame"}, *got.LichessGameID)
}

// markingFailedMidCall stands for an admin marking a pairing failed
// while the bulk call, or a challenge call, is under way.
type markingFailedMidCall struct {
	*lichess.Fake
	t       *testing.T
	tx      pgx.Tx
	pairing gen.Pairing
}

func (m markingFailedMidCall) CreateBulkPairing(
	ctx context.Context,
	organiser tokencrypt.Secret,
	req lichess.BulkPairingRequest,
) (lichess.BulkPairing, error) {
	_, err := rounds.MarkFailed(ctx, m.tx, m.pairing.RoundID, m.pairing.ID, pgtype.UUID{})
	require.NoError(m.t, err)
	return m.Fake.CreateBulkPairing(ctx, organiser, req)
}

func (m markingFailedMidCall) CreateChallenge(
	ctx context.Context,
	challenger tokencrypt.Secret,
	opponent string,
	req lichess.ChallengeRequest,
) (lichess.Challenge, error) {
	_, err := rounds.MarkFailed(ctx, m.tx, m.pairing.RoundID, m.pairing.ID, pgtype.UUID{})
	require.NoError(m.t, err)
	return m.Fake.CreateChallenge(ctx, challenger, opponent, req)
}

func TestCreateGames_AnAdminsMarkFailedMeanwhileWins(t *testing.T) {
	// Spec §6.3: a pairing's status only moves with a guarded update.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	charlie := c.player(t, "charlie", 1800)
	delta := c.player(t, "delta", 1700)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo}, [2]gen.User{charlie, delta})
	api := markingFailedMidCall{Fake: c.fake, t: t, tx: c.tx, pairing: pairings[1]}

	report, err := c.creator(api).CreateGames(context.Background(), c.tx, round.ID, c.cfg, false, time.Now())
	require.NoError(t, err)

	failed := c.pairing(t, pairings[1])
	assert.Equal(t, gen.PairingStatusFailed, failed.Status)
	assert.Nil(t, failed.LichessGameID)
	assert.Equal(t, []string{c.fake.Bulks[0].Games[1].ID}, report.Unrecorded)
	assert.Equal(t, 1, report.Created)
	assert.Empty(t, noticesOf(t, c.tx, delta))
}

func TestCreateGames_AnAdminsMarkFailedDuringAChallengeWins(t *testing.T) {
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, bravo.ID)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
	api := markingFailedMidCall{Fake: c.fake, t: t, tx: c.tx, pairing: pairings[0]}

	report, err := c.creator(api).CreateGames(context.Background(), c.tx, round.ID, c.cfg, false, time.Now())
	require.NoError(t, err)

	failed := c.pairing(t, pairings[0])
	assert.Equal(t, gen.PairingStatusFailed, failed.Status)
	assert.Equal(t, gen.PairingMethodManualExternal, failed.CreationMethod)
	assert.Nil(t, failed.LichessGameID)
	assert.Equal(t, []string{c.fake.Challenges[0].ID}, report.Unrecorded)
	assert.Zero(t, report.Challenges)
	assert.Empty(t, noticesOf(t, c.tx, alpha))
	assert.Empty(t, noticesOf(t, c.tx, bravo))
}

func TestCreateGames_SwitchedOffGivesTheGamesUpToThePlayers(t *testing.T) {
	// Decision 1: setting pairing.game_creation back to manual is how an
	// admin stops automation. A round still waiting for its games is
	// given up at once, without a call to Lichess.
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
	c.cfg.GameCreation = settings.GameCreationManual

	report := c.run(t, round, false)
	assert.NotEmpty(t, report.Skipped)
	assert.Equal(t, gen.GameCreationStateGaveUp, report.State)
	assert.Empty(t, c.fake.Calls)
	assert.Equal(t, gen.GameCreationStateGaveUp, c.round(t, round).GameCreation)
	_, n := noticeKeyed(t, c.tx, bravo, pairedKey(round, pairings[0]))
	assert.Equal(t, 1, n, "told to start it by hand")
}

func TestCreateGames_WithoutTheOrganiserTokenFailsNamingIt(t *testing.T) {
	c := newCreation(t)
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	round, _ := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
	creator := c.creator(c.fake)
	creator.Organiser = ""

	_, err := creator.CreateGames(context.Background(), c.tx, round.ID, c.cfg, false, time.Now())
	require.ErrorIs(t, err, rounds.ErrNoOrganiserToken)
	assert.Contains(t, err.Error(), "LICHESS_ORG_TOKEN")
	assert.Empty(t, c.fake.Calls)
}

func TestGiveUp_TellsBothPlayersToStartByHandOnce(t *testing.T) {
	c := newCreation(t)
	ctx := context.Background()
	alpha := c.player(t, "alpha", 2000)
	bravo := c.player(t, "bravo", 1900)
	round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})

	given, err := rounds.GiveUp(ctx, c.tx, round.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.GameCreationStateGaveUp, given.GameCreation)
	again, err := rounds.GiveUp(ctx, c.tx, round.ID)
	require.NoError(t, err)
	assert.Equal(t, gen.GameCreationStateGaveUp, again.GameCreation)

	for _, user := range []gen.User{alpha, bravo} {
		notice, n := noticeKeyed(t, c.tx, user, pairedKey(round, pairings[0]))
		require.Equal(t, 1, n)
		assert.Contains(t, notice.Body, "start it by hand as a rated correspondence game at 2 days per move")
	}
	assert.Equal(t, gen.PairingStatusPending, c.pairing(t, pairings[0]).Status, "still theirs to start")
}

func TestRetryGameCreation(t *testing.T) {
	ctx := context.Background()

	t.Run("queues a retry and tells the players their game now exists", func(t *testing.T) {
		c := newCreation(t)
		alpha := c.player(t, "alpha", 2000)
		bravo := c.player(t, "bravo", 1900)
		round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
		_, err := rounds.GiveUp(ctx, c.tx, round.ID)
		require.NoError(t, err)

		claimed, err := rounds.RetryGameCreation(ctx, c.tx, round.ID, c.cfg, pgtype.UUID{}, c.sched)
		require.NoError(t, err)
		assert.Equal(t, gen.GameCreationStateCreating, claimed.GameCreation)
		assert.Equal(t, []scheduledCreation{{roundID: round.ID, retry: true}}, c.sched.create)

		c.run(t, round, true)
		assert.Equal(t, gen.PairingStatusCreated, c.pairing(t, pairings[0]).Status)
		notice, n := noticeKeyed(t, c.tx, alpha, "round:"+itoa(round.Number)+":created:"+pairings[0].ID.String())
		require.Equal(t, 1, n)
		assert.Contains(t, notice.Title, "has now been created")
		assert.Contains(t, notice.Body, "Don't start another one")
		_, n = noticeKeyed(t, c.tx, alpha, pairedKey(round, pairings[0]))
		assert.Equal(t, 1, n, "the by-hand notice is not repeated")
	})

	t.Run("a retry's challenge tells both players under its own key", func(t *testing.T) {
		c := newCreation(t)
		alpha := c.player(t, "alpha", 2000)
		bravo := c.player(t, "bravo", 1900)
		exec(t, c.tx, `UPDATE oauth_tokens SET revoked_at = now() WHERE user_id = $1`, alpha.ID)
		round, pairings := c.creatingRound(t, twoDaysRated, [2]gen.User{alpha, bravo})
		_, err := rounds.GiveUp(ctx, c.tx, round.ID)
		require.NoError(t, err)
		_, err = rounds.RetryGameCreation(ctx, c.tx, round.ID, c.cfg, pgtype.UUID{}, c.sched)
		require.NoError(t, err)

		c.run(t, round, true)
		assert.Equal(t, gen.PairingMethodChallenge, c.pairing(t, pairings[0]).CreationMethod)
		created := "round:" + itoa(round.Number) + ":created:" + pairings[0].ID.String()

		notice, n := noticeKeyed(t, c.tx, bravo, created)
		require.Equal(t, 1, n)
		assert.Equal(t, "Round "+itoa(round.Number)+": the league has now challenged alpha for you", notice.Title)
		assert.Contains(t, notice.Body, "Don't start another one by hand")

		notice, n = noticeKeyed(t, c.tx, alpha, created)
		require.Equal(t, 1, n)
		assert.Equal(t, notify.Challenge, notify.Category(notice.Category))
		assert.Contains(t, notice.Body, "The league has now sent you a challenge from bravo's account")
		assert.Contains(t, notice.Body, "don't start another one by hand")

		for _, user := range []gen.User{alpha, bravo} {
			_, n = noticeKeyed(t, c.tx, user, pairedKey(round, pairings[0]))
			assert.Equal(t, 1, n, "the by-hand notice is not repeated")
		}
	})

	t.Run("refused while game creation is off", func(t *testing.T) {
		c := newCreation(t)
		round, _ := c.creatingRound(t, twoDaysRated, [2]gen.User{c.player(t, "a", 1), c.player(t, "b", 1)})
		c.cfg.GameCreation = settings.GameCreationManual
		_, err := rounds.RetryGameCreation(ctx, c.tx, round.ID, c.cfg, pgtype.UUID{}, c.sched)
		assert.ErrorIs(t, err, rounds.ErrGameCreationOff)
	})

	t.Run("refused for a round published by hand", func(t *testing.T) {
		c := newCreation(t)
		round := addPublishedRound(t, c.tx, nil)
		_, err := rounds.RetryGameCreation(ctx, c.tx, round.ID, c.cfg, pgtype.UUID{}, c.sched)
		assert.ErrorIs(t, err, rounds.ErrNotCreatingGames)
		assert.Equal(t, gen.GameCreationStateManual, c.round(t, round).GameCreation)
	})

	t.Run("refused while a job for the round waits", func(t *testing.T) {
		c := newCreation(t)
		round, _ := c.creatingRound(t, twoDaysRated, [2]gen.User{c.player(t, "a", 1), c.player(t, "b", 1)})
		c.sched.busy = map[int32]bool{round.ID: true}
		_, err := rounds.RetryGameCreation(ctx, c.tx, round.ID, c.cfg, pgtype.UUID{}, c.sched)
		assert.ErrorIs(t, err, rounds.ErrAlreadyQueued)
	})
}

func TestCountInFlight_ACreatedGameKeepsACappedPlayerOut(t *testing.T) {
	// Spec §5.8: a game the league created counts from its creation, not
	// from sync-games ingesting it, so a capped player is at capacity
	// at the next generation even if sync-games has not run.
	c := newCreation(t)
	ctx := context.Background()
	capped := c.player(t, "capped", 2000)
	other := c.player(t, "other", 1900)
	setCapacity(t, c.tx, capped, 1)
	round, _ := c.creatingRound(t, twoDaysRated, [2]gen.User{capped, other})
	c.run(t, round, false)

	next := generate(t, c.tx, c.cfg, nil)
	exclusion, err := c.q.GetExclusionForUserInRound(ctx, gen.GetExclusionForUserInRoundParams{
		RoundID: next.Round.ID,
		UserID:  capped.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, gen.ExclusionReasonAtCapacity, exclusion.Reason)
	require.NotNil(t, exclusion.OngoingGames)
	assert.EqualValues(t, 1, *exclusion.OngoingGames)
}
