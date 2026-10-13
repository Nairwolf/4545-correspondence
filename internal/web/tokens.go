package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/tokenhealth"
)

// Token health for admins (spec §3.3 item 3, §8.5): the /admin/tokens
// page, and the banner on every admin page. Both are computed on each
// request from the same rows as /health, never stored as alerts
// (Phase 5 decision 10).

// organiserCheck is the organiser token's state as the latest
// validate-tokens run that got an answer from Lichess recorded it.
// Organiser is nil when no run has, yet.
type organiserCheck struct {
	Organiser *tokenhealth.Organiser
	CheckedAt time.Time
}

// organiserToken reads the latest successful validate-tokens run. A
// failed run (Lichess down) learned nothing, so the state found before
// it still stands.
func (s *Server) organiserToken(ctx context.Context) (organiserCheck, error) {
	run, err := s.q.GetLatestSuccessfulJobRunByName(ctx, "validate-tokens")
	if errors.Is(err, pgx.ErrNoRows) {
		return organiserCheck{}, nil
	}
	if err != nil {
		return organiserCheck{}, err
	}
	var detail struct {
		Organiser *tokenhealth.Organiser `json:"organiser_token"`
	}
	if err := json.Unmarshal(run.Detail, &detail); err != nil {
		return organiserCheck{}, fmt.Errorf("read validate-tokens run %d: %w", run.ID, err)
	}
	return organiserCheck{Organiser: detail.Organiser, CheckedAt: run.StartedAt.Time}, nil
}

// adminAlert is one line of the admin banner.
type adminAlert struct {
	Text string
	Link string
}

// adminPage is page plus the admin banner, for the pages under /admin.
// A failed query becomes an alert of its own rather than failing the
// page: the banner is the admins' warning, not what they came for.
func (s *Server) adminPage(r *http.Request, title, nav string) base {
	b := s.page(r, title, nav)
	alerts, err := s.adminAlerts(r.Context(), time.Now())
	if err != nil {
		alerts = append(alerts, adminAlert{Text: "The site could not check its own health: " + err.Error()})
	}
	b.Alerts = alerts
	return b
}

// adminAlerts is the banner (spec §8.5): a draft waiting for review and
// when it will publish itself (§6.1), a watched job whose latest run
// failed, and, when the site creates games, an organiser token that
// cannot or soon will not work.
func (s *Server) adminAlerts(ctx context.Context, now time.Time) ([]adminAlert, error) {
	var alerts []adminAlert

	draft, err := s.q.GetDraftRound(ctx)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return alerts, err
	default:
		alerts = append(alerts, adminAlert{
			Text: fmt.Sprintf("Round %d is a draft waiting for review. It publishes itself at %s UTC.",
				draft.Number, draft.PublishAt.Time.UTC().Format("Mon 2 Jan 15:04")),
			Link: fmt.Sprintf("/admin/rounds/%d", draft.ID),
		})
	}

	for _, name := range jobNames {
		last, err := s.q.GetLatestJobRunByName(ctx, name)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return alerts, err
		}
		if last.Status == gen.JobRunStatusFailed {
			alerts = append(alerts, adminAlert{
				Text: fmt.Sprintf("The latest %s run failed (%s UTC).",
					name, last.StartedAt.Time.UTC().Format("Mon 2 Jan 15:04")),
				Link: "/admin/jobs",
			})
		}
	}

	cfg, err := settings.Load(ctx, s.q)
	if err != nil {
		return alerts, err
	}
	if cfg.GameCreation != settings.GameCreationLichess {
		// Under manual nothing uses the organiser token, and an empty
		// LICHESS_ORG_TOKEN is expected.
		return alerts, nil
	}
	check, err := s.organiserToken(ctx)
	if err != nil {
		return alerts, err
	}
	if text := organiserProblem(check.Organiser, now); text != "" {
		alerts = append(alerts, adminAlert{Text: text, Link: "/admin/tokens"})
	}
	return alerts, nil
}

// organiserProblem words what is wrong with the organiser token for the
// banner, or "" if nothing is. o is nil when it was never checked.
func organiserProblem(o *tokenhealth.Organiser, now time.Time) string {
	const cannot = " The site cannot create games until it is fixed."
	switch {
	case o == nil:
		return "The organiser token has not been checked yet. Run ic validate-tokens."
	case o.State == tokenhealth.OrganiserNotSet:
		return "LICHESS_ORG_TOKEN is not set." + cannot
	case o.State == tokenhealth.OrganiserInvalid:
		return "Lichess does not accept the organiser token." + cannot
	case o.State == tokenhealth.OrganiserMissingScope:
		return "The organiser token lacks the challenge:bulk permission." + cannot
	case !o.Usable(now):
		return "The organiser token expired on " + o.Expires.UTC().Format("2 January 2006") + "." + cannot
	case o.ExpiresSoon(now):
		return "The organiser token expires on " + o.Expires.UTC().Format("2 January 2006") + ". Replace it before then."
	}
	return ""
}

type tokensData struct {
	base
	GameCreation settings.GameCreation
	GraceDays    int

	Organiser *tokenhealth.Organiser
	CheckedAt time.Time
	Problem   string // organiserProblem, whatever the mode

	Rows []tokenRow
}

// tokenRow is one approved player without a valid token.
type tokenRow struct {
	Username string
	Problem  string    // "never authorised", "revoked" or "expired"
	Since    time.Time // zero when never authorised
	// GraceLeft words the days of grace left under lichess.
	GraceLeft string
	Excluded  bool
	// Sitting is why they are not paired anyway, if they are not: the
	// token matters less for them.
	Sitting string
}

// handleAdminTokens is /admin/tokens (spec §3.3 item 3, §8.5): the
// approved players without a valid token, since when and the grace
// left, and the organiser token's state and expiry.
func (s *Server) handleAdminTokens(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()

	cfg, err := settings.Load(ctx, s.q)
	if err != nil {
		serverError(w, err)
		return
	}
	check, err := s.organiserToken(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	players, err := s.q.ListApprovedPlayerTokens(ctx)
	if err != nil {
		serverError(w, err)
		return
	}

	data := tokensData{
		base:         s.adminPage(r, "Tokens", "tokens"),
		GameCreation: cfg.GameCreation,
		GraceDays:    cfg.TokenInvalidGraceDays,
		Organiser:    check.Organiser,
		CheckedAt:    check.CheckedAt,
		Problem:      organiserProblem(check.Organiser, now),
	}
	for _, p := range players {
		tok := tokenhealth.PlayerOf(p.TokenIssuedAt, p.TokenRevokedAt, p.TokenExpiresAt, now)
		if tok.Valid() {
			continue
		}
		row := tokenRow{
			Username: p.LichessUsername,
			Problem:  tokenProblem(tok),
			Since:    tok.InvalidSince,
			Excluded: tok.Excluded(cfg.TokenInvalidGraceDays, now),
		}
		if tok.Stored && !row.Excluded {
			row.GraceLeft = plural(tok.GraceDaysLeft(cfg.TokenInvalidGraceDays, now), "day")
		}
		switch {
		case !p.IsActive:
			row.Sitting = "paused by them"
		case p.PausedByAdmin:
			row.Sitting = "paused by an admin"
		case p.AutoPausedAt.Valid:
			row.Sitting = "auto-paused"
		}
		data.Rows = append(data.Rows, row)
	}

	s.render(w, "admin_tokens", data)
}

// tokenProblem names what is wrong with a token that is not valid.
func tokenProblem(tok tokenhealth.Player) string {
	switch {
	case !tok.Stored:
		return "never authorised"
	case tok.Revoked:
		return "revoked"
	default:
		return "expired"
	}
}
