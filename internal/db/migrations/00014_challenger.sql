-- +goose Up
-- Phase 5 step 6 (spec §3.3, §6.3 step 6): who sent a pairing's
-- challenge. When only one player's token works, create-games sends
-- the challenge from that player's account and the other must accept
-- it. The pairing holds the challenge's id as its game id (§3.4), but
-- nothing else says which of the two sent it — and a missed start is
-- recorded for the one who had to accept (§6.4), and the challenge is
-- withdrawn with the sender's token.
--
-- Set exactly when creation_method = 'challenge', and always one of
-- the pairing's two players.
ALTER TABLE pairings
  ADD COLUMN challenger_user_id uuid REFERENCES users(id),
  ADD CONSTRAINT pairings_challenger_set CHECK (
    (creation_method = 'challenge') = (challenger_user_id IS NOT NULL)
  ),
  ADD CONSTRAINT pairings_challenger_plays CHECK (
    challenger_user_id IN (white_user_id, black_user_id)
  );

-- +goose Down
ALTER TABLE pairings DROP COLUMN challenger_user_id;
