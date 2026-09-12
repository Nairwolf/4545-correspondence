-- name: CreateJobRun :one
-- Written at the START of every background job (spec §7's "every job
-- must record its outcome"), before the job does anything else, so a
-- crash mid-run still leaves a "running" row rather than no row at all.
INSERT INTO job_runs (job_name, river_job_id, status)
VALUES ($1, $2, 'running')
RETURNING *;

-- name: FinishJobRun :exec
UPDATE job_runs SET
  status          = $2,
  finished_at     = now(),
  items_processed = $3,
  error           = $4,
  detail          = $5
WHERE id = $1;

-- name: ListRecentJobRuns :many
-- Backs the /jobs page (spec §8.1's Phase 1 stand-in for the admin
-- health page).
SELECT * FROM job_runs ORDER BY started_at DESC LIMIT $1;

-- name: GetLatestJobRunByName :one
-- Backs /health: the most recent outcome of a named job.
SELECT * FROM job_runs WHERE job_name = $1 ORDER BY started_at DESC LIMIT 1;

-- name: GetLatestSuccessfulJobRunByName :one
-- Backs /health's "last_success" field: the newest run of a named job
-- that actually succeeded, independent of what the most recent run did.
SELECT * FROM job_runs
WHERE job_name = $1 AND status = 'succeeded'
ORDER BY started_at DESC
LIMIT 1;
