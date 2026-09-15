package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/scoring"
)

type playerGame struct {
	GameID     string
	Round      int32
	Opponent   string
	Color      string
	Result     string
	InProgress bool
	Opening    string
	Accuracy   pgtype.Numeric
}

type playerData struct {
	base
	Header      gen.GetPlayerHeaderRow
	Unrated     bool
	Games       []playerGame
	LevelPct    int // progress through the current level, 0–100
	Wins        int
	Draws       int
	Losses      int
	Total       int
	RatingChart chartData
	XPChart     chartData
}

// chartData is a ready-to-draw inline SVG line: a "points" attribute for
// a <polyline> plus the value range, in a fixed 600×160 viewBox. Empty
// Points means "not enough data, hide the chart".
type chartData struct {
	Points string
	Min    float64
	Max    float64
	Last   float64
}

func (s *Server) handlePlayer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	username := chi.URLParam(r, "username")

	user, err := s.q.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}

	header, err := s.q.GetPlayerHeader(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	games, err := s.q.ListGamesForUser(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	snapshots, err := s.q.ListRatingSnapshotsForUser(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}
	finishedAsc, err := s.q.ListFinishedGamesForUserAsc(ctx, user.ID)
	if err != nil {
		serverError(w, err)
		return
	}

	data := playerData{
		base:        s.page(r, header.LichessUsername, ""),
		Header:      header,
		Unrated:     header.IsUnrated != nil && *header.IsUnrated,
		Games:       resolveGames(user.ID, games),
		LevelPct:    levelProgress(header.Xp, header.Level, header.XpToNextLevel),
		RatingChart: ratingChart(snapshots),
		XPChart:     xpChart(user.ID, finishedAsc, s.cfg.XPWeights),
	}
	for _, g := range data.Games {
		if g.InProgress {
			continue
		}
		switch g.Result {
		case "win":
			data.Wins++
		case "draw":
			data.Draws++
		case "loss":
			data.Losses++
		}
	}
	data.Total = data.Wins + data.Draws + data.Losses

	s.render(w, "player", data)
}

func resolveGames(userID pgtype.UUID, rows []gen.ListGamesForUserRow) []playerGame {
	out := make([]playerGame, 0, len(rows))
	for _, g := range rows {
		playedWhite := g.WhiteUserID == userID
		pg := playerGame{
			GameID:     g.LichessGameID,
			Round:      g.RoundNumber,
			InProgress: g.Status == gen.GameStatusInProgress,
			Result:     resultWord(g.Result, playedWhite),
		}
		if playedWhite {
			pg.Color = "white"
			pg.Opponent = g.BlackUsername
			pg.Accuracy = g.WhiteAccuracy
		} else {
			pg.Color = "black"
			pg.Opponent = g.WhiteUsername
			pg.Accuracy = g.BlackAccuracy
		}
		if g.OpeningName != nil {
			pg.Opening = *g.OpeningName
		}
		out = append(out, pg)
	}
	return out
}

// levelProgress is how far XP has climbed into the current level, as a
// 0–100 percentage: the span from level² to (level+1)² is the bar, and
// xpToNext is what's left of it.
func levelProgress(xp, level, xpToNext *int32) int {
	if xp == nil || level == nil || xpToNext == nil {
		return 0
	}
	span := (*level*2 + 1) // (level+1)² − level²
	if span <= 0 {
		return 0
	}
	done := span - *xpToNext
	pct := int(float64(done) / float64(span) * 100)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

func ratingChart(snapshots []gen.ListRatingSnapshotsForUserRow) chartData {
	var series []float64
	for _, s := range snapshots {
		switch {
		case s.CorrespondenceRating != nil:
			series = append(series, float64(*s.CorrespondenceRating))
		case s.ClassicalRating != nil:
			series = append(series, float64(*s.ClassicalRating))
		}
	}
	return lineChart(series)
}

// xpChart accumulates XP game by game (oldest first) so the player page
// can show the climb over time (spec §8.1).
func xpChart(userID pgtype.UUID, finished []gen.ListFinishedGamesForUserAscRow, w scoring.XPWeights) chartData {
	var series []float64
	var total float64
	for _, g := range finished {
		if g.Result == nil {
			continue
		}
		playedWhite := g.WhiteUserID == userID
		switch resultWord(g.Result, playedWhite) {
		case "win":
			total += float64(w.Win)
		case "draw":
			total += float64(w.Draw)
		default:
			total += float64(w.Loss)
		}
		series = append(series, total)
	}
	return lineChart(series)
}

// lineChart scales a value series into a 600×160 viewBox (y inverted so
// larger values sit higher). Fewer than two points yields an empty
// chart the template hides.
func lineChart(values []float64) chartData {
	if len(values) < 2 {
		return chartData{}
	}
	min, max := values[0], values[0]
	for _, v := range values {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	span := max - min
	if span == 0 {
		span = 1
	}
	const w, h = 600.0, 160.0
	var b []byte
	for i, v := range values {
		x := float64(i) / float64(len(values)-1) * w
		y := h - (v-min)/span*h
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, []byte(strconv.FormatFloat(x, 'f', 1, 64))...)
		b = append(b, ',')
		b = append(b, []byte(strconv.FormatFloat(y, 'f', 1, 64))...)
	}
	return chartData{Points: string(b), Min: min, Max: max, Last: values[len(values)-1]}
}
