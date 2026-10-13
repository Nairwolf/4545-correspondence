-- +goose Up
-- Phase 5 step 5 (spec §6.3): how a round's games were created.
--
-- manual   published under pairing.game_creation = manual: players
--          start every game by hand. Every round before Phase 5, and
--          every draft until it is published.
-- creating published under lichess: a create-games job is queued or
--          running.
-- created  create-games finished. Pairings it could not create (no
--          working token on either side) are left to be started by
--          hand.
-- gave_up  create-games failed its last attempt, or found game
--          creation switched off: the pairings left are started by hand.
CREATE TYPE game_creation_state AS ENUM ('manual', 'creating', 'created', 'gave_up');

ALTER TABLE rounds
  ADD COLUMN game_creation    game_creation_state NOT NULL DEFAULT 'manual',
  ADD COLUMN games_created_at timestamptz;

-- +goose Down
ALTER TABLE rounds
  DROP COLUMN games_created_at,
  DROP COLUMN game_creation;

DROP TYPE game_creation_state;
