-- Token health (spec §3.3, §7, §8.5; Phase 5): the weekly
-- validate-tokens job and /admin/tokens.

-- name: ListTokensToValidate :many
-- Every stored token validate-tokens tests: those not already known to
-- be revoked (Lichess never brings one back; a re-authorisation stores
-- a new token), of the players who can be paired now or once approved.
-- A rejected or banned user's token is never used, so it is not tested
-- and they are not told it lapsed.
SELECT t.user_id, t.access_token, t.issued_at, u.lichess_username
FROM oauth_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.revoked_at IS NULL
  AND u.status IN ('pending', 'approved')
ORDER BY u.lichess_user_id;

-- name: MarkTokenValidated :execrows
-- Lichess accepted the token. expires_at is refreshed from its answer.
-- The access_token guard leaves alone a row that a sign-in replaced
-- while the job ran: that newer token was not the one tested.
UPDATE oauth_tokens
SET last_validated_at = now(), expires_at = $3
WHERE user_id = $1 AND access_token = $2 AND revoked_at IS NULL;

-- name: MarkTokenRevoked :execrows
-- Lichess refused the token, or it lacks challenge:write. One row
-- means this run found it; none means it was already marked, or
-- replaced by a sign-in since it was read (the access_token guard), so
-- the caller tells the player only once.
UPDATE oauth_tokens
SET revoked_at = now(), last_validated_at = now()
WHERE user_id = $1 AND access_token = $2 AND revoked_at IS NULL;

-- name: ListApprovedPlayerTokens :many
-- /admin/tokens (spec §3.3 item 3, §8.5): every approved player with
-- their token, for the page to keep those without a valid one. A
-- base-table LEFT JOIN: token_issued_at is NOT NULL in a stored row,
-- so NULL means no token at all.
SELECT
  u.id AS user_id,
  u.lichess_username,
  p.is_active,
  p.paused_by_admin,
  p.auto_paused_at,
  t.issued_at AS token_issued_at,
  t.revoked_at AS token_revoked_at,
  t.expires_at AS token_expires_at,
  t.last_validated_at AS token_last_validated_at
FROM users u
JOIN player_profiles p ON p.user_id = u.id
LEFT JOIN oauth_tokens t ON t.user_id = u.id
WHERE u.status = 'approved'
ORDER BY u.lichess_user_id;
