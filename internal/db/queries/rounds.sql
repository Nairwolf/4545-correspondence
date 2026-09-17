-- Queries backing the pairing engine (spec §6.2), the round state
-- machine (§6.1), admin round management (§8.5) and the dashboard's
-- "this week" block (§8.3). Phase 4, 2026-09-17 — see PLAN.md.

-- name: CountInFlightGamesForUser :one
-- spec §5.8, amended 2026-09-17 (decision 7): games in progress plus
-- pending pairings of PUBLISHED rounds with no game ingested yet — a
-- pairing the player has been told to start is a game in flight before
-- Lichess knows about it. This replaces the narrower
-- CountOngoingGamesForUser everywhere it was read (the pairing pool,
-- standings, the dashboard), so the three can never disagree.
SELECT
  (SELECT count(*) FROM games g
    WHERE g.status = 'in_progress' AND (g.white_user_id = $1 OR g.black_user_id = $1))
  +
  (SELECT count(*) FROM pairings p
    JOIN rounds r ON r.id = p.round_id
    WHERE r.state = 'published'
      AND p.lichess_game_id IS NULL
      AND p.status IN ('pending', 'created')
      AND (p.white_user_id = $1 OR p.black_user_id = $1)
  ) AS in_flight;

-- name: ListPairingPool :many
-- Every approved user with everything the pairing engine needs to
-- decide eligibility and capacity (spec §5.7, §5.8), in one statement
-- so it can be diffed against Pairing_Maker_Backend. Ordered
-- power_rating DESC, user_id ASC — the engine's own sort order (§6.2
-- step 2). LEFT JOIN on player_standings: a seeded or just-approved
-- player can have no standing row yet (`ic seed-players` never
-- computes one), and losing them from the pool silently would be
-- exactly the kind of NULL-coercion bug this codebase tests against —
-- the caller must treat a NULL power_rating as "no standing yet",
-- never as zero. bye_count and double_count (published rounds only,
-- like every other history read) are safe to compute here because
-- COALESCE keeps them non-null; the ROUND NUMBER of a player's last
-- bye or double game is deliberately NOT joined in here — sqlc cannot
-- prove that column's nullability once it comes from a derived table,
-- and guessing NOT NULL on it would be the exact NULL-as-zero bug
-- class this file elsewhere goes out of its way to avoid. The rounds
-- service instead reads it per player via GetLastByeForUser /
-- GetLastDoubleGameForUser (pgx.ErrNoRows = never), which are simple
-- enough for sqlc to type correctly. The pool is small (tens to a few
-- hundred rows) and this runs once a week, so the extra round trips
-- cost nothing worth avoiding at the price of a wrong type.
SELECT
  u.id AS user_id,
  p.is_active,
  p.paused_by_admin,
  p.auto_paused_at,
  p.max_concurrent_games,
  p.accepts_double_game,
  s.power_rating,
  s.color_score,
  COALESCE(byecount.bye_count, 0)::int AS bye_count,
  COALESCE(doublecount.double_count, 0)::int AS double_count
FROM users u
JOIN player_profiles p ON p.user_id = u.id
LEFT JOIN player_standings s ON s.user_id = u.id
LEFT JOIN (
  SELECT b.user_id, count(*) AS bye_count
  FROM byes b JOIN rounds r ON r.id = b.round_id
  WHERE r.state = 'published'
  GROUP BY b.user_id
) byecount ON byecount.user_id = u.id
LEFT JOIN (
  SELECT d.user_id, count(*) AS double_count
  FROM double_games d JOIN rounds r ON r.id = d.round_id
  WHERE r.state = 'published'
  GROUP BY d.user_id
) doublecount ON doublecount.user_id = u.id
WHERE u.status = 'approved'
ORDER BY s.power_rating DESC NULLS LAST, u.id ASC;

-- name: ListRecentOpponents :many
-- Every pairing of the last `avoid_recent_rounds` PUBLISHED rounds
-- before `round_number` (spec §6.2 step 3's repeat_cost). Selecting the
-- last N published round NUMBERS first, rather than
-- "number > current - avoid_recent_rounds", is deliberate: a cancelled
-- round leaves a gap in the sequence, and arithmetic subtraction would
-- silently shrink the window by counting the gap as if a round had
-- happened. A pairing that never became a game (cancelled or failed)
-- is not "met" and is excluded.
WITH recent_rounds AS (
  SELECT rr0.number FROM rounds rr0
  WHERE rr0.state = 'published' AND rr0.number < sqlc.arg(round_number)
  ORDER BY rr0.number DESC
  LIMIT sqlc.arg(window_size)
)
SELECT
  p.white_user_id,
  p.black_user_id,
  r.number AS round_number
FROM pairings p
JOIN rounds r ON r.id = p.round_id
JOIN recent_rounds rr ON rr.number = r.number
WHERE p.status NOT IN ('cancelled', 'failed');

-- name: NextRoundNumber :one
-- spec §14.2: a generated round continues the sheet's sequence. A
-- cancelled round gives its number back (rounds_number_live), so this
-- is simply one past the highest number any live round still holds.
SELECT COALESCE(MAX(number), 0) + 1 AS next_number
FROM rounds WHERE state <> 'cancelled';

-- name: GetDraftRound :one
-- At most one row can ever match (rounds_one_draft).
SELECT * FROM rounds WHERE state = 'draft';

-- name: GetRoundByID :one
SELECT * FROM rounds WHERE id = $1;

-- name: GetRoundForUpdate :one
-- Locks the round row. Every publish/cancel/regenerate/edit path takes
-- this lock first, so Phase 5 can put a Lichess call between the lock
-- and the state change without changing this shape.
SELECT * FROM rounds WHERE id = $1 FOR UPDATE;

-- name: ListRounds :many
-- Admin round list (spec §8.5).
SELECT
  r.*,
  (SELECT count(*) FROM pairings p WHERE p.round_id = r.id)::int AS pairing_count,
  (SELECT count(*) FROM byes b WHERE b.round_id = r.id)::int AS bye_count
FROM rounds r
ORDER BY r.number DESC;

-- name: ListPairingsForRound :many
-- The draft/round view's pairing table (spec §8.5): usernames for
-- display, and each player's CURRENT power rating alongside the
-- rating_gap stored at generation time, so an admin can see how much
-- the pool has moved since.
SELECT
  p.*,
  wu.lichess_username AS white_username,
  bu.lichess_username AS black_username,
  ws.power_rating AS white_power_rating,
  bs.power_rating AS black_power_rating
FROM pairings p
JOIN users wu ON wu.id = p.white_user_id
JOIN users bu ON bu.id = p.black_user_id
LEFT JOIN player_standings ws ON ws.user_id = p.white_user_id
LEFT JOIN player_standings bs ON bs.user_id = p.black_user_id
WHERE p.round_id = $1
ORDER BY p.position ASC NULLS LAST, p.created_at ASC;

-- name: ListExclusionsForRound :many
SELECT re.*, u.lichess_username
FROM round_exclusions re
JOIN users u ON u.id = re.user_id
WHERE re.round_id = $1
ORDER BY re.reason, u.lichess_username;

-- name: ListByesForRound :many
SELECT b.*, u.lichess_username
FROM byes b
JOIN users u ON u.id = b.user_id
WHERE b.round_id = $1;

-- name: ListDoubleGamesForRound :many
SELECT d.*, u.lichess_username
FROM double_games d
JOIN users u ON u.id = d.user_id
WHERE d.round_id = $1;

-- name: CreateGeneratedRound :one
INSERT INTO rounds (
  number, state, publish_at, published_at, pair_at, generated_by,
  pool_size, odd_pool, repeat_pairings, settings_used
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: RefreshGeneratedRound :one
-- Regeneration (spec §8.5) runs the engine again into the round row
-- that already exists, so an admin who rejects a draft keeps its
-- number and its review window: only the generation's own facts are
-- replaced. Guarded on state='draft' like every other edit path.
UPDATE rounds SET
  generated_at = now(), generated_by = $2, pool_size = $3, odd_pool = $4,
  repeat_pairings = $5, settings_used = $6
WHERE id = $1 AND state = 'draft'
RETURNING *;

-- name: InsertGeneratedPairing :one
-- Every engine-generated pairing starts manual_external/pending — the
-- shape `import-pairings` already produces (spec §6.3: Phase 4 makes
-- no Lichess call; Phase 5 overwrites creation_method at publish).
INSERT INTO pairings (
  round_id, white_user_id, black_user_id, creation_method, status,
  position, rating_gap, color_penalty, repeat_of_round
) VALUES (
  $1, $2, $3, 'manual_external', 'pending', $4, $5, $6, $7
)
RETURNING *;

-- name: InsertBye :one
INSERT INTO byes (round_id, user_id) VALUES ($1, $2) RETURNING *;

-- name: InsertDoubleGame :one
INSERT INTO double_games (round_id, user_id) VALUES ($1, $2) RETURNING *;

-- name: DeleteDoubleGame :execrows
-- Used when an admin removes one of the volunteer's two pairings
-- (spec §8.5): they are no longer playing two games this round.
DELETE FROM double_games WHERE round_id = $1 AND user_id = $2;

-- name: InsertRoundExclusion :one
INSERT INTO round_exclusions (round_id, user_id, reason, ongoing_games, max_concurrent_games)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: PublishRound :one
-- Guarded on state='draft' so every publish path (the scheduled job,
-- the hourly sweep, an admin's "publish now") is the same idempotent
-- update: whichever runs first wins, the rest affect zero rows.
-- pair_at is set to the same moment as published_at (spec §6.3): an
-- early publish must never leave §7.3's pair_at-1day cutoff ahead of
-- the games players go on to create.
UPDATE rounds SET state = 'published', published_at = $2, pair_at = $2
WHERE id = $1 AND state = 'draft'
RETURNING *;

-- name: CancelRound :one
-- Drafts only (a published round may already have games; cancelling
-- one is Phase 5, spec §6.3).
UPDATE rounds SET state = 'cancelled', notes = $2
WHERE id = $1 AND state = 'draft'
RETURNING *;

-- name: CancelPairingsForRound :exec
UPDATE pairings SET status = 'cancelled' WHERE round_id = $1;

-- name: DeleteRoundPairings :exec
DELETE FROM pairings WHERE round_id = $1;

-- name: DeleteRoundByes :exec
DELETE FROM byes WHERE round_id = $1;

-- name: DeleteRoundDoubleGames :exec
DELETE FROM double_games WHERE round_id = $1;

-- name: DeleteRoundExclusions :exec
DELETE FROM round_exclusions WHERE round_id = $1;

-- name: FlipPairingColours :one
-- Swaps the two players' colours in place; the caller nulls the
-- diagnostic columns (they no longer describe the stored colours) and
-- sets edited_by.
UPDATE pairings SET
  white_user_id = $2, black_user_id = $3, edited_by = $4,
  rating_gap = NULL, color_penalty = NULL, repeat_of_round = NULL
WHERE id = $1
RETURNING *;

-- name: DeletePairing :one
-- Returns the deleted row so the caller can write the round_exclusion
-- and, if this was one of the volunteer's two games, drop their
-- double_games record.
DELETE FROM pairings WHERE id = $1 RETURNING *;

-- name: InsertEditedPairing :one
-- Used for a swap between two pairings: the caller deletes both old
-- rows and inserts two new ones with this query, all in one
-- transaction, rather than UPDATEing in place — updating in place can
-- transiently collide with pairings_round_white_black when the target
-- (round, white, black) tuple is what a sibling row is about to
-- vacate.
INSERT INTO pairings (round_id, white_user_id, black_user_id, creation_method, status, edited_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListDraftsDue :many
-- The hourly publish-round-sweep's input (spec §7): a draft whose
-- review window has already ended, for the case its scheduled
-- publish-round job was lost to a restart or a failed enqueue, or was
-- never scheduled at all (a round generated from the CLI, which has no
-- river client to enqueue with). "Now" is the caller's, like every
-- other timestamp the rounds service writes: now() here would be the
-- transaction's start time, which is not the same instant and is not
-- something a test can control.
SELECT * FROM rounds WHERE state = 'draft' AND publish_at <= $1;

-- name: GetLatestPublishedRound :one
SELECT * FROM rounds WHERE state = 'published' ORDER BY number DESC LIMIT 1;

-- name: GetPairingForUserInRound :one
-- The dashboard's "this week" block (spec §8.3). sqlc.arg names the
-- parameter for what it actually is here — the player asking, who may
-- be either colour — rather than the misleading "white_user_id" its
-- first use in the CASE would otherwise suggest.
SELECT
  p.*,
  (CASE WHEN p.white_user_id = sqlc.arg(user_id) THEN bu.lichess_username ELSE wu.lichess_username END)::text AS opponent_username,
  (p.white_user_id = sqlc.arg(user_id)) AS is_white
FROM pairings p
JOIN users wu ON wu.id = p.white_user_id
JOIN users bu ON bu.id = p.black_user_id
WHERE p.round_id = $1 AND (p.white_user_id = sqlc.arg(user_id) OR p.black_user_id = sqlc.arg(user_id));

-- name: GetExclusionForUserInRound :one
SELECT * FROM round_exclusions WHERE round_id = $1 AND user_id = $2;

-- name: CountByesForUser :one
SELECT count(*) FROM byes b
JOIN rounds r ON r.id = b.round_id
WHERE b.user_id = $1 AND r.state = 'published';

-- name: CountDoubleGamesForUser :one
SELECT count(*) FROM double_games d
JOIN rounds r ON r.id = d.round_id
WHERE d.user_id = $1 AND r.state = 'published';

-- name: GetLastByeForUser :one
-- pgx.ErrNoRows means "never had a bye" — the engine's rotation rule
-- (spec §6.2 step 6b) treats that as having waited forever.
SELECT b.*, r.number AS round_number
FROM byes b
JOIN rounds r ON r.id = b.round_id
WHERE b.user_id = $1 AND r.state = 'published'
ORDER BY r.number DESC
LIMIT 1;

-- name: GetLastDoubleGameForUser :one
-- pgx.ErrNoRows means "never volunteered" — same rotation rule as
-- GetLastByeForUser, for step 6a's volunteer instead of step 6b's bye.
SELECT d.*, r.number AS round_number
FROM double_games d
JOIN rounds r ON r.id = d.round_id
WHERE d.user_id = $1 AND r.state = 'published'
ORDER BY r.number DESC
LIMIT 1;
