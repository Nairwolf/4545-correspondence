-- +goose Up
-- Defaults are NOT inserted here; the settings package returns spec §4.2
-- defaults for missing keys, so a fresh DB behaves correctly and the admin
-- UI (Phase 6) only stores overrides.
CREATE TABLE settings (
  key         text PRIMARY KEY,
  value       jsonb NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  updated_by  uuid REFERENCES users(id)
);

-- +goose Down
DROP TABLE settings;
