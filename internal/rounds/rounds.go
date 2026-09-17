// Package rounds is the I/O layer around the pure pairing engine: it
// loads the pool and the recent-opponent history out of Postgres, runs
// internal/pairing over them, and writes the round, its pairings, byes,
// double games and exclusions back. It also owns the round state
// machine of spec §6.1 — draft, published, cancelled — and the review
// window that carries a draft from one to the other.
//
// Every entry point takes a transaction and, where a round already
// exists, locks its row first (GetRoundForUpdate), so that Phase 5 can
// put a Lichess call between the lock and the state change without
// changing any of these shapes. Nothing here talks to Lichess: a Phase
// 4 round is published as a set of manual_external pairings that
// players start by hand, and sync-games discovers the games (§6.3).
package rounds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/pairing"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// ErrDraftExists is returned when a round is generated while a draft is
// already waiting for review. Only one draft may exist at a time — a
// database fact (rounds_one_draft), so the scheduled job and an admin's
// "generate now" racing each other is a lost insert rather than two
// rounds for the same week.
var ErrDraftExists = errors.New("a draft round already exists: publish, cancel or regenerate it")

// ErrNotDraft is returned by the paths that only apply to a draft:
// cancelling, regenerating, and editing pairings. A published round may
// already have games behind it.
var ErrNotDraft = errors.New("round is not a draft")

// Scheduler enqueues the publish-round job that ends a draft's review
// window. It is an interface because internal/rounds has no business
// knowing about river, and because the CLI has no job runner at all:
// `ic generate-round` passes nil and the hourly sweep publishes the
// draft instead, at most an hour late.
type Scheduler interface {
	SchedulePublish(ctx context.Context, tx pgx.Tx, roundID int32, publishAt time.Time) error
}

// Snapshot is the §4.2 pairing settings as they stood when a round was
// generated, stored on rounds.settings_used. It is a struct rather than
// a map so the stored JSON has stable key order and two generations can
// be compared, and it carries Rated and DaysPerMove — which Phase 4
// never reads — because Phase 5 creates the games from the round and
// must use the settings the round was paired under, not today's.
type Snapshot struct {
	Mode              settings.PairingMode     `json:"mode"`
	ReviewWindowHours int                      `json:"review_window_hours"`
	AvoidRecentRounds int                      `json:"avoid_recent_rounds"`
	ColorWeight       int                      `json:"color_weight"`
	RepeatPenalty     int                      `json:"repeat_penalty"`
	OddPoolStrategy   settings.OddPoolStrategy `json:"odd_pool_strategy"`
	Rated             bool                     `json:"rated"`
	DaysPerMove       int                      `json:"days_per_move"`
}

func snapshot(cfg settings.Settings) Snapshot {
	return Snapshot{
		Mode:              cfg.PairingMode,
		ReviewWindowHours: cfg.ReviewWindowHours,
		AvoidRecentRounds: cfg.AvoidRecentRounds,
		ColorWeight:       cfg.ColorWeight,
		RepeatPenalty:     cfg.RepeatPenalty,
		OddPoolStrategy:   cfg.OddPoolStrategy,
		Rated:             cfg.Rated,
		DaysPerMove:       cfg.DaysPerMove,
	}
}

// engineConfig translates the settings into the engine's own config.
// The two enums are spelled the same in the database, but they are
// mapped explicitly rather than converted by string: the engine is not
// supposed to care what a settings row looks like.
func engineConfig(cfg settings.Settings, number int32) pairing.Config {
	oddPool := pairing.DoubleThenBye
	if cfg.OddPoolStrategy == settings.OddPoolByeOnly {
		oddPool = pairing.ByeOnly
	}
	return pairing.Config{
		RoundNumber:       int(number),
		AvoidRecentRounds: cfg.AvoidRecentRounds,
		ColorWeight:       cfg.ColorWeight,
		RepeatPenalty:     cfg.RepeatPenalty,
		OddPool:           oddPool,
	}
}

// pool is the engine's input together with the mapping back from the
// engine's string ids to the uuids the database wants.
type pool struct {
	players []pairing.Player
	history pairing.History
	userIDs map[string]pgtype.UUID
}

// loadPool reads everything §6.2 step 1 needs. It deliberately computes
// eligibility live from users and player_profiles rather than reading
// player_standings.is_eligible, which is only refreshed on ingest.
func loadPool(ctx context.Context, q *gen.Queries, cfg settings.Settings, number int32) (pool, error) {
	rows, err := q.ListPairingPool(ctx)
	if err != nil {
		return pool{}, fmt.Errorf("list pairing pool: %w", err)
	}

	p := pool{
		players: make([]pairing.Player, 0, len(rows)),
		userIDs: make(map[string]pgtype.UUID, len(rows)),
	}
	for _, row := range rows {
		player, err := toPlayer(ctx, q, row, cfg)
		if err != nil {
			return pool{}, err
		}
		p.players = append(p.players, player)
		p.userIDs[player.ID] = row.UserID
	}

	p.history, err = loadHistory(ctx, q, number, cfg.AvoidRecentRounds)
	if err != nil {
		return pool{}, err
	}
	return p, nil
}

func toPlayer(ctx context.Context, q *gen.Queries, row gen.ListPairingPoolRow, cfg settings.Settings) (pairing.Player, error) {
	inFlight, err := q.CountInFlightGamesForUser(ctx, row.UserID)
	if err != nil {
		return pairing.Player{}, fmt.Errorf("count in-flight games: %w", err)
	}
	lastBye, err := lastByeRound(ctx, q, row.UserID)
	if err != nil {
		return pairing.Player{}, err
	}
	lastDouble, err := lastDoubleRound(ctx, q, row.UserID)
	if err != nil {
		return pairing.Player{}, err
	}

	return pairing.Player{
		ID:              row.UserID.String(),
		Status:          statusOf(row),
		PowerRating:     powerRating(row, cfg),
		ColorScore:      derefInt32(row.ColorScore),
		InFlight:        int(inFlight),
		MaxConcurrent:   optionalInt(row.MaxConcurrentGames),
		AcceptsDouble:   row.AcceptsDoubleGame,
		LastByeRound:    lastBye,
		LastDoubleRound: lastDouble,
		ByeCount:        int(row.ByeCount),
		DoubleCount:     int(row.DoubleCount),
	}, nil
}

// statusOf resolves the §5.7 flags into the one reason the player will
// be told, in the order §5.7 lists them: a player who is both inactive
// and paused reads as inactive, which is the one they chose themselves.
func statusOf(row gen.ListPairingPoolRow) pairing.Status {
	switch {
	case !row.IsActive:
		return pairing.StatusInactive
	case row.PausedByAdmin:
		return pairing.StatusPaused
	case row.AutoPausedAt.Valid:
		return pairing.StatusAutoPaused
	default:
		return pairing.StatusActive
	}
}

// powerRating falls back to rating.unrated_default for a player with no
// player_standings row yet — a seeded or just-approved player, until
// the next recompute. Treating the missing rating as zero would sort
// them to the bottom of the pool and pair them with whoever is last;
// dropping them from the pool would silently deny an approved player
// their game. Spec §14 leaves "should unrated players be pooled at the
// league median" open; the default is the closest answer the settings
// already contain.
func powerRating(row gen.ListPairingPoolRow, cfg settings.Settings) int {
	if row.PowerRating == nil {
		return cfg.UnratedDefault
	}
	return int(*row.PowerRating)
}

func lastByeRound(ctx context.Context, q *gen.Queries, userID pgtype.UUID) (*int, error) {
	bye, err := q.GetLastByeForUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // never had one: the rotation treats that as having waited forever
	}
	if err != nil {
		return nil, fmt.Errorf("last bye: %w", err)
	}
	number := int(bye.RoundNumber)
	return &number, nil
}

func lastDoubleRound(ctx context.Context, q *gen.Queries, userID pgtype.UUID) (*int, error) {
	double, err := q.GetLastDoubleGameForUser(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("last double game: %w", err)
	}
	number := int(double.RoundNumber)
	return &number, nil
}

func loadHistory(ctx context.Context, q *gen.Queries, number int32, window int) (pairing.History, error) {
	history := pairing.History{}
	if window <= 0 {
		return history, nil
	}
	rows, err := q.ListRecentOpponents(ctx, gen.ListRecentOpponentsParams{
		RoundNumber: number,
		WindowSize:  int32(window),
	})
	if err != nil {
		return nil, fmt.Errorf("list recent opponents: %w", err)
	}
	for _, row := range rows {
		history.Record(row.WhiteUserID.String(), row.BlackUserID.String(), int(row.RoundNumber))
	}
	return history, nil
}

// draftConflict recognises the one draft index. The check-then-insert
// alternative would leave a window between the two statements in which
// the other generator commits.
func draftConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "rounds_one_draft"
}

func derefInt32(v *int32) int {
	if v == nil {
		return 0
	}
	return int(*v)
}

// optionalInt carries a nullable cap across without ever coercing NULL
// to zero: NULL is unlimited (spec §5.8), zero is "no games at all".
func optionalInt(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func int32Ptr(v int) *int32 {
	n := int32(v)
	return &n
}

func timestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
