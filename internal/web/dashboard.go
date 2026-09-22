package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/rounds"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/standings"
)

// The player dashboard (spec §8.3) at /account: what a player controls
// themselves — activity, the optional concurrent-games cap, double-game
// acceptance, resuming an auto-paused quest — next to what they most
// want to see: their games, their standing and whether the league can
// still create games for them. Every control is a plain form POST that
// redirects back here (PRG); validation failures re-render the page
// with the message inline. Every change writes one audit_log row.

// recentGamesShown is how many finished games the dashboard lists
// before pointing at the full history on the player page.
const recentGamesShown = 10

type dashboardData struct {
	base
	User    gen.User
	Profile gen.PlayerProfile
	// CanConfigure is false for a rejected applicant: they see their
	// status and nothing to change (spec §8.2, §13).
	CanConfigure bool

	Header   gen.GetPlayerHeaderRow
	Unrated  bool
	LevelPct int

	Ongoing  int // live in-flight game count (spec §5.8's ongoing_games, amended 2026-09-17)
	Capacity capacityView
	Ceiling  int // player.max_concurrent_ceiling

	OngoingGames []playerGame
	RecentGames  []playerGame
	MoreGames    bool // finished games beyond RecentGames exist

	ThisWeek    thisWeekView
	DoubleGames string // "You've played N double games so far.", empty when none

	Token     *gen.OauthToken // nil when none stored
	TokenOK   bool            // stored, not revoked, not expired
	IssuedOn  string
	ExpiresOn string

	Saved string // one-line confirmation after a successful change
	Error string // inline validation message
}

// capacityView is the "games at once" control's state and the plain-
// language sentence beside it (spec §8.3).
type capacityView struct {
	Limited  bool
	Cap      int32
	Sentence string
	OverCap  bool // ongoing exceeds the cap: show the calm explanation
}

// capacityFor words the live count the way §8.3 asks: the number in
// progress, whether a cap applies, and what that means for next week.
func capacityFor(ongoing int, cap *int32) capacityView {
	if cap == nil {
		return capacityView{
			Sentence: fmt.Sprintf("%s in progress — no limit set, you'll be paired every week.", plural(ongoing, "game")),
		}
	}
	v := capacityView{Limited: true, Cap: *cap}
	switch {
	case ongoing < int(*cap):
		v.Sentence = fmt.Sprintf("%d of %d games in progress — you'll be paired this week.", ongoing, *cap)
	default:
		v.Sentence = fmt.Sprintf("%d of %d games in progress — you'll be skipped until one finishes.", ongoing, *cap)
		v.OverCap = ongoing > int(*cap)
	}
	return v
}

// thisWeekKind says which of the "This week" sentences applies. The
// zero value means no round has been published yet: no section.
type thisWeekKind string

const (
	thisWeekPaired    thisWeekKind = "paired"
	thisWeekBye       thisWeekKind = "bye"
	thisWeekExcluded  thisWeekKind = "excluded"
	thisWeekNotInPool thisWeekKind = "not_in_pool"
)

// thisWeekView is the "This week" section (spec §8.3): what happened to
// this player in the latest published round, answering "why didn't I
// get a game this week?" without anyone having to ask.
type thisWeekView struct {
	Kind  thisWeekKind
	Round int32
	// Games has two entries for the odd-pool volunteer (§6.2 step 6a).
	Games []thisWeekGame
	// NeedsChallenge: at least one game still has to be started by
	// hand on Lichess (until Phase 5 creates games itself).
	NeedsChallenge bool
	Sentence       string // bye, excluded and not-in-pool wording
	ByeHistory     string // "N byes so far, most recently in round M.", empty when none
}

type thisWeekGame struct {
	Opponent  string
	Color     string // "white" or "black"
	GameID    string // empty until sync-games has found the game
	NotPlayed bool   // the pairing was marked failed (or cancelled)
}

// thisWeekFor loads the "This week" section for the latest published
// round. A draft is never shown: it can still change before it
// publishes.
func thisWeekFor(ctx context.Context, q *gen.Queries, userID pgtype.UUID) (thisWeekView, error) {
	round, err := q.GetLatestPublishedRound(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return thisWeekView{}, nil
	}
	if err != nil {
		return thisWeekView{}, err
	}
	v := thisWeekView{Round: round.Number}

	if v.ByeHistory, err = byeHistoryFor(ctx, q, userID); err != nil {
		return thisWeekView{}, err
	}

	pairings, err := q.ListPairingsForUserInRound(ctx, gen.ListPairingsForUserInRoundParams{
		RoundID: round.ID,
		UserID:  userID,
	})
	if err != nil {
		return thisWeekView{}, err
	}
	if len(pairings) > 0 {
		v.Kind = thisWeekPaired
		for _, p := range pairings {
			g := thisWeekGame{
				Opponent:  p.OpponentUsername,
				Color:     "black",
				NotPlayed: p.Status == gen.PairingStatusFailed || p.Status == gen.PairingStatusCancelled,
			}
			if p.IsWhite {
				g.Color = "white"
			}
			if p.LichessGameID != nil {
				g.GameID = *p.LichessGameID
			}
			if g.GameID == "" && !g.NotPlayed {
				v.NeedsChallenge = true
			}
			v.Games = append(v.Games, g)
		}
		return v, nil
	}

	exc, err := q.GetExclusionForUserInRound(ctx, gen.GetExclusionForUserInRoundParams{
		RoundID: round.ID,
		UserID:  userID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Every approved player in the pool gets a pairing or an
		// exclusion row, so no row means they were not approved yet
		// when the round was generated.
		v.Kind = thisWeekNotInPool
		v.Sentence = fmt.Sprintf("Round %d was paired before you joined — you'll be in the next one.", round.Number)
	case err != nil:
		return thisWeekView{}, err
	case exc.Reason == gen.ExclusionReasonBye:
		v.Kind = thisWeekBye
		v.Sentence = byeSentence(oddPoolStrategyOf(round))
	default:
		v.Kind = thisWeekExcluded
		v.Sentence = exclusionSentence(exc, round.Number)
	}
	return v, nil
}

// byeHistoryFor words the player's bye count and the last one's round.
func byeHistoryFor(ctx context.Context, q *gen.Queries, userID pgtype.UUID) (string, error) {
	n, err := q.CountByesForUser(ctx, userID)
	if err != nil || n == 0 {
		return "", err
	}
	last, err := q.GetLastByeForUser(ctx, userID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s so far, most recently in round %d.", plural(int(n), "bye"), last.RoundNumber), nil
}

// oddPoolStrategyOf reads the odd-pool rule the round was generated
// under. An imported round has no snapshot; it also has no byes, so
// the default is only a formality there.
func oddPoolStrategyOf(round gen.Round) settings.OddPoolStrategy {
	var snap rounds.Snapshot
	if len(round.SettingsUsed) == 0 || json.Unmarshal(round.SettingsUsed, &snap) != nil {
		return settings.OddPoolDoubleThenBye
	}
	return snap.OddPoolStrategy
}

// byeSentence is spec §8.3's bye wording. It must never read as a
// penalty: a bye goes by rotation alone, so the player who just had one
// is the last in line for the next.
func byeSentence(strategy settings.OddPoolStrategy) string {
	if strategy == settings.OddPoolByeOnly {
		return "Odd number of players this week, so you sat out. You're first in line to avoid the next one."
	}
	return "Odd number of players this week and nobody was free for a double game, so you sat out. You're first in line to avoid the next one."
}

// exclusionSentence words a round_exclusions row (spec §4.1, §8.3). The
// pause reasons stay short: the banners at the top of the page already
// explain them and say what to do.
func exclusionSentence(exc gen.RoundExclusion, round int32) string {
	switch exc.Reason {
	case gen.ExclusionReasonAtCapacity:
		if exc.OngoingGames == nil || exc.MaxConcurrentGames == nil {
			return fmt.Sprintf("You were at your games-at-once limit, so you sat out round %d. No penalty — you're back as soon as a game finishes.", round)
		}
		return fmt.Sprintf("You had %s in progress and your limit is %d, so you sat out round %d. No penalty — you're back as soon as a game finishes.",
			plural(int(*exc.OngoingGames), "game"), *exc.MaxConcurrentGames, round)
	case gen.ExclusionReasonInactive:
		return fmt.Sprintf("Your quest was paused, so you sat out round %d.", round)
	case gen.ExclusionReasonPaused:
		return fmt.Sprintf("An admin had paused your quest, so you sat out round %d.", round)
	case gen.ExclusionReasonAutoPaused:
		return fmt.Sprintf("Your quest was paused after unanswered challenges, so you sat out round %d.", round)
	case gen.ExclusionReasonRemovedByAdmin:
		return fmt.Sprintf("An admin removed your pairing for round %d.", round)
	}
	return fmt.Sprintf("You weren't paired in round %d.", round)
}

// savedMessage turns the ?saved= code into the confirmation line.
func savedMessage(code string) string {
	switch code {
	case "activity":
		return "Activity updated. It applies from the next round."
	case "capacity":
		return "Game limit updated. It applies from the next round."
	case "double":
		return "Double-game preference updated."
	case "resume":
		return "Welcome back — your quest resumes with the next round."
	}
	return ""
}

// handleAccount renders the dashboard.
func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	s.renderDashboard(w, r, http.StatusOK, "")
}

// renderDashboard loads everything the page shows. errMsg, when set, is
// a validation message from the POST handler that called this instead
// of redirecting.
func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	ctx := r.Context()
	user, _ := currentUser(r)
	data := dashboardData{
		base:         s.page(r, "Your account", "account"),
		User:         user,
		CanConfigure: canConfigure(user),
		Ceiling:      s.cfg.MaxConcurrentCeiling,
		Saved:        savedMessage(r.URL.Query().Get("saved")),
		Error:        errMsg,
	}

	profile, err := s.q.GetPlayerProfile(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	data.Profile = profile

	header, err := s.q.GetPlayerHeader(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	data.Header = header
	data.Unrated = header.IsUnrated != nil && *header.IsUnrated
	data.LevelPct = levelProgress(header.Xp, header.Level, header.XpToNextLevel)

	ongoing, err := s.q.CountInFlightGamesForUser(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	data.Ongoing = int(ongoing)
	data.Capacity = capacityFor(int(ongoing), profile.MaxConcurrentGames)

	games, err := s.q.ListGamesForUser(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	for _, g := range resolveGames(user.ID, games) {
		switch {
		case g.InProgress:
			data.OngoingGames = append(data.OngoingGames, g)
		case len(data.RecentGames) < recentGamesShown:
			data.RecentGames = append(data.RecentGames, g)
		default:
			data.MoreGames = true
		}
	}

	if user.Status == gen.UserStatusApproved {
		if data.ThisWeek, err = thisWeekFor(ctx, s.q, user.ID); err != nil {
			serverError(w, err)
			return
		}
		doubles, err := s.q.CountDoubleGamesForUser(ctx, user.ID)
		if err != nil {
			serverError(w, err)
			return
		}
		if doubles > 0 {
			data.DoubleGames = fmt.Sprintf("You've played %s so far.", plural(int(doubles), "double game"))
		}
	}

	tok, err := s.q.GetOAuthToken(ctx, user.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// seeded member who has never signed in before: no token yet
	case err != nil:
		serverError(w, err)
		return
	default:
		data.Token = &tok
		data.TokenOK = !tok.RevokedAt.Valid && (!tok.ExpiresAt.Valid || tok.ExpiresAt.Time.After(time.Now()))
		data.IssuedOn = tok.IssuedAt.Time.UTC().Format("2 January 2006")
		if tok.ExpiresAt.Valid {
			data.ExpiresOn = tok.ExpiresAt.Time.UTC().Format("2 January 2006")
		}
	}

	w.WriteHeader(status)
	s.render(w, "account", data)
}

// canConfigure: pending applicants may set their preferences ahead of
// approval (they take effect once approved); rejected ones have nothing
// to configure. Banned users never reach here (withUser).
func canConfigure(u gen.User) bool {
	return u.Status == gen.UserStatusPending || u.Status == gen.UserStatusApproved
}

// --- actions -----------------------------------------------------------

// settingsRequest is the common prologue of the four POST handlers: the
// user, their current profile (the audit "before"), and the parsed form.
// A false return means a response has already been written.
func (s *Server) settingsRequest(w http.ResponseWriter, r *http.Request) (gen.User, gen.PlayerProfile, bool) {
	user, _ := currentUser(r)
	if !canConfigure(user) {
		http.Error(w, "nothing to configure for this account", http.StatusForbidden)
		return gen.User{}, gen.PlayerProfile{}, false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return gen.User{}, gen.PlayerProfile{}, false
	}
	profile, err := s.q.GetPlayerProfile(r.Context(), user.ID)
	if err != nil {
		serverError(w, err)
		return gen.User{}, gen.PlayerProfile{}, false
	}
	return user, profile, true
}

func redirectToDashboard(w http.ResponseWriter, r *http.Request, saved string) {
	http.Redirect(w, r, "/account?saved="+saved, http.StatusSeeOther)
}

// handleSetActivity is "I'm playing" / "Pause my quest" (spec §8.3).
// is_active is one of the inputs to player_standings.is_eligible, so
// the standing is recomputed in the same transaction.
func (s *Server) handleSetActivity(w http.ResponseWriter, r *http.Request) {
	user, profile, ok := s.settingsRequest(w, r)
	if !ok {
		return
	}
	active := r.PostForm.Get("active") == "on"
	if active == profile.IsActive {
		redirectToDashboard(w, r, "activity")
		return
	}
	err := s.inTx(r.Context(), func(q *gen.Queries) error {
		if _, err := q.SetPlayerActive(r.Context(), gen.SetPlayerActiveParams{UserID: user.ID, IsActive: active}); err != nil {
			return err
		}
		if _, err := standings.Recompute(r.Context(), q, user.ID, s.cfg); err != nil {
			return err
		}
		return audit(r.Context(), q, user.ID, "profile.activity", user.ID,
			map[string]bool{"is_active": profile.IsActive},
			map[string]bool{"is_active": active},
		)
	})
	if err != nil {
		serverError(w, err)
		return
	}
	redirectToDashboard(w, r, "activity")
}

// handleSetCapacity is the optional concurrent-games cap (spec §5.8,
// §8.3). The limit off means NULL — unlimited, the default — never 0.
func (s *Server) handleSetCapacity(w http.ResponseWriter, r *http.Request) {
	user, profile, ok := s.settingsRequest(w, r)
	if !ok {
		return
	}
	var cap *int32
	if r.PostForm.Get("limit") == "on" {
		n, err := strconv.Atoi(r.PostForm.Get("cap"))
		if err != nil || n < 1 || n > s.cfg.MaxConcurrentCeiling {
			s.renderDashboard(w, r, http.StatusUnprocessableEntity,
				fmt.Sprintf("The game limit must be a whole number between 1 and %d.", s.cfg.MaxConcurrentCeiling))
			return
		}
		v := int32(n)
		cap = &v
	}
	if sameCap(cap, profile.MaxConcurrentGames) {
		redirectToDashboard(w, r, "capacity")
		return
	}
	err := s.inTx(r.Context(), func(q *gen.Queries) error {
		if _, err := q.SetPlayerCapacity(r.Context(), gen.SetPlayerCapacityParams{UserID: user.ID, MaxConcurrentGames: cap}); err != nil {
			return err
		}
		return audit(r.Context(), q, user.ID, "profile.capacity", user.ID,
			map[string]*int32{"max_concurrent_games": profile.MaxConcurrentGames},
			map[string]*int32{"max_concurrent_games": cap},
		)
	})
	if err != nil {
		serverError(w, err)
		return
	}
	redirectToDashboard(w, r, "capacity")
}

func sameCap(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// handleSetDoubleGames is the odd-pool volunteer opt-out (spec §6.2
// step 6a, §8.3).
func (s *Server) handleSetDoubleGames(w http.ResponseWriter, r *http.Request) {
	user, profile, ok := s.settingsRequest(w, r)
	if !ok {
		return
	}
	accept := r.PostForm.Get("accept") == "on"
	if accept == profile.AcceptsDoubleGame {
		redirectToDashboard(w, r, "double")
		return
	}
	err := s.inTx(r.Context(), func(q *gen.Queries) error {
		if _, err := q.SetPlayerAcceptsDouble(r.Context(), gen.SetPlayerAcceptsDoubleParams{UserID: user.ID, AcceptsDoubleGame: accept}); err != nil {
			return err
		}
		return audit(r.Context(), q, user.ID, "profile.double_games", user.ID,
			map[string]bool{"accepts_double_game": profile.AcceptsDoubleGame},
			map[string]bool{"accepts_double_game": accept},
		)
	})
	if err != nil {
		serverError(w, err)
		return
	}
	redirectToDashboard(w, r, "double")
}

// handleResume clears an auto-pause (spec §8.3 "Resume quest"). Only
// meaningful while auto-paused: otherwise nothing changes and nothing
// is audited. auto_paused_at is an input to is_eligible, so the
// standing is recomputed in the same transaction, as for activity.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	user, profile, ok := s.settingsRequest(w, r)
	if !ok {
		return
	}
	err := s.inTx(r.Context(), func(q *gen.Queries) error {
		n, err := q.ClearAutoPause(r.Context(), user.ID)
		if err != nil || n == 0 {
			return err
		}
		if _, err := standings.Recompute(r.Context(), q, user.ID, s.cfg); err != nil {
			return err
		}
		return audit(r.Context(), q, user.ID, "profile.resume", user.ID,
			map[string]any{"auto_paused_at": profile.AutoPausedAt},
			map[string]any{"auto_paused_at": nil},
		)
	})
	if err != nil {
		serverError(w, err)
		return
	}
	redirectToDashboard(w, r, "resume")
}
