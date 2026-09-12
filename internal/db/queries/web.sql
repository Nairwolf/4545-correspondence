-- Queries backing the public web pages (spec §8.1). Read-only; every
-- page-specific shaping (sorting, filtering, cumulative XP) is done in
-- Go so these stay single readable statements.

-- name: ListRecentFinishedGames :many
-- Home page "recent results feed" (Overview sheet): the most recently
-- finished league games, newest first.
SELECT
  g.lichess_game_id,
  g.round_number,
  g.result,
  g.termination,
  g.finished_at,
  wu.lichess_username AS white_username,
  bu.lichess_username AS black_username
FROM games g
JOIN users wu ON wu.id = g.white_user_id
JOIN users bu ON bu.id = g.black_user_id
WHERE g.status = 'finished'
ORDER BY g.finished_at DESC
LIMIT $1;

-- name: ListOngoingGames :many
-- Home page "ongoing games" (Overview sheet): every in-progress league
-- game, the one whose clock has been sitting longest first — that's the
-- one most likely to need a nudge.
SELECT
  g.lichess_game_id,
  g.round_number,
  g.started_at,
  g.last_move_at,
  wu.lichess_username AS white_username,
  bu.lichess_username AS black_username
FROM games g
JOIN users wu ON wu.id = g.white_user_id
JOIN users bu ON bu.id = g.black_user_id
WHERE g.status = 'in_progress'
ORDER BY g.last_move_at ASC;

-- name: CountActivePlayers :one
-- Home page "active player count".
SELECT count(*)
FROM player_profiles p
JOIN users u ON u.id = p.user_id
WHERE u.status = 'approved' AND p.is_active;

-- name: GetPlayerHeader :one
-- Header block of the player profile page (spec §8.1): identity + the
-- materialised standing. LEFT JOIN so a player who has never had a
-- standing computed still resolves.
SELECT
  u.id,
  u.lichess_username,
  u.created_at AS member_since,
  p.is_active,
  s.rating,
  s.is_unrated,
  s.wins,
  s.draws,
  s.losses,
  s.ongoing,
  s.games_played,
  s.xp,
  s.level,
  s.xp_to_next_level,
  s.power_rating,
  s.last_k_score,
  s.last_k_perf_rating
FROM users u
JOIN player_profiles p ON p.user_id = u.id
LEFT JOIN player_standings s ON s.user_id = u.id
WHERE u.id = $1;

-- name: ListGamesForUser :many
-- Full game history for the player profile page — every league game the
-- player has, finished or in progress, newest first.
SELECT
  g.lichess_game_id, g.pairing_id, g.round_number, g.white_user_id, g.black_user_id,
  g.status, g.result, g.termination, g.lichess_status, g.days_per_turn, g.eco,
  g.opening_name, g.opening_ply, g.white_first_move, g.black_first_move,
  g.white_rating_at_game, g.black_rating_at_game, g.white_accuracy, g.black_accuracy,
  g.white_acpl, g.black_acpl, g.white_moves, g.black_moves, g.started_at,
  g.last_move_at, g.finished_at, g.duration_seconds, g.raw_payload, g.ingested_at, g.updated_at,
  wu.lichess_username AS white_username,
  bu.lichess_username AS black_username
FROM games g
JOIN users wu ON wu.id = g.white_user_id
JOIN users bu ON bu.id = g.black_user_id
WHERE g.white_user_id = $1 OR g.black_user_id = $1
ORDER BY g.started_at DESC;

-- name: ListRatingSnapshotsForUser :many
-- "Rating over time" chart on the player page — oldest first so the
-- handler can draw it straight through.
SELECT correspondence_rating, classical_rating, fetched_at
FROM rating_snapshots
WHERE user_id = $1
ORDER BY fetched_at ASC;

-- name: ListFinishedGamesForUserAsc :many
-- "XP over time" chart on the player page: finished games oldest first,
-- resolved to this player's side in Go to accumulate XP round by round.
SELECT
  white_user_id, result, finished_at, round_number
FROM games
WHERE status = 'finished' AND (white_user_id = $1 OR black_user_id = $1)
ORDER BY finished_at ASC;

-- name: ListAmbiguousPairings :many
-- The /jobs page lists pairings sync-games could not resolve to a single
-- Lichess game (spec §7.3) so an admin can see what needs a hand.
SELECT
  p.id,
  r.number AS round_number,
  p.created_at,
  wu.lichess_username AS white_username,
  bu.lichess_username AS black_username
FROM pairings p
JOIN rounds r ON r.id = p.round_id
JOIN users wu ON wu.id = p.white_user_id
JOIN users bu ON bu.id = p.black_user_id
WHERE p.match_ambiguous
ORDER BY r.number DESC, p.created_at DESC;
