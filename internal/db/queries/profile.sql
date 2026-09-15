-- Player self-service (spec §8.3): the four things a player controls on
-- their dashboard. Each write is paired with an audit_log row by the
-- caller, which reads the profile first so the audit can record the
-- previous value.

-- name: SetPlayerActive :one
-- "I'm playing" / "Pause my quest". Takes effect at the next round;
-- games already running are untouched (spec §8.3).
UPDATE player_profiles SET is_active = $2 WHERE user_id = $1 RETURNING *;

-- name: SetPlayerCapacity :one
-- max_concurrent_games caps TOTAL ongoing games, not pairings per
-- round (spec §5.8). NULL is the unlimited default and the value the
-- dashboard writes when the player turns the limit off — never 0, which
-- the CHECK constraint refuses anyway.
UPDATE player_profiles SET max_concurrent_games = $2 WHERE user_id = $1 RETURNING *;

-- name: SetPlayerAcceptsDouble :one
-- Opt out of (or back into) absorbing an odd pool with two games
-- (spec §6.2 step 6a). On by default; no penalty either way.
UPDATE player_profiles SET accepts_double_game = $2 WHERE user_id = $1 RETURNING *;

-- name: ClearAutoPause :execrows
-- "Resume quest" (spec §8.3): only meaningful while auto-paused, so a
-- stale double-submit changes nothing and the caller can tell from the
-- row count that there is nothing to audit.
UPDATE player_profiles SET auto_paused_at = NULL
WHERE user_id = $1 AND auto_paused_at IS NOT NULL;
