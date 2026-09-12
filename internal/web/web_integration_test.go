//go:build integration

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nairwolf/4545-correspondence/internal/db/gen"
	"github.com/nairwolf/4545-correspondence/internal/settings"
)

// testServer wires a Server whose page queries run inside a transaction
// that is rolled back at test end (so nothing is committed to the test
// database), while /health still pings the real pool.
func testServer(t *testing.T) (*Server, *gen.Queries, pgx.Tx) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(ctx) })

	q := gen.New(tx)
	srv, err := newServer(pool, q, settings.Defaults())
	require.NoError(t, err)
	return srv, q, tx
}

func addPlayer(t *testing.T, q *gen.Queries, tx pgx.Tx, name string, active bool, rating, xp, powerRating, last5perf int32, last5 string) gen.User {
	t.Helper()
	ctx := context.Background()
	u, err := q.CreateApprovedUser(ctx, gen.CreateApprovedUserParams{LichessUsername: name, LichessUserID: strings.ToLower(name)})
	require.NoError(t, err)
	_, err = q.CreatePlayerProfile(ctx, u.ID)
	require.NoError(t, err)
	if !active {
		_, err = tx.Exec(ctx, "UPDATE player_profiles SET is_active = false WHERE user_id = $1", u.ID)
		require.NoError(t, err)
	}

	r, p := rating, last5perf
	level, toNext := int32(0), int32(1)
	require.NoError(t, q.UpsertPlayerStanding(ctx, gen.UpsertPlayerStandingParams{
		UserID:          u.ID,
		Rating:          &r,
		GamesPlayed:     5,
		Wins:            2,
		Draws:           1,
		Losses:          2,
		Ongoing:         1,
		LastKScore:      mustNumeric(t, last5),
		LastKPerfRating: &p,
		PowerRating:     powerRating,
		Xp:              xp,
		Level:           level,
		XpToNextLevel:   toNext,
		IsEligible:      true,
	}))
	return u
}

func mustNumeric(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	var n pgtype.Numeric
	require.NoError(t, n.Scan(s))
	return n
}

func get(t *testing.T, srv *Server, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	body, _ := io.ReadAll(rec.Body)
	return rec.Code, string(body)
}

// order returns the usernames in the order they appear in the rendered
// standings/levels table (player links are unique per row).
func order(html string, names ...string) []string {
	type pos struct {
		name string
		at   int
	}
	var ps []pos
	for _, n := range names {
		if i := strings.Index(html, "/players/"+n+`">`+n); i >= 0 {
			ps = append(ps, pos{n, i})
		}
	}
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j-1].at > ps[j].at; j-- {
			ps[j-1], ps[j] = ps[j], ps[j-1]
		}
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.name
	}
	return out
}

func TestStandingsPage(t *testing.T) {
	srv, q, tx := testServer(t)
	// power rating: gm 2400, mid 2000, low 1700
	addPlayer(t, q, tx, "Gm", true, 2380, 40, 2400, 2410, "40")
	addPlayer(t, q, tx, "Mid", true, 1990, 20, 2000, 1995, "25")
	addPlayer(t, q, tx, "Low", false, 1710, 8, 1700, 1690, "10")

	t.Run("default order is power rating, highest first", func(t *testing.T) {
		code, body := get(t, srv, "/standings")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"Gm", "Mid", "Low"}, order(body, "Gm", "Mid", "Low"))
	})

	t.Run("sort by rating ascending reverses it", func(t *testing.T) {
		code, body := get(t, srv, "/standings?sort=rating&dir=asc")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"Low", "Mid", "Gm"}, order(body, "Gm", "Mid", "Low"))
	})

	t.Run("active filter drops the inactive player", func(t *testing.T) {
		code, body := get(t, srv, "/standings?active=1")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"Gm", "Mid"}, order(body, "Gm", "Mid", "Low"))
		assert.NotContains(t, body, `/players/Low">Low`)
	})

	t.Run("search narrows to one", func(t *testing.T) {
		code, body := get(t, srv, "/standings?q=mid")
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{"Mid"}, order(body, "Gm", "Mid", "Low"))
	})
}

func TestLevelsPageOrdersByXP(t *testing.T) {
	srv, q, tx := testServer(t)
	addPlayer(t, q, tx, "Grinder", true, 1600, 90, 1600, 1605, "30") // most XP, lowest rating
	addPlayer(t, q, tx, "Ace", true, 2200, 12, 2200, 2210, "35")

	code, body := get(t, srv, "/levels")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{"Grinder", "Ace"}, order(body, "Grinder", "Ace"))
}

func TestPlayerPage(t *testing.T) {
	srv, q, tx := testServer(t)
	u := addPlayer(t, q, tx, "Solo", true, 1850, 15, 1850, 1860, "20")
	_ = u

	t.Run("known player renders", func(t *testing.T) {
		code, body := get(t, srv, "/players/solo") // case-insensitive lookup
		require.Equal(t, http.StatusOK, code)
		assert.Contains(t, body, "Solo")
		assert.Contains(t, body, "Game history")
	})

	t.Run("unknown player is 404", func(t *testing.T) {
		code, _ := get(t, srv, "/players/ghost")
		assert.Equal(t, http.StatusNotFound, code)
	})
}

func TestHealthEndpoint(t *testing.T) {
	srv, _, _ := testServer(t)
	code, body := get(t, srv, "/health")
	// The test database is up, and no job has failed in this transaction's
	// view, so health is 200 with a JSON body naming every job.
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"db":"ok"`)
	assert.Contains(t, body, "sync-games")
}
