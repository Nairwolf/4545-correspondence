-- +goose Up

-- Player OAuth tokens (spec §4.1 OAuthToken). One row per user, replaced
-- on every sign-in. There is no refresh_token column: Lichess does not
-- issue refresh tokens (verified against the API, PLAN.md) — a token is
-- long-lived (~1 year) and the remedy for an expired or revoked one is
-- to sign in again.
CREATE TABLE oauth_tokens (
  user_id            uuid PRIMARY KEY REFERENCES users(id),
  access_token       bytea NOT NULL,          -- AES-256-GCM, nonce-prefixed (internal/tokencrypt)
  scopes             text[] NOT NULL,         -- as granted: {'challenge:write'} for now, 'msg:write' from Phase 5
  issued_at          timestamptz NOT NULL DEFAULT now(),
  expires_at         timestamptz,             -- issued_at + expires_in
  revoked_at         timestamptz,             -- set by the token-health probe (Phase 3/5)
  last_validated_at  timestamptz NOT NULL DEFAULT now()
);

-- Server-side sessions (spec §8.2). The cookie holds a random id; only
-- its sha256 is stored, so reading the table does not yield a usable
-- credential.
CREATE TABLE sessions (
  token_hash    bytea PRIMARY KEY,
  user_id       uuid NOT NULL REFERENCES users(id),
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);

ALTER TABLE users
  -- Raw GET /api/account body from the last sign-in: the registration
  -- queue's signals (account age, rated games, closed/TOS flags) are
  -- read from here at render time — same philosophy as games.raw_payload.
  ADD COLUMN lichess_profile             jsonb,
  ADD COLUMN lichess_profile_fetched_at  timestamptz,
  -- When the applicant ticked the fair-play agreement on the join page.
  -- NULL for users created by seed-players.
  ADD COLUMN fair_play_agreed_at         timestamptz;

-- +goose Down
ALTER TABLE users
  DROP COLUMN fair_play_agreed_at,
  DROP COLUMN lichess_profile_fetched_at,
  DROP COLUMN lichess_profile;
DROP TABLE sessions;
DROP TABLE oauth_tokens;
