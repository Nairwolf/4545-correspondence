-- +goose Up
CREATE TYPE game_status      AS ENUM ('in_progress','finished');
CREATE TYPE game_result      AS ENUM ('white_win','black_win','draw');
CREATE TYPE game_termination AS ENUM ('mate','resign','clock_flag','draw_agreement','threefold',
                                      'fifty_move','stalemate','insufficient_material','draw_other','unknown');

CREATE TABLE games (
  lichess_game_id      text PRIMARY KEY,
  pairing_id           uuid NOT NULL UNIQUE REFERENCES pairings(id),
  round_number         int NOT NULL,        -- denormalised from rounds.number
  white_user_id        uuid NOT NULL REFERENCES users(id),
  black_user_id        uuid NOT NULL REFERENCES users(id),
  status               game_status NOT NULL,
  result               game_result,         -- NULL while in_progress
  termination          game_termination,    -- NULL while in_progress
  lichess_status       text NOT NULL,       -- raw API status, kept verbatim
  days_per_turn        int,
  eco                  text,
  opening_name         text,
  opening_ply          int,
  white_first_move     text,
  black_first_move     text,
  white_rating_at_game int,                 -- players.white.rating at game creation
  black_rating_at_game int,
  white_accuracy       numeric(5,2),
  black_accuracy       numeric(5,2),
  white_acpl           int,                 -- API gives average CPL, not total
  black_acpl           int,
  white_moves          int,
  black_moves          int,
  started_at           timestamptz NOT NULL,      -- createdAt
  last_move_at         timestamptz NOT NULL,
  finished_at          timestamptz,               -- lastMoveAt when finished
  duration_seconds     bigint,
  raw_payload          jsonb NOT NULL,
  ingested_at          timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now(),
  CHECK (white_user_id <> black_user_id),
  CHECK (status = 'in_progress' OR (result IS NOT NULL AND termination IS NOT NULL AND finished_at IS NOT NULL))
);
CREATE INDEX games_white_finished ON games (white_user_id, finished_at DESC);
CREATE INDEX games_black_finished ON games (black_user_id, finished_at DESC);
CREATE INDEX games_status ON games (status);
CREATE INDEX games_finished_at ON games (finished_at DESC) WHERE status = 'finished';

-- +goose Down
DROP TABLE games;
DROP TYPE game_termination;
DROP TYPE game_result;
DROP TYPE game_status;
