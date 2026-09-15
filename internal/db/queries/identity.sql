-- Registration and sign-in (spec §8.2) and the admin registration queue
-- (spec §8.5). Every status change here is paired with an audit_log row
-- by the caller.

-- name: CreatePendingUser :one
-- A new applicant arriving through /join: pending until an admin
-- approves. The Lichess profile is stored as fetched so the queue can
-- show its signals without another API call.
INSERT INTO users (
  lichess_username, lichess_user_id, status,
  lichess_profile, lichess_profile_fetched_at, fair_play_agreed_at
) VALUES (
  $1, $2, 'pending', $3, now(), now()
)
RETURNING *;

-- name: UpdateUserLichessProfile :exec
-- Refreshed on every sign-in.
UPDATE users
SET lichess_profile = $2, lichess_profile_fetched_at = now()
WHERE id = $1;

-- name: RenameUser :exec
-- Lichess lets users change the casing of their name (and, rarely,
-- the name itself); the id is stable, so we follow the id and update
-- the display name.
UPDATE users SET lichess_username = $2 WHERE id = $1;

-- name: PromoteToAdmin :exec
UPDATE users SET role = 'admin' WHERE id = $1;

-- name: ApproveUser :one
-- From pending or rejected (an admin may change their mind). A NULL
-- approved_by means the system did it — the bootstrap-admin rule.
-- Returns no row when the user is not in an approvable state, so a
-- stale form submission is a no-op the caller can detect.
UPDATE users
SET status = 'approved', approved_at = now(), approved_by = $2, rejection_reason = NULL
WHERE id = $1 AND status IN ('pending', 'rejected')
RETURNING *;

-- name: RejectUser :one
UPDATE users
SET status = 'rejected', rejection_reason = $2
WHERE id = $1 AND status = 'pending'
RETURNING *;

-- name: ListUsersByStatus :many
-- The registration queue's tabs: oldest application first, so the
-- person who has waited longest is at the top.
SELECT * FROM users WHERE status = $1 ORDER BY created_at ASC;

-- name: ListLookalikeCandidates :many
-- Every name an applicant's could be confused with. The comparison
-- itself is in Go (internal/web), where it can be unit-tested.
SELECT id, lichess_username FROM users WHERE status <> 'rejected';

-- name: UpsertOAuthToken :exec
-- Every sign-in replaces the stored token: the new one is what Lichess
-- currently honours, and clearing revoked_at is what makes a
-- re-authorisation (spec §3.1) take effect.
INSERT INTO oauth_tokens (user_id, access_token, scopes, expires_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE SET
  access_token      = EXCLUDED.access_token,
  scopes            = EXCLUDED.scopes,
  issued_at         = now(),
  expires_at        = EXCLUDED.expires_at,
  revoked_at        = NULL,
  last_validated_at = now();

-- name: GetOAuthToken :one
SELECT * FROM oauth_tokens WHERE user_id = $1;
