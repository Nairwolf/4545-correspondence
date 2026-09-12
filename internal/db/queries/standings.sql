-- name: UpsertPlayerStanding :exec
-- The write side of the "materialised, recomputed on ingest" table (spec
-- §4.1) — internal/standings computes every value with internal/scoring
-- and writes the whole row back here in one call, rather than the
-- read-modify-write pattern an incremental UPDATE would need.
INSERT INTO player_standings (
  user_id, rating, is_unrated, games_played, wins, draws, losses, ongoing,
  last_k_score, last_k_perf_rating, power_rating, color_score, xp, level,
  xp_to_next_level, last_level_up_round, last_level_up_at, last_game_finished_at, is_eligible
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19
)
ON CONFLICT (user_id) DO UPDATE SET
  rating                = EXCLUDED.rating,
  is_unrated            = EXCLUDED.is_unrated,
  games_played          = EXCLUDED.games_played,
  wins                  = EXCLUDED.wins,
  draws                 = EXCLUDED.draws,
  losses                = EXCLUDED.losses,
  ongoing               = EXCLUDED.ongoing,
  last_k_score          = EXCLUDED.last_k_score,
  last_k_perf_rating    = EXCLUDED.last_k_perf_rating,
  power_rating          = EXCLUDED.power_rating,
  color_score           = EXCLUDED.color_score,
  xp                    = EXCLUDED.xp,
  level                 = EXCLUDED.level,
  xp_to_next_level      = EXCLUDED.xp_to_next_level,
  last_level_up_round   = EXCLUDED.last_level_up_round,
  last_level_up_at      = EXCLUDED.last_level_up_at,
  last_game_finished_at = EXCLUDED.last_game_finished_at,
  is_eligible            = EXCLUDED.is_eligible,
  updated_at             = now();

-- name: GetStandings :many
-- One row per approved player — the raw feed for both the /standings and
-- /levels pages (spec §8.1). Sorting, active/inactive filtering and name
-- search are all done in Go (the league is small and it keeps the SQL a
-- single readable statement); the default order here is power rating,
-- highest first, so an un-sorted render already looks right. A player
-- with no player_standings row yet (never recomputed) still appears,
-- sorted last.
SELECT
  u.id, u.lichess_username, p.is_active,
  s.rating, s.is_unrated, s.games_played, s.wins, s.draws, s.losses, s.ongoing,
  s.last_k_score, s.last_k_perf_rating, s.power_rating,
  s.xp, s.level, s.xp_to_next_level, s.last_level_up_round, s.last_level_up_at
FROM users u
JOIN player_profiles p ON p.user_id = u.id
LEFT JOIN player_standings s ON s.user_id = u.id
WHERE u.status = 'approved'
ORDER BY s.power_rating DESC NULLS LAST, u.lichess_username ASC;

-- name: GetPlayerStanding :one
SELECT * FROM player_standings WHERE user_id = $1;
