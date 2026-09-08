-- name: UpsertGame :one
-- Idempotent, keyed on lichess_game_id (spec §3.4). Every column that can
-- legitimately change across re-ingestion (an in_progress game finishing,
-- or the same finished game re-fetched) is updated to the new value;
-- identity columns (the id itself, which pairing/round/players it
-- belongs to, when it started) never change for a given game id and are
-- only ever set on insert. Re-running with byte-identical input changes
-- nothing but updated_at, matching the spec's own idempotency test.
INSERT INTO games (
  lichess_game_id, pairing_id, round_number, white_user_id, black_user_id,
  status, result, termination, lichess_status, days_per_turn, eco, opening_name, opening_ply,
  white_first_move, black_first_move, white_rating_at_game, black_rating_at_game,
  white_accuracy, black_accuracy, white_acpl, black_acpl, white_moves, black_moves,
  started_at, last_move_at, finished_at, duration_seconds, raw_payload
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
  $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28
)
ON CONFLICT (lichess_game_id) DO UPDATE SET
  status               = EXCLUDED.status,
  result               = EXCLUDED.result,
  termination          = EXCLUDED.termination,
  lichess_status       = EXCLUDED.lichess_status,
  days_per_turn        = EXCLUDED.days_per_turn,
  eco                  = EXCLUDED.eco,
  opening_name         = EXCLUDED.opening_name,
  opening_ply          = EXCLUDED.opening_ply,
  white_first_move     = EXCLUDED.white_first_move,
  black_first_move     = EXCLUDED.black_first_move,
  white_rating_at_game = EXCLUDED.white_rating_at_game,
  black_rating_at_game = EXCLUDED.black_rating_at_game,
  white_accuracy       = EXCLUDED.white_accuracy,
  black_accuracy       = EXCLUDED.black_accuracy,
  white_acpl           = EXCLUDED.white_acpl,
  black_acpl           = EXCLUDED.black_acpl,
  white_moves          = EXCLUDED.white_moves,
  black_moves          = EXCLUDED.black_moves,
  last_move_at         = EXCLUDED.last_move_at,
  finished_at          = EXCLUDED.finished_at,
  duration_seconds     = EXCLUDED.duration_seconds,
  raw_payload          = EXCLUDED.raw_payload,
  updated_at           = now()
RETURNING *;

-- name: DeleteGame :exec
-- Only for the rare case a previously in_progress game later turns out
-- to be aborted/noStart on re-check — those are never stored (spec
-- §4.1), so any row that was written before that was known removes
-- itself here rather than lingering.
DELETE FROM games WHERE lichess_game_id = $1;

-- name: ListFinishedGamesForUser :many
-- All finished games for one player, most recent first — the raw
-- material for internal/scoring, which is pure and takes exactly this
-- shape (games already resolved to one player's perspective) as input.
SELECT * FROM games
WHERE status = 'finished' AND (white_user_id = $1 OR black_user_id = $1)
ORDER BY finished_at DESC;

-- name: CountOngoingGamesForUser :one
-- spec §5.8's ongoing_games(player).
SELECT count(*) FROM games
WHERE status = 'in_progress' AND (white_user_id = $1 OR black_user_id = $1);
