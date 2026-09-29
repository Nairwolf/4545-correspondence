-- +goose Up
-- Phase 5 (spec §10): the on-site notification centre, the league's
-- only channel to a player besides the message bulk pairing sends.

-- category is text rather than an enum: internal/notify owns the list
-- and refuses a category it does not know, so adding one later needs
-- no migration.
--
-- dedupe_key names the event (e.g. round:201:paired:<pairing id>), so a
-- retried job never notifies twice. It is unique per recipient rather
-- than across the table: both players of a pairing get a notice about
-- the same event, under the same key. NULL means "no deduplication";
-- NULLs never conflict.
CREATE TABLE notifications (
  id          bigserial PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES users(id),
  category    text NOT NULL,
  title       text NOT NULL,
  body        text NOT NULL,
  link_url    text,
  dedupe_key  text,
  read_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (user_id, dedupe_key)
);
CREATE INDEX notifications_user_read ON notifications (user_id, read_at);

-- A row means the player turned that category off (spec §4.1). The
-- account-critical categories (registration, auto_pause, token) are
-- sent regardless, so a row for one of them has no effect.
CREATE TABLE notification_preferences (
  user_id   uuid NOT NULL REFERENCES users(id),
  category  text NOT NULL,
  PRIMARY KEY (user_id, category)
);

-- +goose Down
DROP TABLE notification_preferences;
DROP TABLE notifications;
