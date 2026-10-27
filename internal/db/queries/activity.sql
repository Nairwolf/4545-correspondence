-- Unstarted pairings, missed starts and auto-pause (spec §6.4; Phase 5
-- step 7): evaluate-activity, the 48-hour reminder and cancel-challenge.

-- name: ListUnstartedPairings :many
-- Pairings of published rounds still waiting for a game, published at
-- or before published_before: what evaluate-activity retires and what
-- the 48-hour reminder is about. Left out: a round whose create-games
-- job is still at work (it owns its pairings until it finishes), a
-- pairing flagged ambiguous (two games exist for it; an admin picks
-- one), and one whose game is already known — any pairing with a game
-- id but a challenge's, whose id is the challenge's until it is
-- accepted. settings_used words the time control in the reminder.
SELECT
  p.id AS pairing_id,
  p.round_id,
  r.number AS round_number,
  r.settings_used,
  p.creation_method,
  p.lichess_game_id,
  p.challenger_user_id,
  p.white_user_id,
  p.black_user_id,
  wu.lichess_username AS white_username,
  bu.lichess_username AS black_username
FROM pairings p
JOIN rounds r ON r.id = p.round_id
JOIN users wu ON wu.id = p.white_user_id
JOIN users bu ON bu.id = p.black_user_id
WHERE r.state = 'published'
  AND r.game_creation <> 'creating'
  AND r.published_at <= sqlc.arg(published_before)
  AND p.status = 'pending'
  AND NOT p.match_ambiguous
  AND (p.lichess_game_id IS NULL OR p.creation_method = 'challenge')
ORDER BY r.number ASC, p.position ASC NULLS LAST, p.created_at ASC;

-- name: RetireUnstartedPairing :execrows
-- evaluate-activity marks an unstarted pairing failed. Guarded like
-- every change to a pairing's status (spec §6.3): still pending, still
-- not ambiguous, and with the game id it was listed with, so a game
-- sync-games attached, a challenge create-games recorded or an admin's
-- Mark failed in the meantime wins.
UPDATE pairings SET status = 'failed'
WHERE id = sqlc.arg(id)
  AND status = 'pending'
  AND NOT match_ambiguous
  AND lichess_game_id IS NOT DISTINCT FROM sqlc.narg(lichess_game_id);

-- name: InsertMissedStart :execrows
-- At most one per player and round: the double-game volunteer who
-- leaves both challenges unanswered has missed one round. A second
-- evaluation of the same pairing inserts nothing, and the caller can
-- tell from the row count.
INSERT INTO missed_starts (user_id, round_id, pairing_id, recorded_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (round_id, user_id) DO NOTHING;

-- name: ListMissedStartsForUser :many
-- Every missed start of a player, oldest first, for the streak rule
-- (internal/activity).
SELECT m.recorded_at, r.number AS round_number
FROM missed_starts m
JOIN rounds r ON r.id = m.round_id
WHERE m.user_id = $1
ORDER BY m.recorded_at ASC;

-- name: GetLatestGameStartForUser :one
-- When the player's latest league game started: a started game ends a
-- run of missed starts (spec §6.4). pgx.ErrNoRows means they have never
-- started one. A plain ORDER BY rather than max(), whose result sqlc
-- would type as never NULL.
SELECT started_at FROM games
WHERE white_user_id = sqlc.arg(user_id) OR black_user_id = sqlc.arg(user_id)
ORDER BY started_at DESC
LIMIT 1;

-- name: SetAutoPaused :execrows
-- The missed-start rule pauses a player (spec §6.4). A player already
-- auto-paused keeps the date they were first paused, and the caller
-- can tell from the row count not to tell them again.
UPDATE player_profiles SET auto_paused_at = $2
WHERE user_id = $1 AND auto_paused_at IS NULL;

-- name: GetChallengeToWithdraw :one
-- cancel-challenge's pairing: a challenge pairing, with whoever sent
-- the challenge. pgx.ErrNoRows for a pairing that is not a challenge.
SELECT
  p.status,
  p.lichess_game_id,
  p.challenger_user_id,
  u.lichess_username AS challenger_username
FROM pairings p
JOIN users u ON u.id = p.challenger_user_id
WHERE p.id = $1;
