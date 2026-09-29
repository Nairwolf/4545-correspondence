-- The on-site notification centre (spec §10). internal/notify is the
-- only writer of notifications; the web layer reads them and marks
-- them read. Every read and update is scoped to the player, so a
-- notification id from someone else's list changes nothing.

-- name: InsertNotification :execrows
-- A retried job sending the same event again finds the dedupe key
-- taken and inserts nothing (spec §10). The row count tells the caller
-- which happened.
INSERT INTO notifications (user_id, category, title, body, link_url, dedupe_key)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, dedupe_key) DO NOTHING;

-- name: IsNotificationCategoryMuted :one
SELECT EXISTS (
  SELECT 1 FROM notification_preferences WHERE user_id = $1 AND category = $2
);

-- name: CountUnreadNotifications :one
-- The number on the bell in the site header.
SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL;

-- name: ListNotificationsForUser :many
-- Newest first. The id breaks ties between notices sent in the same
-- transaction, which share created_at.
SELECT * FROM notifications
WHERE user_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: MarkNotificationRead :execrows
UPDATE notifications SET read_at = now()
WHERE id = $1 AND user_id = $2 AND read_at IS NULL;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications SET read_at = now()
WHERE user_id = $1 AND read_at IS NULL;

-- name: ListMutedNotificationCategories :many
SELECT category FROM notification_preferences
WHERE user_id = $1
ORDER BY category;

-- name: MuteNotificationCategory :exec
INSERT INTO notification_preferences (user_id, category)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

-- name: UnmuteNotificationCategory :exec
DELETE FROM notification_preferences WHERE user_id = $1 AND category = $2;
