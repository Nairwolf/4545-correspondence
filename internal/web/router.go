package web

import (
	"io/fs"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Handler is the fully wired HTTP handler for the site.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	// CSRF (spec §11): every non-safe request must be same-origin, judged
	// by Sec-Fetch-Site or Origin vs Host. With SameSite=Lax cookies this
	// is the whole defence — no per-form token.
	r.Use(http.NewCrossOriginProtection().Handler)
	r.Use(s.withUser)

	r.Group(func(r chi.Router) {
		r.Use(middleware.Logger)
		r.Use(middleware.Timeout(15 * time.Second))

		r.Get("/", s.handleHome)
		r.Get("/standings", s.handleStandings)
		r.Get("/levels", s.handleLevels)
		r.Get("/players/{username}", s.handlePlayer)
		r.Get("/health", s.handleHealth)

		r.With(s.authLimiter.middleware).Get("/join", s.handleJoin)
		r.With(s.authLimiter.middleware).Post("/join", s.handleJoinPost)
		r.With(s.authLimiter.middleware).Get("/login", s.handleLogin)
		r.Post("/logout", s.handleLogout)

		// The player dashboard (spec §8.3), one group behind requireUser
		// for the same reason /admin is one group behind requireAdmin.
		r.Route("/account", func(r chi.Router) {
			r.Use(s.requireUser)
			r.Get("/", s.handleAccount)
			r.Post("/activity", s.handleSetActivity)
			r.Post("/capacity", s.handleSetCapacity)
			r.Post("/double-games", s.handleSetDoubleGames)
			r.Post("/resume", s.handleResume)
		})

		// Everything under /admin is behind the role check (spec §11),
		// so a route added here later cannot forget it.
		r.Route("/admin", func(r chi.Router) {
			r.Use(s.requireAdmin)
			r.Get("/", s.handleAdminIndex)
			r.Get("/registrations", s.handleRegistrations)
			r.Post("/registrations/approve", s.handleApproveRegistrations)
			r.Post("/registrations/{id}/reject", s.handleRejectRegistration)
			r.Get("/jobs", s.handleJobs)

			r.Route("/rounds", func(r chi.Router) {
				r.Get("/", s.handleAdminRounds)
				r.Post("/generate", s.handleGenerateRound)
				r.Get("/{id}", s.handleRoundView)
				r.Post("/{id}/publish", s.handlePublishRound)
				r.Post("/{id}/cancel", s.handleCancelRound)
				r.Post("/{id}/regenerate", s.handleRegenerateRound)
				r.Post("/{id}/pairings/swap", s.handleSwapPairings)
				r.Post("/{id}/pairings/{pid}/flip", s.handleFlipPairing)
				r.Post("/{id}/pairings/{pid}/remove", s.handleRemovePairing)
				r.Post("/{id}/pairings/{pid}/fail", s.handleFailPairing)
			})
		})
	})

	// The callback sits outside the logged group on purpose: chi's
	// Logger prints the full request URI, which here carries the
	// one-time authorisation code. The handler logs its own outcome
	// line instead. It also gets a longer timeout — it makes two
	// Lichess calls in sequence.
	r.With(s.authLimiter.middleware, middleware.Timeout(60*time.Second)).
		Get("/auth/lichess/callback", s.handleCallback)

	static, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))

	return r
}

// cacheControl adds a modest cache lifetime to the embedded static
// assets — they only change on deploy, and app.css is fingerprint-free
// so a day is a safe ceiling.
func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}
