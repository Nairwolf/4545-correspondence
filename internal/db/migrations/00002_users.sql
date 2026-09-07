-- +goose Up
CREATE TYPE user_role   AS ENUM ('player','admin');
CREATE TYPE user_status AS ENUM ('pending','approved','rejected','banned');

CREATE TABLE users (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  lichess_username  text NOT NULL,                       -- canonical casing
  lichess_user_id   text NOT NULL UNIQUE,                -- lowercase id from Lichess
  role              user_role   NOT NULL DEFAULT 'player',
  status            user_status NOT NULL DEFAULT 'pending',
  created_at        timestamptz NOT NULL DEFAULT now(),
  approved_at       timestamptz,
  approved_by       uuid REFERENCES users(id),
  rejection_reason  text
);
CREATE UNIQUE INDEX users_username_ci ON users (lower(lichess_username));

CREATE TABLE player_profiles (
  user_id               uuid PRIMARY KEY REFERENCES users(id),
  is_active             boolean NOT NULL DEFAULT true,
  max_concurrent_games  int,                              -- NULL = UNLIMITED (spec §5.8). Never coerce to 0.
  accepts_double_game   boolean NOT NULL DEFAULT true,
  paused_by_admin       boolean NOT NULL DEFAULT false,
  paused_reason         text,
  auto_paused_at        timestamptz,
  timezone              text,
  joined_at             timestamptz NOT NULL DEFAULT now(),
  CHECK (max_concurrent_games IS NULL OR max_concurrent_games > 0)
);

-- +goose Down
DROP TABLE player_profiles;
DROP TABLE users;
DROP TYPE user_status;
DROP TYPE user_role;
