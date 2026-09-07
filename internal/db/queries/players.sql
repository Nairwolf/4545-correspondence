-- name: GetUserByLichessUserID :one
SELECT * FROM users WHERE lichess_user_id = $1;

-- name: GetUserByUsername :one
-- Case-insensitive, matching the users_username_ci unique index.
SELECT * FROM users WHERE lower(lichess_username) = lower($1);

-- name: CreateApprovedUser :one
-- Phase 1 has no registration flow (that's Phase 2): every user the
-- seed-players CLI creates is approved directly, by the CLI operator's
-- own authority, so there is no separate pending/approve step here.
INSERT INTO users (lichess_username, lichess_user_id, status, approved_at)
VALUES ($1, $2, 'approved', now())
RETURNING *;

-- name: ListApprovedUsers :many
SELECT * FROM users WHERE status = 'approved' ORDER BY lichess_username;

-- name: CreatePlayerProfile :one
-- All other columns take their schema defaults (active, unlimited
-- capacity, double-games opted in — spec §4.1).
INSERT INTO player_profiles (user_id) VALUES ($1) RETURNING *;

-- name: GetPlayerProfile :one
SELECT * FROM player_profiles WHERE user_id = $1;

-- name: InsertRatingSnapshot :one
INSERT INTO rating_snapshots (
  user_id, correspondence_rating, correspondence_prov, correspondence_games,
  classical_rating, classical_prov, classical_games
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetLatestRatingSnapshot :one
SELECT * FROM rating_snapshots
WHERE user_id = $1
ORDER BY fetched_at DESC
LIMIT 1;
