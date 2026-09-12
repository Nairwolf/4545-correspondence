package web

import (
	"net/http"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

type homeData struct {
	base
	Ongoing     []gen.ListOngoingGamesRow
	Recent      []gen.ListRecentFinishedGamesRow
	Top         []standingRow
	ActiveCount int64
}

// handleHome reproduces the Overview sheet (spec §8.1): ongoing games,
// the recent-results feed, the current top 5 by power rating, and the
// active-player count.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	ongoing, err := s.q.ListOngoingGames(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	recent, err := s.q.ListRecentFinishedGames(ctx, 20)
	if err != nil {
		serverError(w, err)
		return
	}
	active, err := s.q.CountActivePlayers(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	standings, err := s.q.GetStandings(ctx)
	if err != nil {
		serverError(w, err)
		return
	}

	// GetStandings is already ordered by power rating, highest first
	// (NULLS last), so the leaders are simply the head of the slice —
	// but skip players with no computed standing yet.
	var top []standingRow
	for _, row := range standings {
		if row.PowerRating == nil {
			break
		}
		top = append(top, toStandingRow(len(top)+1, row))
		if len(top) == 5 {
			break
		}
	}

	s.render(w, "home", homeData{
		base:        base{Title: "Infinite Correspondence", Nav: "home"},
		Ongoing:     ongoing,
		Recent:      recent,
		Top:         top,
		ActiveCount: active,
	})
}
