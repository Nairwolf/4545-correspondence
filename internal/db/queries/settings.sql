-- name: GetSetting :one
-- A miss (pgx.ErrNoRows) means "no override" — internal/settings falls
-- back to the spec §4.2 default rather than treating it as an error.
SELECT * FROM settings WHERE key = $1;

-- name: ListSettings :many
SELECT * FROM settings ORDER BY key;

-- name: UpsertSetting :one
INSERT INTO settings (key, value, updated_by)
VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET
  value      = EXCLUDED.value,
  updated_by = EXCLUDED.updated_by,
  updated_at = now()
RETURNING *;
