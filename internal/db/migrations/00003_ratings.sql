-- +goose Up
CREATE TABLE rating_snapshots (
  id                      bigserial PRIMARY KEY,
  user_id                 uuid NOT NULL REFERENCES users(id),
  correspondence_rating   int,
  correspondence_prov     boolean,          -- NULL when no rating
  correspondence_games    int,
  classical_rating        int,
  classical_prov          boolean,
  classical_games         int,
  fetched_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rating_snapshots_latest ON rating_snapshots (user_id, fetched_at DESC);

-- +goose Down
DROP TABLE rating_snapshots;
