-- +goose Up
CREATE TYPE round_state    AS ENUM ('draft','published','cancelled');
CREATE TYPE round_source   AS ENUM ('schedule','manual','imported');
CREATE TYPE pairing_method AS ENUM ('bulk','challenge','manual_external');
CREATE TYPE pairing_status AS ENUM ('pending','created','in_progress','completed','failed','cancelled');

CREATE TABLE rounds (
  id               serial PRIMARY KEY,
  number           int NOT NULL UNIQUE,
  state            round_state NOT NULL,
  generated_at     timestamptz NOT NULL DEFAULT now(),
  publish_at       timestamptz NOT NULL,
  published_at     timestamptz,
  pair_at          timestamptz NOT NULL,        -- for imported rounds: the sheet's pairing date
  generated_by     round_source NOT NULL,
  bulk_pairing_id  text,
  notes            text
);

CREATE TABLE pairings (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  round_id         int NOT NULL REFERENCES rounds(id),
  white_user_id    uuid NOT NULL REFERENCES users(id),
  black_user_id    uuid NOT NULL REFERENCES users(id),
  creation_method  pairing_method NOT NULL,
  lichess_game_id  text UNIQUE,
  status           pairing_status NOT NULL DEFAULT 'pending',
  match_ambiguous  boolean NOT NULL DEFAULT false,
  created_at       timestamptz NOT NULL DEFAULT now(),
  edited_by        uuid REFERENCES users(id),
  CHECK (white_user_id <> black_user_id)
);
CREATE INDEX pairings_round ON pairings (round_id);
CREATE INDEX pairings_unmatched ON pairings (status) WHERE lichess_game_id IS NULL;
-- one imported/manual pairing per (round, white, black): the idempotency key
-- for `ic import-pairings` re-runs (spec: "re-running the same file is a no-op").
CREATE UNIQUE INDEX pairings_round_white_black ON pairings (round_id, white_user_id, black_user_id);

-- +goose Down
DROP TABLE pairings;
DROP TABLE rounds;
DROP TYPE pairing_status;
DROP TYPE pairing_method;
DROP TYPE round_source;
DROP TYPE round_state;
