package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/session"
)

type ctxKey int

const userKey ctxKey = iota

// withUser resolves the request's session to its users row, for the nav
// and for the guards below. Status is re-read on every request, so an
// approval, rejection or ban takes effect on the user's next click
// without touching their session — except a ban, which also ends it.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.sessions.Load(r.Context(), r)
		switch {
		case errors.Is(err, session.ErrNoSession):
			next.ServeHTTP(w, r)
			return
		case err != nil:
			serverError(w, err)
			return
		}
		if u.Status == gen.UserStatusBanned {
			if err := s.sessions.Destroy(r.Context(), w, r); err != nil {
				serverError(w, err)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// currentUser returns the signed-in user, if any.
func currentUser(r *http.Request) (gen.User, bool) {
	u, ok := r.Context().Value(userKey).(gen.User)
	return u, ok
}

// requireUser sends anonymous requests to sign in.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := currentUser(r); !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAdmin is the server-side role check spec §11 asks for on
// every admin request: anonymous → sign in; signed in but not an admin
// → 403.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := currentUser(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if u.Role != gen.UserRoleAdmin {
			http.Error(w, "admins only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
