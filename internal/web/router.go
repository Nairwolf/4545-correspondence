package web

import (
	"io/fs"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Handler is the fully wired HTTP handler for the public site.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Get("/", s.handleHome)
	r.Get("/standings", s.handleStandings)
	r.Get("/levels", s.handleLevels)
	r.Get("/players/{username}", s.handlePlayer)
	r.Get("/health", s.handleHealth)
	r.Get("/jobs", s.handleJobs)

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
