-- name: GetRoundByNumber :one
-- A cancelled round's number is no longer unique (rounds_number_live,
-- spec §14.2, Phase 4): excluding cancelled rows keeps this :one query
-- honest instead of returning an arbitrary one of two matches.
SELECT * FROM rounds WHERE number = $1 AND state <> 'cancelled';

-- name: CreateRound :one
-- published_at is accepted explicitly (rather than left to default)
-- because import-pairings creates rounds that are already published —
-- there is no review window to have happened for a round the sheet
-- already ran.
INSERT INTO rounds (number, state, publish_at, published_at, pair_at, generated_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListPairingsToRecheck :many
-- Every pairing whose Lichess game is worth re-fetching (spec §7.3 step
-- 1): it already has a game id and hasn't reached a terminal pairing
-- status. This deliberately covers two cases with one query: a pairing
-- already ingested as in_progress (its games row exists, we want the
-- latest state), AND a pairing whose game id was already known at
-- import time — e.g. from the CSV's optional game_id column — but has
-- never been fetched at all yet, so no games row exists for it.
-- Matching (ListUnmatchedPairings) only ever handles the OTHER case:
-- no game id known yet. Restricted to PUBLISHED rounds (Phase 4,
-- 2026-09-17): a draft's pairings are not real yet and must never
-- reach Lichess.
SELECT
  p.id AS pairing_id,
  p.lichess_game_id,
  r.number AS round_number,
  p.white_user_id,
  p.black_user_id
FROM pairings p
JOIN rounds r ON r.id = p.round_id
WHERE r.state = 'published'
  AND p.lichess_game_id IS NOT NULL
  AND p.status NOT IN ('completed', 'failed', 'cancelled');

-- name: ListUnmatchedPairings :many
-- Pairings sync-games must try to match against a Lichess game (spec
-- §7.3 step 2): no game id yet, still pending, and not already flagged
-- for an admin to resolve. Joins in exactly what matching.Match needs
-- (the round's pair_at, and both players' Lichess ids) so the caller
-- makes no further per-pairing queries. Restricted to PUBLISHED rounds
-- (Phase 4, 2026-09-17): without this, the pair_at-1day cutoff would
-- let the hourly job attach a real Lichess game the two draft opponents
-- happen to have started into a round that has not been published yet,
-- and the resulting games row would block the draft's regeneration.
SELECT
  p.id AS pairing_id,
  p.round_id,
  r.number AS round_number,
  r.pair_at,
  p.white_user_id,
  p.black_user_id,
  wu.lichess_user_id AS white_lichess_id,
  bu.lichess_user_id AS black_lichess_id
FROM pairings p
JOIN rounds r ON r.id = p.round_id
JOIN users wu ON wu.id = p.white_user_id
JOIN users bu ON bu.id = p.black_user_id
WHERE r.state = 'published'
  AND p.lichess_game_id IS NULL
  AND p.status = 'pending'
  AND NOT p.match_ambiguous;

-- name: ListAttachedGameIDs :many
-- Every game id already claimed by some pairing — matching.Match's
-- takenGameIDs, so a game already ingested for one pairing is never
-- also matched to a different one.
SELECT lichess_game_id FROM pairings WHERE lichess_game_id IS NOT NULL;

-- name: AttachGameToPairing :exec
UPDATE pairings SET lichess_game_id = $2, status = $3 WHERE id = $1;

-- name: MarkPairingAmbiguous :exec
UPDATE pairings SET match_ambiguous = true WHERE id = $1;

-- name: MarkPairingFailed :exec
UPDATE pairings SET status = 'failed' WHERE id = $1;

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
