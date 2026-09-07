-- name: CreateAuditLogEntry :exec
-- actor_user_id is NULL for CLI-driven writes (seed-players,
-- import-pairings) — spec §4.1: "null = system".
INSERT INTO audit_log (actor_user_id, action, entity_type, entity_id, before, after)
VALUES ($1, $2, $3, $4, $5, $6);
