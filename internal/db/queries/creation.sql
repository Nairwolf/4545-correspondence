-- Creating a round's games on Lichess (spec §6.3; Phase 5 step 5): the
-- create-games job, its retry, and the round page's game-creation line.

-- name: StartGameCreation :one
-- Records how a round just published will have its games created:
-- 'creating' when the site creates them (its create-games job is queued
-- in the same transaction), 'manual' when players start them by hand.
-- Guarded on the round being published, so a draft never carries a
-- game-creation state of its own.
UPDATE rounds SET game_creation = $2
WHERE id = $1 AND state = 'published'
RETURNING *;

-- name: ClaimGameCreation :one
-- An admin's retry (spec §8.5): a round published under lichess goes
-- back to 'creating' while its retry job is queued. A round published
-- under manual is never claimed: it records that its players were told
-- to start every game by hand.
UPDATE rounds SET game_creation = 'creating'
WHERE id = $1 AND state = 'published' AND game_creation <> 'manual'
RETURNING *;

-- name: FinishGameCreation :one
-- create-games' last step. Guarded on 'creating', so a run that finds
-- the round already finished or given up changes nothing. Called with
-- 'created' (and the time) or 'gave_up' (and NULL).
UPDATE rounds SET game_creation = sqlc.arg(outcome), games_created_at = sqlc.arg(games_created_at)
WHERE id = sqlc.arg(id) AND game_creation = 'creating'
RETURNING *;

-- name: SetRoundBulkPairing :exec
-- The first bulk that created any of the round's games: the one the
-- round page shows. A retry's later bulk is named in its job run only.
UPDATE rounds SET bulk_pairing_id = COALESCE(bulk_pairing_id, $2) WHERE id = $1;

-- name: ListPairingsToCreate :many
-- Step 1 of create-games: the round's pairings still waiting for a
-- game — generated as manual_external, still pending, no game id —
-- with what the job needs to create them. One flagged ambiguous is
-- left out: sync-games found two games for it, and an admin must pick
-- one rather than the league create a third.
SELECT
  p.id AS pairing_id,
  p.white_user_id,
  p.black_user_id,
  wu.lichess_user_id AS white_lichess_id,
  bu.lichess_user_id AS black_lichess_id,
  wu.lichess_username AS white_username,
  bu.lichess_username AS black_username
FROM pairings p
JOIN users wu ON wu.id = p.white_user_id
JOIN users bu ON bu.id = p.black_user_id
WHERE p.round_id = $1
  AND p.creation_method = 'manual_external'
  AND p.status = 'pending'
  AND p.lichess_game_id IS NULL
  AND NOT p.match_ambiguous
ORDER BY p.position ASC NULLS LAST, p.created_at ASC;

-- name: ListRoundPlayerTokens :many
-- The stored tokens of every player of the round's pairings, for the
-- probe that precedes a bulk (spec §6.3 step 3). A player with no row
-- has no token.
SELECT t.*
FROM oauth_tokens t
WHERE t.user_id IN (
  SELECT pw.white_user_id FROM pairings pw WHERE pw.round_id = $1
  UNION
  SELECT pb.black_user_id FROM pairings pb WHERE pb.round_id = $1
);

-- name: RecordCreatedGame :execrows
-- A game Lichess created for a pairing. Guarded like every change to a
-- pairing's status (spec §6.3): a pairing an admin marked failed while
-- the job ran stays failed, and none is ever given a second game.
UPDATE pairings
SET creation_method = $2, status = $3, lichess_game_id = $4
WHERE id = $1 AND status = 'pending' AND lichess_game_id IS NULL;

-- name: RecordChallenge :execrows
-- A challenge sent for a pairing (spec §6.3 step 6). Its id is the
-- game's once the opponent accepts (§3.4), so it is stored as the game
-- id, and the pairing stays pending until sync-games finds the game by
-- that id. Guarded like RecordCreatedGame.
UPDATE pairings
SET creation_method = 'challenge', challenger_user_id = $2, lichess_game_id = $3
WHERE id = $1 AND status = 'pending' AND lichess_game_id IS NULL;
