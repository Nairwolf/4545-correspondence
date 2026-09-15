// Package web serves the site: the public pages (spec §8.1: home,
// standings, levels, player profiles, /health, /jobs), registration and
// sign-in (spec §8.2, auth.go) and the account page. Public pages render
// from the materialised player_standings / games tables the background
// jobs maintain, never by computing scoring numbers per request.
//
// The player dashboard and admin panel (spec §8.3–§8.5) are later phases.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/lichess"
	"github.com/nairwolf/4545-correspondence/internal/session"
	"github.com/nairwolf/4545-correspondence/internal/settings"
	"github.com/nairwolf/4545-correspondence/internal/tokencrypt"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps is everything the server needs from the outside world.
type Deps struct {
	Pool    *pgxpool.Pool
	Lichess lichess.API
	Auth    lichess.Auth
	// TokenKey encrypts player tokens at rest (TOKEN_ENCRYPTION_KEY).
	TokenKey tokencrypt.Key
	// StateSecret signs the OAuth-state cookie (SESSION_SECRET).
	StateSecret []byte
	// AdminUsernames is the bootstrap admin list (ADMIN_LICHESS_USERNAMES).
	AdminUsernames []string
	// SecureCookies marks every cookie Secure; true when served over https.
	SecureCookies bool
}

// txBeginner is the one thing the sign-in flow needs from the database
// beyond queries: a transaction. Both *pgxpool.Pool and pgx.Tx (which
// nests via a savepoint) satisfy it, so tests can run the whole flow
// inside their rolled-back transaction.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Server holds the dependencies shared by every handler. It is built
// once at startup; settings are read once here rather than per request
// because there is no admin UI to change them at runtime yet (spec §4.2
// editing is Phase 6).
type Server struct {
	pool      *pgxpool.Pool
	db        txBeginner
	q         *gen.Queries
	cfg       settings.Settings
	templates map[string]*template.Template
	started   time.Time

	lichess       lichess.API
	auth          lichess.Auth
	tokenKey      tokencrypt.Key
	stateSecret   []byte
	adminIDs      map[string]bool // Lichess ids (lowercase usernames)
	secureCookies bool
	sessions      *session.Manager
	authLimiter   *rateLimiter
}

// New builds the server, parsing every page template up front so a
// malformed template fails startup rather than the first request.
func New(ctx context.Context, deps Deps) (*Server, error) {
	q := gen.New(deps.Pool)
	cfg, err := settings.Load(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("web: load settings: %w", err)
	}
	return newServer(deps, deps.Pool, q, cfg)
}

// newServer is the shared constructor. Tests use it directly to inject a
// transaction-scoped db and *gen.Queries (so nothing is committed to the
// real database) while still passing the real pool that /health pings.
func newServer(deps Deps, db txBeginner, q *gen.Queries, cfg settings.Settings) (*Server, error) {
	pages := []string{"home", "standings", "levels", "player", "jobs", "join", "account", "auth_error", "admin_registrations"}
	templates := make(map[string]*template.Template, len(pages))
	for _, name := range pages {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(
			templatesFS,
			"templates/layout.html",
			"templates/"+name+".html",
		)
		if err != nil {
			return nil, fmt.Errorf("web: parse template %s: %w", name, err)
		}
		templates[name] = t
	}

	adminIDs := make(map[string]bool, len(deps.AdminUsernames))
	for _, name := range deps.AdminUsernames {
		adminIDs[strings.ToLower(name)] = true
	}

	return &Server{
		pool:          deps.Pool,
		db:            db,
		q:             q,
		cfg:           cfg,
		templates:     templates,
		started:       time.Now(),
		lichess:       deps.Lichess,
		auth:          deps.Auth,
		tokenKey:      deps.TokenKey,
		stateSecret:   deps.StateSecret,
		adminIDs:      adminIDs,
		secureCookies: deps.SecureCookies,
		sessions:      session.New(q, deps.SecureCookies),
		authLimiter:   newRateLimiter(10, 10),
	}, nil
}

// render executes a page template through the shared layout, buffering
// first so a template error becomes a 500 rather than a half-written
// 200.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	t, ok := s.templates[name]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.Copy(w, &buf)
}

// serverError logs nothing here (the router's logger middleware already
// records the request); it just renders a plain 500.
func serverError(w http.ResponseWriter, err error) {
	http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
}

// base is embedded by every page's data struct so the layout can render
// the nav and title without each handler restating them.
type base struct {
	Title   string
	Nav     string    // active top-nav item: "home" | "standings" | "levels" | "account" | "admin" | "jobs"
	User    *gen.User // signed-in user, nil for a visitor
	IsAdmin bool
}

// page builds the base for a request: title, active nav item, and who
// is signed in.
func (s *Server) page(r *http.Request, title, nav string) base {
	b := base{Title: title, Nav: nav}
	if u, ok := currentUser(r); ok {
		b.User = &u
		b.IsAdmin = u.Role == gen.UserRoleAdmin
	}
	return b
}

// --- template helpers -------------------------------------------------

var funcs = template.FuncMap{
	"gameURL": func(id string) string { return "https://lichess.org/" + id },
	"userURL": func(name string) string { return "https://lichess.org/@/" + name },
	"playerURL": func(name string) string {
		return "/players/" + template.URLQueryEscaper(name)
	},
	"int32OrDash": func(v *int32) string {
		if v == nil {
			return "—"
		}
		return strconv.Itoa(int(*v))
	},
	"numOrDash":  func(n pgtype.Numeric) string { return numericString(n, "—") },
	"num1":       func(n pgtype.Numeric) string { return numericString(n, "0.0") },
	"date":       func(t pgtype.Timestamptz) string { return tsFormat(t, "2006-01-02") },
	"datetime":   func(t pgtype.Timestamptz) string { return tsFormat(t, "2006-01-02 15:04") },
	"daysSince":  daysSince,
	"resultWord": resultWord,
	"scoreLine":  scoreLine,
	"lower":      strings.ToLower,
	"pct": func(part, whole int) string {
		if whole == 0 {
			return "0"
		}
		return strconv.FormatFloat(float64(part)/float64(whole)*100, 'f', 0, 64)
	},
	"sortHref":  sortHref,
	"sortArrow": sortArrow,
}

func numericString(n pgtype.Numeric, zero string) string {
	if !n.Valid {
		return zero
	}
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return zero
	}
	return strconv.FormatFloat(f.Float64, 'f', -1, 64)
}

func tsFormat(t pgtype.Timestamptz, layout string) string {
	if !t.Valid {
		return "—"
	}
	return t.Time.UTC().Format(layout)
}

// daysSince is whole days between t and now, floored at 0. Used for
// "days since last move" on ongoing games (spec §8.1).
func daysSince(t pgtype.Timestamptz) int {
	if !t.Valid {
		return 0
	}
	d := int(math.Floor(time.Since(t.Time).Hours() / 24))
	if d < 0 {
		return 0
	}
	return d
}

// scoreLine renders a finished game's result as a chess score, from
// White's point of view.
func scoreLine(r *gen.GameResult) string {
	if r == nil {
		return "—"
	}
	switch *r {
	case gen.GameResultWhiteWin:
		return "1–0"
	case gen.GameResultBlackWin:
		return "0–1"
	case gen.GameResultDraw:
		return "½–½"
	}
	return "—"
}

// resultWord renders a game result from one player's point of view.
func resultWord(r *gen.GameResult, playedWhite bool) string {
	if r == nil {
		return "·"
	}
	switch *r {
	case gen.GameResultDraw:
		return "draw"
	case gen.GameResultWhiteWin:
		if playedWhite {
			return "win"
		}
		return "loss"
	case gen.GameResultBlackWin:
		if playedWhite {
			return "loss"
		}
		return "win"
	}
	return "·"
}
