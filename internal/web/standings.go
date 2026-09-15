package web

import (
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
)

// standingRow is the fully-resolved shape the standings and levels
// templates render — no nil-unwrapping or formatting left for the
// template beyond the shared helpers.
type standingRow struct {
	Rank             int
	Username         string
	IsActive         bool
	Unrated          bool
	Rating           *int32
	Games            *int32
	Wins             *int32
	Draws            *int32
	Losses           *int32
	Ongoing          *int32
	Last5            pgtype.Numeric
	Last5Perf        *int32
	PowerRating      *int32
	XP               *int32
	Level            *int32
	XPToNext         *int32
	LastLevelUpRound *int32
	LastLevelUpAt    pgtype.Timestamptz
}

func toStandingRow(rank int, r gen.GetStandingsRow) standingRow {
	return standingRow{
		Rank:             rank,
		Username:         r.LichessUsername,
		IsActive:         r.IsActive,
		Unrated:          r.IsUnrated != nil && *r.IsUnrated,
		Rating:           r.Rating,
		Games:            r.GamesPlayed,
		Wins:             r.Wins,
		Draws:            r.Draws,
		Losses:           r.Losses,
		Ongoing:          r.Ongoing,
		Last5:            r.LastKScore,
		Last5Perf:        r.LastKPerfRating,
		PowerRating:      r.PowerRating,
		XP:               r.Xp,
		Level:            r.Level,
		XPToNext:         r.XpToNextLevel,
		LastLevelUpRound: r.LastLevelUpRound,
		LastLevelUpAt:    r.LastLevelUpAt,
	}
}

// standingsParams is the parsed ?sort=&dir=&active=&q= query
// (spec §8.1: "sortable on every column, filterable by active/inactive,
// searchable by name").
type standingsParams struct {
	Sort   string
	Dir    string // "asc" | "desc"
	Active string // "" (all) | "1" | "0"
	Q      string
}

func parseStandingsParams(r *http.Request) standingsParams {
	q := r.URL.Query()
	p := standingsParams{
		Sort:   q.Get("sort"),
		Dir:    q.Get("dir"),
		Active: q.Get("active"),
		Q:      strings.TrimSpace(q.Get("q")),
	}
	if p.Dir != "asc" && p.Dir != "desc" {
		p.Dir = "asc"
	}
	if p.Active != "1" && p.Active != "0" {
		p.Active = ""
	}
	if !sortableColumns[p.Sort] {
		p.Sort = ""
	}
	return p
}

var sortableColumns = map[string]bool{
	"player": true, "rating": true, "games": true, "wins": true, "draws": true,
	"losses": true, "ongoing": true, "last5": true, "perf": true, "active": true,
}

// arrangeStandings filters then sorts the raw feed. It is pure so the
// filter/sort behaviour can be unit-tested without a database. With no
// sort column it preserves the query's own order (power rating desc,
// then username), which is what the page shows by default.
func arrangeStandings(rows []gen.GetStandingsRow, p standingsParams) []gen.GetStandingsRow {
	out := make([]gen.GetStandingsRow, 0, len(rows))
	for _, row := range rows {
		if p.Active == "1" && !row.IsActive {
			continue
		}
		if p.Active == "0" && row.IsActive {
			continue
		}
		if p.Q != "" && !strings.Contains(strings.ToLower(row.LichessUsername), strings.ToLower(p.Q)) {
			continue
		}
		out = append(out, row)
	}

	if p.Sort != "" {
		sort.SliceStable(out, func(i, j int) bool {
			less := standingLess(out[i], out[j], p.Sort)
			if p.Dir == "desc" {
				return standingLess(out[j], out[i], p.Sort)
			}
			return less
		})
	}
	return out
}

// standingLess reports whether a sorts before b on the given column, in
// ascending order. A nil metric (player with no computed standing yet)
// always sorts smallest, so it lands last under a descending sort.
func standingLess(a, b gen.GetStandingsRow, col string) bool {
	switch col {
	case "player":
		return strings.ToLower(a.LichessUsername) < strings.ToLower(b.LichessUsername)
	case "active":
		return boolRank(a.IsActive) < boolRank(b.IsActive)
	case "rating":
		return intPtrLess(a.Rating, b.Rating)
	case "games":
		return intPtrLess(a.GamesPlayed, b.GamesPlayed)
	case "wins":
		return intPtrLess(a.Wins, b.Wins)
	case "draws":
		return intPtrLess(a.Draws, b.Draws)
	case "losses":
		return intPtrLess(a.Losses, b.Losses)
	case "ongoing":
		return intPtrLess(a.Ongoing, b.Ongoing)
	case "last5":
		return numericLess(a.LastKScore, b.LastKScore)
	case "perf":
		return intPtrLess(a.LastKPerfRating, b.LastKPerfRating)
	}
	return false
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func intPtrLess(a, b *int32) bool {
	if a == nil {
		return b != nil
	}
	if b == nil {
		return false
	}
	return *a < *b
}

func numericLess(a, b pgtype.Numeric) bool {
	av, aok := numericFloat(a)
	bv, bok := numericFloat(b)
	if !aok {
		return bok
	}
	if !bok {
		return false
	}
	return av < bv
}

func numericFloat(n pgtype.Numeric) (float64, bool) {
	if !n.Valid {
		return 0, false
	}
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0, false
	}
	return f.Float64, true
}

type standingsData struct {
	base
	Rows   []standingRow
	Params standingsParams
	Total  int
}

func (s *Server) handleStandings(w http.ResponseWriter, r *http.Request) {
	raw, err := s.q.GetStandings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	p := parseStandingsParams(r)
	arranged := arrangeStandings(raw, p)

	rows := make([]standingRow, len(arranged))
	for i, row := range arranged {
		rows[i] = toStandingRow(i+1, row)
	}

	s.render(w, "standings", standingsData{
		base:   s.page(r, "Standings", "standings"),
		Rows:   rows,
		Params: p,
		Total:  len(raw),
	})
}

type levelsData struct {
	base
	Rows []standingRow
	Win  int
	Draw int
	Loss int
}

// handleLevels reproduces the Levels sheet (spec §8.1): the same player
// feed, ranked by XP, with the XP rules stated inline.
func (s *Server) handleLevels(w http.ResponseWriter, r *http.Request) {
	raw, err := s.q.GetStandings(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}

	sort.SliceStable(raw, func(i, j int) bool {
		return intPtrLess(raw[j].Xp, raw[i].Xp) // XP desc; GetStandings already broke ties by username
	})

	rows := make([]standingRow, len(raw))
	for i, row := range raw {
		rows[i] = toStandingRow(i+1, row)
	}

	s.render(w, "levels", levelsData{
		base: s.page(r, "Levels", "levels"),
		Rows: rows,
		Win:  s.cfg.XPWeights.Win,
		Draw: s.cfg.XPWeights.Draw,
		Loss: s.cfg.XPWeights.Loss,
	})
}

// sortHref / sortArrow build the column-header links on the standings
// table. Clicking a column sorts ascending; clicking the active column
// again flips direction. active/inactive filter and search term are
// carried across.
func sortHref(p standingsParams, col string) string {
	v := url.Values{}
	v.Set("sort", col)
	dir := "asc"
	if p.Sort == col && p.Dir == "asc" {
		dir = "desc"
	}
	v.Set("dir", dir)
	if p.Active != "" {
		v.Set("active", p.Active)
	}
	if p.Q != "" {
		v.Set("q", p.Q)
	}
	return "/standings?" + v.Encode()
}

func sortArrow(p standingsParams, col string) string {
	if p.Sort != col {
		return ""
	}
	if p.Dir == "desc" {
		return " ▾"
	}
	return " ▴"
}
