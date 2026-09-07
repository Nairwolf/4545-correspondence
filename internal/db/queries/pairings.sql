-- name: GetRoundByNumber :one
SELECT * FROM rounds WHERE number = $1;

-- name: CreateRound :one
-- published_at is accepted explicitly (rather than left to default)
-- because import-pairings creates rounds that are already published —
-- there is no review window to have happened for a round the sheet
-- already ran.
INSERT INTO rounds (number, state, publish_at, published_at, pair_at, generated_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: UpsertManualPairing :one
-- Idempotent: import-pairings can be re-run on the same file safely,
-- matched via the pairings_round_white_black unique index created for
-- exactly this purpose. On conflict, lichess_game_id is updated only if
-- it wasn't already set — a re-import must never blow away a game id
-- that sync-games has since discovered and attached (spec §7.3), and
-- status is left untouched for the same reason.
INSERT INTO pairings (round_id, white_user_id, black_user_id, creation_method, lichess_game_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (round_id, white_user_id, black_user_id) DO UPDATE SET
  lichess_game_id = COALESCE(pairings.lichess_game_id, EXCLUDED.lichess_game_id)
RETURNING *;
