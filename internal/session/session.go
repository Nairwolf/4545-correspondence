// Package session implements server-side sessions (spec §8.2): a random
// id in an httpOnly cookie, its sha256 in the sessions table. The
// Lichess token is never a session credential — a session only says
// which users row the browser is.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// CookieName is the session cookie.
const CookieName = "ic_session"

const (
	// lifetime is a session's absolute lifetime; there is no sliding
	// renewal, so a stolen cookie is bounded however active its user.
	lifetime = 30 * 24 * time.Hour
	// touchEvery bounds how often last_seen_at is written: once an hour
	// rather than on every request.
	touchEvery = time.Hour
)

// ErrNoSession is returned by Load when the request carries no live
// session — no cookie, an unknown id, or an expired row.
var ErrNoSession = errors.New("session: none")

// Manager creates, loads and destroys sessions against one set of
// queries.
type Manager struct {
	q      *gen.Queries
	secure bool
	now    func() time.Time
}

// New builds a Manager. secure marks the cookie Secure, which the
// caller derives from whether the site is served over https.
func New(q *gen.Queries, secure bool) *Manager {
	return &Manager{q: q, secure: secure, now: time.Now}
}

// Create starts a session for userID and sets its cookie. Expired rows
// are swept at the same time — enough housekeeping for a league this
// size, with no job to schedule.
func (m *Manager) Create(ctx context.Context, w http.ResponseWriter, userID pgtype.UUID) error {
	if _, err := m.q.DeleteExpiredSessions(ctx); err != nil {
		return fmt.Errorf("session: sweep expired: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("session: random id: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	expires := m.now().Add(lifetime)
	err := m.q.CreateSession(ctx, gen.CreateSessionParams{
		TokenHash: hash(token),
		UserID:    userID,
		ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("session: create: %w", err)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// Load returns the user behind the request's session, or ErrNoSession.
func (m *Manager) Load(ctx context.Context, r *http.Request) (gen.User, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return gen.User{}, ErrNoSession
	}
	h := hash(c.Value)

	row, err := m.q.GetSessionUser(ctx, h)
	if errors.Is(err, pgx.ErrNoRows) {
		return gen.User{}, ErrNoSession
	}
	if err != nil {
		return gen.User{}, fmt.Errorf("session: load: %w", err)
	}

	if m.now().Sub(row.LastSeenAt.Time) >= touchEvery {
		if err := m.q.TouchSession(ctx, h); err != nil {
			return gen.User{}, fmt.Errorf("session: touch: %w", err)
		}
	}
	return row.User, nil
}

// Destroy ends the request's session, if any, and clears the cookie.
func (m *Manager) Destroy(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		if err := m.q.DeleteSession(ctx, hash(c.Value)); err != nil {
			return fmt.Errorf("session: delete: %w", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
