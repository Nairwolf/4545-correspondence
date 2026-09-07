-- +goose Up
CREATE TABLE audit_log (
  id             bigserial PRIMARY KEY,
  actor_user_id  uuid REFERENCES users(id),   -- NULL = system/CLI
  action         text NOT NULL,
  entity_type    text NOT NULL,
  entity_id      text NOT NULL,
  before         jsonb,
  after          jsonb,
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE audit_log;
