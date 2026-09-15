-- Server-side sessions (spec §8.2). token_hash is sha256 of the cookie
-- value; internal/session owns the hashing and the cookie.

-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: GetSessionUser :one
-- The user behind a live session, plus when the session was last seen
-- so the caller can decide whether to touch it. An expired session is
-- simply not found.
SELECT sqlc.embed(u), s.last_seen_at
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE token_hash = $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteExpiredSessions :execrows
-- Called opportunistically when a session is created — enough
-- housekeeping for a league this size, with no job to schedule.
DELETE FROM sessions WHERE expires_at <= now();
