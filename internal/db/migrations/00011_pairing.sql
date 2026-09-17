-- +goose Up
-- Phase 4 (spec §12): the pairing engine's schema. See PLAN.md's
-- "Schema" section for the reasoning behind each addition.

CREATE TYPE exclusion_reason AS ENUM (
  'at_capacity', 'inactive', 'paused', 'auto_paused', 'no_valid_token',
  'bye', 'pending_approval', 'removed_by_admin'
);
CREATE TYPE odd_pool_outcome AS ENUM ('even', 'double_game', 'bye');

-- Append-only explanation/audit tables (spec §4.1): the pairing
-- algorithm never reads them back; they exist so "why didn't I get a
-- game this week?" and bye/double-game rotation history can be
-- answered directly, without re-running the engine.
CREATE TABLE round_exclusions (
  id                    bigserial PRIMARY KEY,
  round_id              int NOT NULL REFERENCES rounds(id),
  user_id               uuid NOT NULL REFERENCES users(id),
  reason                exclusion_reason NOT NULL,
  ongoing_games         int,   -- populated when reason = at_capacity
  max_concurrent_games  int,   -- populated when reason = at_capacity
  created_at            timestamptz NOT NULL DEFAULT now(),
  UNIQUE (round_id, user_id)
);

CREATE TABLE byes (
  id          bigserial PRIMARY KEY,
  round_id    int NOT NULL REFERENCES rounds(id),
  user_id     uuid NOT NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (round_id, user_id)
);

CREATE TABLE double_games (
  id          bigserial PRIMARY KEY,
  round_id    int NOT NULL REFERENCES rounds(id),
  user_id     uuid NOT NULL REFERENCES users(id),
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (round_id, user_id)
);

-- A cancelled round's number is given back (a regenerated or
-- re-cancelled round can reuse it, so the sequence has no gaps), so
-- uniqueness is only enforced among rounds that still hold their
-- number. rounds_one_draft makes "at most one draft at a time" (spec
-- §6.1) a database fact rather than a check-then-insert race between
-- an admin's "Generate now" and the scheduled job.
ALTER TABLE rounds
  DROP CONSTRAINT rounds_number_key,
  ADD COLUMN pool_size       int,
  ADD COLUMN odd_pool        odd_pool_outcome,
  ADD COLUMN repeat_pairings int,
  ADD COLUMN settings_used   jsonb;

CREATE UNIQUE INDEX rounds_number_live ON rounds (number) WHERE state <> 'cancelled';
CREATE UNIQUE INDEX rounds_one_draft ON rounds ((true)) WHERE state = 'draft';

-- Per-pairing diagnostics (spec §8.5): stored at generation time
-- because standings move afterwards, so the admin view must show what
-- the engine actually saw. An admin edit (flip/swap/remove) nulls
-- these three columns on the touched row rather than leaving numbers
-- that no longer hold.
ALTER TABLE pairings
  ADD COLUMN position        int,
  ADD COLUMN rating_gap      int,
  ADD COLUMN color_penalty   int,
  ADD COLUMN repeat_of_round int;

-- +goose Down
ALTER TABLE pairings
  DROP COLUMN position,
  DROP COLUMN rating_gap,
  DROP COLUMN color_penalty,
  DROP COLUMN repeat_of_round;

DROP INDEX rounds_one_draft;
DROP INDEX rounds_number_live;

ALTER TABLE rounds
  DROP COLUMN pool_size,
  DROP COLUMN odd_pool,
  DROP COLUMN repeat_pairings,
  DROP COLUMN settings_used,
  ADD CONSTRAINT rounds_number_key UNIQUE (number);

DROP TABLE double_games;
DROP TABLE byes;
DROP TABLE round_exclusions;

DROP TYPE odd_pool_outcome;
DROP TYPE exclusion_reason;
