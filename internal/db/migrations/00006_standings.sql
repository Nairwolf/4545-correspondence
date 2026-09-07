-- +goose Up
CREATE TABLE player_standings (
  user_id               uuid PRIMARY KEY REFERENCES users(id),
  rating                int,              -- base rating (spec §5.1); NULL = unrated
  is_unrated            boolean NOT NULL DEFAULT false,
  games_played          int NOT NULL,
  wins                  int NOT NULL,
  draws                 int NOT NULL,
  losses                int NOT NULL,
  ongoing               int NOT NULL,
  last_k_score          numeric(4,1),     -- NULL until >= min_games_for_perf
  last_k_perf_rating    int,
  power_rating          int NOT NULL,
  color_score           int NOT NULL,
  xp                    int NOT NULL,
  level                 int NOT NULL,
  xp_to_next_level      int NOT NULL,
  last_level_up_round   int,
  last_level_up_at      timestamptz,
  last_game_finished_at timestamptz,
  is_eligible           boolean NOT NULL DEFAULT false,   -- Phase 4 fills this; Phase 1 = is_active && approved
  updated_at            timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE player_standings;
