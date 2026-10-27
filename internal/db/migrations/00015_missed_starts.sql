-- +goose Up
-- Phase 5 step 7 (spec §6.4): missed starts and auto-pause.
--
-- A missed start is recorded only for a challenge pairing nobody
-- accepted, for the player who had to accept it (pairings'
-- challenger_user_id names the other one). One per player and round:
-- the double-game volunteer who leaves both of a round's challenges
-- unanswered has missed one round, not two. Rows should be rare; a
-- full table means token health is failing, not players (§4.1.1).
CREATE TABLE missed_starts (
  id           bigserial PRIMARY KEY,
  user_id      uuid NOT NULL REFERENCES users(id),
  round_id     int NOT NULL REFERENCES rounds(id),
  pairing_id   uuid NOT NULL REFERENCES pairings(id),
  recorded_at  timestamptz NOT NULL,
  UNIQUE (round_id, user_id)
);

-- The player's last "Resume quest": missed starts recorded before it no
-- longer count towards the next auto-pause.
ALTER TABLE player_profiles ADD COLUMN resumed_at timestamptz;

-- +goose Down
ALTER TABLE player_profiles DROP COLUMN resumed_at;
DROP TABLE missed_starts;
