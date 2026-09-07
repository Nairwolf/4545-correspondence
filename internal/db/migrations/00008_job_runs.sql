-- +goose Up
CREATE TYPE job_run_status AS ENUM ('running','succeeded','failed');

CREATE TABLE job_runs (
  id               bigserial PRIMARY KEY,
  job_name         text NOT NULL,
  river_job_id     bigint,
  started_at       timestamptz NOT NULL DEFAULT now(),
  finished_at      timestamptz,
  status           job_run_status NOT NULL DEFAULT 'running',
  items_processed  int NOT NULL DEFAULT 0,
  error            text,
  detail           jsonb                      -- per-job counters
);
CREATE INDEX job_runs_name_started ON job_runs (job_name, started_at DESC);

-- +goose Down
DROP TABLE job_runs;
DROP TYPE job_run_status;
