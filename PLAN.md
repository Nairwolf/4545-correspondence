# Phase 5 implementation plan — Automation

**Status: adopted 2026-09-28, being built.** Proposed 2026-09-24 and
parked in `PLAN_PHASE5.md` until Phase 4's live checks were recorded;
it replaced the Phase 4 plan here once they were (that plan and its
record stay in git history, last at `d765c07`). Re-read against those
checks before starting: they change nothing here (step 0 under "What
was built"). Decisions 12–15 were taken by the maintainer on
2026-09-24; the rest are the plan's defaults, to be confirmed or
overturned on review.

## Context

Phases 1–4 are built. The site pairs a round every Monday, but every
game is still started by hand: a published pairing is
`manual_external` / `pending`, players challenge each other, and
`sync-games` finds the game through §7.3 matching. That is the
spreadsheet's fourth failure ("a meaningful share of pairings never
become games"), and Phase 5 exists to remove it (spec §12):

- create the round's games on Lichess at publication, with **bulk
  pairing** (§3.2, §6.3);
- fall back to a **direct challenge** for a player whose token has
  lapsed (§3.3);
- keep tokens healthy: **`validate-tokens`** (§7), the re-authorise
  prompts, the `token.invalid_grace_days` rule (§3.3);
- **missed starts** and **auto-pause** (§6.4, `MissedStart`,
  `evaluate-activity`), and retiring unstarted pairings automatically;
- **on-site notifications** (§10).

Not in Phase 5: cancelling a published round (decision 13 — dropped,
not deferred); custom Lichess PMs (decision 12) and the §8.4
inactivity check-in (decision 14); the admin settings UI, editable
rules, the full health page and the GDPR deletion path (Phase 6);
history import (Phase 7).

Phase 4's own live checks were run by the maintainer on 2026-09-25 and
2026-09-27 and all passed (the Phase 4 plan's "Live verification", in
git history).

### What Phases 1–4 already provide (reused, not rebuilt)

- `internal/lichess`: one serialised client, 429 wait-and-retry-once,
  `APIError`, the `API` interface and `Fake`; `Account` and
  `RevokeToken` already take a player's bearer token
  (`tokencrypt.Secret`, prints `[redacted]`).
- `oauth_tokens` with `scopes`, `expires_at`, `revoked_at`,
  `last_validated_at`; `UpsertOAuthToken` already clears `revoked_at`
  on every sign-in, so **the existing sign-in is the one-click
  re-authorisation** (§3.1) — no new OAuth code.
- `rounds.Publish` (guarded `WHERE state = 'draft'`, row lock),
  `rounds.Scheduler` (the transactional publish-job insert),
  `rounds.Snapshot` carrying `Rated` and `DaysPerMove` precisely so
  Phase 5 creates games with the settings the round was paired under.
- `sync-games`: `ListPairingsToRecheck` already re-fetches any pairing
  with a game id and a non-terminal status (so `created` bulk games
  need no new sync code); aborted / `noStart` → pairing `failed`;
  `matchPendingPairings` is the §7.3 search reused to find
  hand-started games before a retry (decision 15).
- `standings.Recompute` already returns `leveledUp` (ignored today) —
  the level-up notification hook.
- `CountInFlightGamesForUser` counts pending and `created` pairings of
  published rounds — but only those with **no game id**
  (`p.lichess_game_id IS NULL`). *Corrected at step 0:* this plan first
  said bulk-created games would count toward caps at once. They would
  not: a bulk pairing gets its game id at creation and has no `games`
  row until `sync-games` ingests it, and an unaccepted challenge keeps
  its id and no game all week. Step 5 changes the term to "no game
  ingested yet" (`NOT EXISTS` a `games` row for the pairing), which is
  spec §5.8's own wording.
- The enum `exclusion_reason` already has `no_valid_token`; the
  `resume` handler and `auto_paused_at` exist (§8.3).
- The job pattern (`runX` writing `job_runs` under
  `context.WithoutCancel`), `/health`'s `jobNames`, the admin
  `requireAdmin` group, the PRG / `?error=` pattern, the
  `testServer` / `addPlayer` harness.

### What the Lichess API check found (OpenAPI v2.0.174, 2026-09-24)

Checked before planning, as §3.2 asks. Points marked *(lila)* are read
from the server source, not documented, and go on the live-check list.

- **Bulk pairing** returns the **game ids at creation** (`games[{id,
  white, black}]`). Omitting `pairAt` starts the games immediately, so
  the "24h vs 7 days" horizon ambiguity (§6.3) does not matter to a
  create-at-publish design. *(lila: 7 days is enforced.)*
- **"For correspondence games, players can have multiple pairings
  within the same bulk"** — now documented, so the double-game
  volunteer's two games go in one bulk.
- A bulk is all-or-nothing; a failed bulk is free. *(lila:)* a bad
  token comes back as `{"tokens": {"<the token>": "<reason>"}}` — the
  key is the raw token, which we map back to a player. Other refusals
  are `{"error": "..."}`. The body therefore contains secrets and must
  never be logged.
- The bulk's `message` is **sent to each player from the organiser
  account** when the game is created (default *"Your game with
  {opponent} is ready: {game}."*, must contain `{game}`). *(lila: it
  ignores the players' messaging preferences.)* This is the §10 "game
  created" Lichess message, for free.
- **A challenge's id is the game's id once accepted** (documented), so
  a challenge pairing is re-checked by id like a bulk game.
  `acceptByToken` no longer exists.
- `POST /api/challenge/{id}/cancel` cancels a challenge the bearer
  sent (used to withdraw a missed challenge, decision 8).
- `POST /api/token/test`: unauthenticated, up to 1000 tokens, returns
  `{userId, scopes, expires}` or `null` per token.
- **`POST /inbox/{username}` sends a message *as the token's owner*.**
  So the spec's model — each player grants `msg:write` so the league
  can PM them (§3.1, §10) — cannot work: a player's `msg:write` token
  could only send messages *from* that player. League PMs would have
  to come from the organiser account, which *(lila)* may start about
  20 new conversations a day. Hence decision 12.

---

## Decisions

1. **Game creation has an on/off switch: `pairing.game_creation` =
   `manual` (default) | `lichess`.** In plain words: with `manual` the
   site behaves as today (players start games by hand); with `lichess`
   it creates them. The code can ship and be tested before the
   organiser account (§14.3, still open) exists, and if automation
   misbehaves an admin turns it off with one `ic setting` and the
   league carries on by hand. The token gate of §5.7 item 6 applies
   only under `lichess`, which is exactly what spec §5.7's Phase 4 note
   anticipates. Each round records the mode it was published under
   (`rounds.game_creation`, below).
2. **"Database first, Lichess after."** Every Lichess call that follows
   from a change in our data (create a round's games, withdraw a missed
   challenge) runs in a river job that is queued *in the same
   transaction* as the change. In plain words: the round is marked
   published and its "create the games" job is written in one step, so
   either both happen or neither does; the job then talks to Lichess
   outside any database transaction and records what it created. This
   deliberately replaces Phase 4's idea of calling Lichess *inside* the
   publish transaction (the comment at the top of `internal/rounds`): a
   transaction held open across a 60-second 429 wait is bad, and worse,
   a crash after Lichess created the games but before our commit would
   roll back our record while the games exist. Each job is
   **idempotent** (safe to run twice) and **reconciles before acting**:
   `create-games` first lists the organiser's recent bulks (`GET
   /api/bulk-pairing`) and adopts one that already matches the round's
   pairings, so a retried job never creates the games twice.
3. **Every publish path creates games the same way.** The web layer and
   the CLI get a job enqueuer (an insert-only river client in the CLI,
   the running client in `serve`), passed through the existing
   `rounds.Scheduler` interface. A side effect worth having: a draft
   made with *Generate now* or `ic generate-round` now gets its own
   publish job, so it publishes when its window ends instead of up to
   an hour later (spec §8.5 note amended).
4. **Tokens are checked at publication, not only weekly.** The first
   thing `create-games` does is one `POST /api/token/test` over the
   round's players. The weekly `validate-tokens` (§7) stays — it drives
   the pool gate, the player's re-authorise prompt and the organiser
   token alert — but with a 24-hour review window it can be a day stale
   by publication, and bulk pairing is all-or-nothing, so §6.3 4(a)
   "rely on validate-tokens having run just beforehand" is not enough
   on its own.
5. **Who gets which kind of game** (§3.3, §6.3 step 1):
   - both players' tokens valid → **bulk**;
   - one valid → a **challenge sent by the player whose token works**,
     with `color` set to their assigned colour; the other must accept;
   - neither valid → stays **manual** (today's behaviour).
   A token "works" when it is stored, not revoked, not expired,
   carries `challenge:write`, and passed the publication probe.
6. **The grace rule is a pool exclusion, not a flag flip.** §3.3 says a
   player whose token has been invalid for more than
   `token.invalid_grace_days` is "automatically set to inactive … until
   they re-authorise". Flipping `is_active` would need undoing on
   re-authorisation and would overwrite the player's own choice.
   Instead the pool computes it live: invalid for longer than the grace
   → excluded with `no_valid_token` (the dashboard says why and how to
   fix it); re-authorising restores them for the next round with
   nothing to undo. "Invalid since" is `revoked_at`, or `expires_at`
   once passed. **A player with no token row at all** (seeded, never
   signed in) is excluded at once, with no grace: grace is for a token
   that lapsed, and this also means the site can never send a surprise
   challenge to a seeded real member from a test database.
7. **`evaluate-activity` runs just before generation, not after it.**
   §7 says "weekly, after round generation"; §6.4 says a challenge
   "still pending when the next round generates" is a missed start. Run
   after generation, a player reaching their second miss would already
   be in the new round, and their next challenge would go unaccepted
   too. So generation (scheduled, *Generate now*, `ic generate-round`)
   first runs `evaluate-activity` in its own transaction with its own
   `job_runs` row, then generates. A failed evaluation is visible but
   does not block generation.
8. **Unstarted pairings and missed starts, precisely** (§6.4, §5.8):
   - At evaluation, **every still-`pending` pairing of a published
     round is marked `failed`**, whatever its kind — challenge,
     hand-started (both tokens lapsed, automation gave up, or the whole
     league under `manual`). It stops counting toward caps: the §5.8
     caveat Phase 4 left to this job, and the end of the admin's
     weekly *Mark failed* chore (the button stays for exceptions).
   - A `MissedStart` is recorded **only for a challenge pairing, for
     the player who had to accept** — the one case where we know who
     did not act. For a hand-started pairing we cannot tell: either
     player could have challenged, and Lichess shows us no challenge
     that was sent and ignored. When automation gave up, the failure
     was the league's, not theirs. So those pairings are retired
     without a missed start and never lead to an auto-pause — the same
     outcome as today, minus the chore. Never for bye or capacity-skip
     rows either (§5.8, §6.2 6b).
   - The missed challenge is withdrawn on Lichess (a job, decision 2)
     so a late acceptance cannot create a game that never counts.
   - "Consecutive" = missed starts recorded after both the player's
     latest started league game and their latest resume. At
     `missed_starts_to_pause − 1` they are warned; at
     `missed_starts_to_pause` `auto_paused_at` is set and they are
     notified with the resume link.
9. **`validate-tokens` runs one hour before `pairing.cron`** (§7). Its
   schedule is `pairing.cron` shifted by −1h (a ten-line wrapper around
   the parsed cron schedule), so it follows the setting, `CRON_TZ=`
   included.
10. **Admin alerts are computed, not stored.** §10's "job failure,
    token expiry → admins, on-site" is a banner on admin pages built
    from the same data as `/health` (a watched job's latest run failed;
    the organiser token invalid or expiring within 14 days), rather
    than a notification row per failure. `/health` also turns 503 when
    `game_creation = lichess` and the organiser token is invalid — the
    P1 alert of §3.2. *Added at step 0 review (maintainer,
    2026-09-28):* the banner also says when a draft is waiting for
    review and when it will publish itself — spec §6.1's "admins are
    notified", which this plan had left out. One query on the draft.
11. **Notifications are idempotent.** `Notification` gains a nullable
    unique `dedupe_key` (e.g. `round:201:paired:<pairing id>`), so a
    retried job never notifies twice.
12. **Lichess PMs: not in Phase 5** *(maintainer, 2026-09-24).* Phase 5
    builds the on-site notification centre; the only Lichess message is
    the one bulk pairing sends by itself. Custom PMs, if still wanted,
    come from the organiser account in Phase 6. No player is ever asked
    to re-authorise for `msg:write`.
13. **A published round is final** *(maintainer, 2026-09-24).*
    "Cancelling a published round" was never in the original spec: it
    was written into §8.5 and §12 by the Phase 4 plan (`895c12c`) as a
    reading of §6.3's line about cancelling a *scheduled bulk during the
    review window* — a design Phase 4 replaced (nothing reaches Lichess
    before publication). After publication an undo cannot be clean:
    the games exist, Lichess has messaged the players, hand-started
    games are out of reach, and aborting bulk games would rest on
    undocumented behaviour. The review window is the undo; a mistake
    after it is lived with or repaired pairing by pairing. Known gap,
    for Phase 6's player management: voiding a single game that exists
    (e.g. a player banned for cheating mid-week).
14. **The inactivity check-in (§8.4): deferred** *(maintainer,
    2026-09-24).* Once games are created automatically, a player who
    stops playing still gets a game every week, and each ends by
    timeout — so "has not finished a game in 3 weeks" no longer
    describes a dormant player. Revisit with real data (e.g. a run of
    losses on time).
15. **When Lichess keeps refusing a round's bulk, players start those
    games by hand** *(maintainer, 2026-09-24)*, instead of the pairings
    being marked `failed` as §6.3 4(c) says:
    - `create-games` gets a longer retry budget than the other jobs
      (10 attempts, which river's default back-off spreads over roughly
      four hours), so a short outage is ridden out first;
    - on the last failed attempt the round's creation is marked
      `gave_up`, the pairings stay `manual_external` / `pending`, and
      both players are told on-site to challenge their opponent by
      hand; the dashboard shows the Phase 4 "challenge them" link;
    - **players who never start them** are handled by decision 8: a
      reminder 48 hours after publication ("your game against X hasn't
      started yet"), then the pairing is retired at the next
      generation, no missed start, no auto-pause;
    - *Retry game creation* on the round page first runs the §7.3
      search for the round's pending pairings, so a game a player has
      already started by hand is attached rather than duplicated; the
      players whose games it then creates are told "your game has now
      been created — don't start another".

---

## Schema (one migration per step that needs it)

Phase 4 used one migration up front; here each feature step brings its
own, so each commit stands alone.

- **Step 2** `00012_notifications.sql`: `notifications` (§4.1, plus
  `dedupe_key text UNIQUE`, index on `(user_id, read_at)`),
  `notification_preferences` (§4.1; a row only for a category a player
  turned off).
- **Step 5** `00013_game_creation.sql`: `rounds.game_creation`
  (enum `manual` | `creating` | `created` | `gave_up`, NOT NULL DEFAULT
  `manual`; existing rounds are `manual`), set at publication and by
  `create-games`; `rounds.games_created_at timestamptz`.
- **Step 7** `00014_missed_starts.sql`: `missed_starts` (§4.1, plus
  `pairing_id` and `UNIQUE (round_id, user_id)`),
  `player_profiles.resumed_at timestamptz` (resets the streak).

No column for challenge ids (the challenge id *is* the game id, stored
in `pairings.lichess_game_id`). `rounds.bulk_pairing_id` already
exists. The §11 deletion checklist gains `notifications`,
`notification_preferences`, `missed_starts`.

## Settings (added to `internal/settings`, validated at load)

`pairing.game_creation` (`manual` | `lichess`, default `manual`),
`token.invalid_grace_days` (≥ 0, 14), `activity.missed_starts_to_pause`
(≥ 1, 2). `pairing.days_per_move` gains validation against Lichess's
set {1, 2, 3, 5, 7, 10, 14} — a value outside it would make every bulk
fail. `LICHESS_ORG_TOKEN` (§2.2) joins `internal/config`; a
`create-games` run under `lichess` without it fails, naming it.

## Lichess client (`internal/lichess`, step 1)

New `API` methods, each taking the bearer explicitly like `Account`:

```go
CreateBulkPairing(ctx, organiser tokencrypt.Secret, req BulkPairingRequest) (BulkPairing, error)
ListBulkPairings(ctx, organiser tokencrypt.Secret) ([]BulkPairing, error)
TestTokens(ctx, tokens []tokencrypt.Secret) (map[tokencrypt.Secret]*TokenInfo, error) // batches of 1000
CreateChallenge(ctx, challenger tokencrypt.Secret, opponent string, req ChallengeRequest) (Challenge, error)
CancelChallenge(ctx, challenger tokencrypt.Secret, id string) error
```

- A 400 from bulk pairing is a `*BulkRejection{Tokens
  map[tokencrypt.Secret]string, Message string}`; its `Error()` names
  the count and the reasons, never a token. The raw body is never kept.
- `BulkPairing` does not require `clock` (correspondence bulks send
  `correspondence.daysPerTurn`).
- `Fake` grows matching state and knobs: bulks, challenges, token
  info, tokens to reject, per-method errors, and a record of every call
  (with which token) for assertions.
- Tests against `httptest.Server` with fixtures shaped like the
  documented examples, including every 400 body variant.

## Game creation at publish (`internal/rounds/create.go`, steps 5–6)

`rounds.Publish` (and `Generate` under `auto_publish`) keeps its
guarded update, records `rounds.game_creation` from the setting and,
under `lichess`, sets it to `creating` and enqueues `create-games
{round_id}` in the same transaction (unique per round while pending or
running, `MaxAttempts` 10). The job:

1. Loads the round and its pairings still `manual_external` /
   `pending` / no game id. None → `created`, done.
2. **Reconciles:** `ListBulkPairings`; a bulk whose games match some of
   these pairings (same white and black, created after
   `published_at`) is adopted instead of re-created. On a retry from
   the round page, also the §7.3 search (decision 15).
3. **Probes** the players' tokens (`TestTokens`); a failing token is
   marked revoked on the spot and the player notified.
4. **Partitions** (decision 5).
5. **Bulk:** one request, `days` / `rated` from the round's
   `settings_used`, no `pairAt` (immediate), `message` = "Infinite
   Correspondence round {n}: your game with {opponent} is ready:
   {game}". On a `BulkRejection` naming tokens: mark them revoked, move
   those pairings to the challenge / manual partition, resubmit — at
   most 3 submissions per run. Any other refusal fails the run (river
   retries; the last attempt gives up, decision 15). The result is
   committed right after the call: `bulk_pairing_id`, each pairing
   `creation_method = bulk`, `status = created`, game id.
6. **Challenges** (step 6): one call each with the challenger's token;
   each committed on its own right after the call, so a crash can at
   worst leave one duplicate challenge; a failed call leaves that
   pairing manual and does not stop the others.
7. `game_creation = created`, `games_created_at`; notifications;
   enqueue a `sync-games` run so the Overview shows the games within
   minutes rather than an hour.

Pairing status only ever moves with a guarded update (`WHERE status =
'pending' AND lichess_game_id IS NULL`), so an admin's *Mark failed* in
the meantime wins.

## Token health (`validate-tokens`, step 4)

Weekly at `pairing.cron − 1h` (decision 9), and `ic validate-tokens`.
Decrypts every stored token, one `TestTokens` call per 1000, then per
token: valid with `challenge:write` → `last_validated_at`, `expires_at`
refreshed; `null` or missing scope → `revoked_at = now()` (if not
already) and a `token` notification (deduplicated per revocation). The
organiser token is tested in the same call; its state goes in the run's
detail for `/health` and the admin banner. Outcome rules as §7.2 (all
calls failed → failed run).

Pool: `ListPairingPool` gains a base-table `LEFT JOIN oauth_tokens`
(check the generated struct for pointer types — CLAUDE.md);
`rounds.statusOf` gains the `no_valid_token` case last, after
inactive, paused and auto-paused (§5.7's order), only under `lichess`.
The engine gets `StatusNoValidToken` → `ReasonNoValidToken`, with table
tests.

## Unstarted pairings, missed starts, auto-pause (`evaluate-activity`, step 7)

As decisions 7 and 8. Lives in `internal/activity` (the streak rule is
a pure function with its own unit tests); called by the three
generation entry points through one helper; `ic evaluate-activity`.
`handleResume` sets `resumed_at`. `cancel-challenge {pairing_id}` is
enqueued for each missed challenge. The 48-hour "hasn't started yet"
reminder (challenge and hand-started pairings alike) is sent by the
hourly `sync-games`, deduplicated per pairing.

## Notifications (`internal/notify`, step 2 onward)

- `notify.Send(ctx, q, Notice{User, Category, Title, Body, Link, Key})`
  checks the player's preference (except account-critical categories:
  `registration`, `auto_pause`, `token`) and inserts with `ON CONFLICT
  (dedupe_key) DO NOTHING`, in the caller's transaction.
- Categories and emitters: `registration` (approve / reject,
  `admin.go`); `round` (paired / bye / double game at publication,
  worded for how the game is created; "couldn't be created, challenge
  X" when creation gives up; "now created" after a retry); `challenge`
  (you must accept); `unstarted` (the 48h reminder); `missed_start`
  (the warning); `auto_pause`; `token`; `level_up` (`syncOneGame`, from
  `Recompute`'s `leveledUp`).
- UI: a bell with the unread count in `layout.html` (one count query
  for a signed-in user); `/account/notifications` (list, mark one or
  all read); a "Notifications" block on `/account` with a checkbox per
  opt-out-able category. The token banner on `/account` shows
  regardless (§10: the only channel certain to work then).

## Admin and dashboard changes

- **Round page:** per pairing — creation method, status, game link;
  round — game-creation line (`manual` / creating / created at … with
  the bulk id / gave up, see job) and *Retry game creation* when it
  gave up or some pairings are still hand-started.
- **`/admin/tokens`** (nav "Tokens"): players without a valid token,
  since when, days of grace left (§3.3 item 3); the organiser token's
  state and expiry.
- **Admin banner** (decision 10).
- **`/health`:** `jobNames` gains `validate-tokens`, `create-games`,
  `evaluate-activity`; an `organiser_token` field.
- **`/account`:** real token status from `last_validated_at` and
  `revoked_at` (the "checked at sign-in" copy goes); the re-authorise
  banner with the consequence in plain words and the grace countdown;
  "This week" worded per case — bulk: the game link; being created:
  "your game is being created"; challenger: "we challenged X for you,
  waiting for them to accept"; challenged: "accept X's challenge" with
  the link; hand-started: as today; the missed-start warning;
  notification preferences.

## Jobs after Phase 5

| Job | Trigger | New? |
|---|---|---|
| `validate-tokens` | `pairing.cron − 1h`; `ic validate-tokens` | new |
| `evaluate-activity` | first step of every generation; `ic evaluate-activity` | new |
| `generate-round` | `pairing.cron`, *Generate now*, `ic` | unchanged |
| `publish-round` / `publish-round-sweep` | as Phase 4; every draft now gets its job | unchanged |
| `create-games` | queued by publication under `lichess`; *Retry*; `ic create-games <n>` | new |
| `cancel-challenge` | queued by a missed start | new |
| `sync-games` | hourly, and once after `create-games`; sends the 48h reminder | small change |

## Module layout (additions)

```
internal/lichess/{bulk,challenge,tokens}.go (+ tests, fake.go)
internal/notify/notify.go (+ tests)
internal/activity/activity.go (+ tests)
internal/rounds/create.go (+ integration tests)
internal/pairing/pool.go                      StatusNoValidToken
internal/jobs/jobs.go                          new kinds, offset schedule, insert-only client
internal/db/migrations/00012–00014, queries/{notifications,tokens,activity}.sql
cmd/ic/{validate_tokens,create_games,evaluate_activity}.go
internal/web/{notifications,tokens}.go, templates/{notifications,admin_tokens}.html, layout.html, account.html, admin_round.html
```

Dependency direction unchanged: `web`, `cmd/ic` → `rounds`,
`activity`, `notify` → `pairing`, `lichess`, `gen`. **No new Go
dependency.**

---

## Build order

Each step is a commit point; work stops after each for review
(CLAUDE.md). No test touches the live API.

0. **Plan and spec** — once Phase 4's live checks are recorded in
   `PLAN.md`: this plan replaces it (as at the Phase 3→4 transition; the
   Phase 4 record stays in git history), `PLAN_PHASE5.md` is removed,
   and the spec amendments below are applied. *(Done 2026-09-28.)*
   Suggested: `docs: plan Phase 5 automation and amend the spec`.
1. **Lichess client** — the five methods, types, `BulkRejection`,
   `Fake`, httptest tests; `LICHESS_ORG_TOKEN`; `days_per_move`
   validation. No behaviour change.
   Suggested: `feat(lichess): bulk pairing, challenges and token test`.
2. **Notification centre** — migration, `internal/notify`, bell, page,
   preferences, and the emitters that need no automation: registration
   decision, level-up, round published / bye / double game (worded for
   today's hand-started games).
   Suggested: `feat(notify): on-site notification centre`.
3. **Job enqueuer everywhere** — insert-only river client for the CLI,
   the running client in web `Deps`, `rounds.Scheduler` passed at every
   generation; drafts from *Generate now* / `ic` get their publish job.
   Suggested: `refactor(jobs): enqueue jobs from the web and CLI`.
4. **Token health** — settings `pairing.game_creation`,
   `token.invalid_grace_days`; `validate-tokens` and the offset
   schedule; the pool gate and engine status; `/account` token status
   and banner; `/admin/tokens`; `/health` and the admin banner (the
   draft awaiting review included).
   Suggested: `feat(tokens): validate-tokens and the no_valid_token gate`.
5. **Bulk pairing at publish** — migration, `create-games` (reconcile,
   probe, bulk, rejections, record, give up to hand-started),
   notifications, round page status and *Retry* (with the §7.3 search
   first), `ic create-games`, "This week" for bulk games. One-token
   pairings stay hand-started in this step. `CountInFlightGamesForUser`
   counts a pairing until its game is ingested, not until it has a game
   id (see "What Phases 1–4 already provide"), with a test: a capped
   player with a bulk-created, not yet synced game is at capacity.
   Suggested: `feat(rounds): create the round's games with bulk pairing`.
6. **Challenge fallback** — one-token pairings → a challenge from the
   token holder; "This week" for both sides.
   Suggested: `feat(rounds): challenge fallback for a lapsed token`.
7. **Unstarted pairings, missed starts, auto-pause** — migration,
   `internal/activity`, `evaluate-activity` before generation,
   retiring unstarted pairings, `cancel-challenge`, the 48h reminder,
   `activity.missed_starts_to_pause`, resume reset, dashboard warning.
   Suggested: `feat(activity): missed starts and auto-pause`.
8. **Docs and close-out** — README (settings, jobs, the switch-on
   runbook), `CLAUDE.md` repository state, spec, "What was built".
   Suggested: `docs: close out Phase 5`.

## Spec amendments (step 0, applied to `docs/infinite-correspondence-spec.md` on 2026-09-28, with a §15 entry)

- **§3.1 / §10 / §13** — `msg:write` is not a player scope: a message
  is sent as the token's owner. Players are never asked for it; the
  bulk `message` is the "game created" Lichess message; custom PMs are
  Phase 6 at the earliest, from the organiser account (decision 12).
  The §10 note "a revoked token cannot send a Lichess PM" goes with it.
- **§3.3** — decisions 5 and 6 (who challenges whom; grace as a live
  `no_valid_token` exclusion; no token row = excluded at once).
- **§3.4 / §7.3** — a challenge's id is its game's id; challenge
  pairings are re-checked by id, not searched.
- **§4.1** — `Notification.dedupe_key`; `MissedStart.pairing_id` and
  uniqueness; `PlayerProfile.resumed_at`; `Round.game_creation` and
  `games_created_at`.
- **§4.2** — `pairing.game_creation`; `days_per_move` allowed values.
- **§5.7** — item 6 applies under `pairing.game_creation = lichess`.
- **§6.3** — decisions 2, 4, 5 and 15: publication queues
  `create-games`; tokens probed first; one bulk including the double
  game (documented since v2.0.174); `pairAt` omitted, horizon moot;
  reconciliation; the rejection body; give up to hand-started games,
  not `failed`. The last paragraph (cancelling a scheduled bulk) goes:
  no bulk is ever scheduled ahead.
- **§6.4 / §7** — decisions 7–9: every unstarted pairing retired at
  the next generation, missed starts only for challenges; the jobs
  table as above.
- **§8.3 / §8.5** — token status and banner, "This week" per case,
  notifications; `/admin/tokens`, *Retry game creation*; **a published
  round is final** (decision 13), replacing "Cancelling a published
  round is Phase 5"; a manually generated draft publishes on time.
- **§8.4** — deferred, with the reason (decision 14).
- **§11** — deletion checklist gains the three tables.
- **§12** — Phase 5 paragraph rewritten to this scope; "cancelling a
  published round" removed from Phases 4 and 5.
- **§13** — decisions 1, 5, 6, 7, 12–15 recorded.
- **§14.3** — still open; now a prerequisite for switching
  `pairing.game_creation` to `lichess`, not for building.

## Deferred

- Custom Lichess PMs → Phase 6, if still wanted (12).
- §8.4 inactivity check-in → after live data (14).
- Voiding a single existing game (e.g. a banned player) → Phase 6
  player management (13).
- Admin player management, settings UI, health page → Phase 6
  (unchanged).

---

## What was built

Each step is recorded here as it lands.

### Step 0 — plan and spec (2026-09-28)

- **Re-read against Phase 4's live checks.** They passed with no code
  change, and nothing in them changes this plan. Two findings carry
  over:
  - The dev database holds published rounds 201 and 202 with pending
    demo pairings nobody will start. Step 7's `evaluate-activity`
    retires them at the first generation after it lands. They are
    hand-started, so no missed start is recorded.
  - Greedy accepted an avoidable rematch in both live rounds. The
    default solver stays the maintainer's call; it is not Phase 5's
    business.
- **Found: the in-flight count skips any pairing with a game id**
  (corrected under "What Phases 1–4 already provide"). The fix is
  scheduled in step 5, where `created` pairings first appear.
- **Spec amended** as listed under "Spec amendments", with a §15
  entry. The listed decisions also forced these edits, not on the
  list:
  - §2.2: `LICHESS_MSG_ENABLED` removed. It switched the per-player
    PMs that decision 12 drops; Phase 6 can bring a flag back with
    organiser PMs. `LICHESS_ORG_TOKEN` is needed only under `lichess`.
  - §3.2: the bulk field table and notes. No `pairAt`; `days` /
    `rated` from the round; the organiser's message; the rejection
    body. The claim that cancelling a bulk before `pairAt` is how the
    review window works is gone.
  - §4.1: `NotificationPreference` reduced to `(user_id, category)`,
    a row meaning "turned off" — the shape this plan's schema section
    gives it. Its `on_site` / `lichess_pm` flags had nothing left to
    switch. `Round.pair_at` is no longer described as sent to
    Lichess; `Pairing.lichess_game_id` holds a challenge's id.
  - §5.8: a pending pairing is retired by `evaluate-activity`; a
    bulk-created game counts from its creation (the step 5 fix).
  - §8.1: the Overview is filled by the `sync-games` run queued after
    creation, not "from the bulk-pairing response".
  - §8.2: the join flow no longer mentions `msg:write`.
  - §10: the table gains a category column matching `internal/notify`
    (step 2). "Challenge pending, repeated after 48h" becomes the
    `challenge` notice plus the `unstarted` 48-hour reminder.
  - `CLAUDE.md`: the "No email" rule now says on-site only, and never
    ask for `msg:write`. The pointer to `PLAN.md` says earlier phases
    are in its git history.
- **Decided at review (maintainer): a draft awaiting review goes in
  the admin banner** (decision 10, step 4). Spec §6.1 says admins are
  notified of a new draft; the plan had no such notice. §6.1, §8.5
  and §10 now say it is the banner.
- **Kept out of the spec: the bulk `message` text.** The plan's
  example begins "Infinite Correspondence round {n}". The spec only
  says "naming the league and the round", and step 5 decides where the
  name comes from (one league per deployment: no new hard-coded league
  text).

### Step 1 — Lichess client (2026-09-28)

Built as planned: the five `API` methods (`bulk.go`, `tokens.go`,
`challenge.go`), `BulkRejection`, the `Fake`'s game-creation half,
`LICHESS_ORG_TOKEN` in `internal/config` (as a `tokencrypt.Secret`,
optional) and `pairing.days_per_move` checked against Lichess's set.
Nothing calls the new methods yet: no behaviour change.

- **Checked against the source, not only the plan.** The OpenAPI files
  were re-read (still v2.0.174), and so was the server code behind
  them (lila's `BulkPairing` controller and `ChallengeBulkSetup`).
  That confirmed the plan's *lila* points and found two more:
  - **A refused bulk has three shapes, not two.** Besides
    `{"tokens": …}` and `{"error": …}` there is
    `{"duplicateUsers": […]}`. It only happens for real-time games,
    but `BulkRejection` carries it rather than dropping it. And
    `"error"` is either a sentence or, for a refused form, an object
    of field reasons (`{"days": ["…"]}`).
  - **The bulk list returns the organiser's 100 most recent bulks**,
    whether their games were created yet or not — what step 5's
    reconciliation needs. A bulk created without `pairAt` still
    answers `pairedAt: null`, because Lichess returns the bulk as
    stored, before its games were made.
- **Deviation: `do` split in two.** `send` makes the call (with the
  one 429 retry) and returns any status; `do` is `send` plus turning a
  non-2xx into an `*APIError`, as before. `CreateBulkPairing` uses
  `send` so it can read its own 400 body. No other caller changed.
- **Beyond the plan: form refusals are readable.** `APIError.Message`
  was empty when Lichess refused a form (`"error"` is an object then).
  It now reads e.g. "days: Invalid value" — useful for challenge
  errors in the job log.
- **No token in any error text.** `BulkRejection` keeps only the
  reasons, keyed by the tokens the caller sent; any reason that
  quotes one of those tokens has it replaced by `[redacted]`, and
  `Error()` names counts and reasons only. The body is read with a
  1 MB limit (a thousand refused tokens don't fit in the 8 KB used for
  other errors) and never kept.
- **`TestTokens` separates "invalid" from "not mentioned".** A token
  Lichess answered `null` for is in the map with a nil value; one it
  didn't mention is absent. A caller revoking tokens on the strength
  of this call must only revoke the first kind.
- **The `Fake`** checks tokens the way Lichess does: unknown, or
  without `challenge:write`, is refused. `RejectTokens` covers "valid
  at the probe, refused by the bulk". It stamps a bulk with the wall
  clock so a reconciliation looking for bulks created after
  publication finds it. Its ids depend only on call order (`bulk1`,
  `game2`, …), and every call is recorded with the token it was made
  as.
- **For step 7:** cancelling a challenge that was *accepted* in the
  meantime aborts the game instead, until both players have made
  their first move (documented). `cancel-challenge` must therefore treat "the
  game exists" as a possibility, not an error; it never sends
  `opponentToken`, which would let it abort a game already in play.

Tests: every method against `httptest`, including all four 400 shapes
(tokens, duplicate users, a sentence, a refused form) plus an
unreadable body; a 429 on a bulk is an `APIError` after the one retry;
no error text — rejection, transport failure, 401 — contains a token;
token-test batching (2 005 tokens → 1 000, 1 000, 5), duplicates sent
once, `null` vs absent; the challenge form (colour always set, no
clock, no keep-alive stream) and its refusals; cancel and its 404;
the `Fake`'s all-or-nothing bulk, ids, call record and per-method
errors; every `days_per_move` Lichess accepts, and 0, 4 and `"2"`
refused; `LICHESS_ORG_TOKEN` optional and never printed.

Automated verification: `go build`, `go vet` (both tags), gofmt,
`make test` green; `make test-integration` green in every package but
`cmd/ic`. There two sync-games tests fail
(`TestDoSyncGames_NeverMatchesADraftRoundsPairing`,
`…_AmbiguousMatchFlagsThePairingAndAttachesNothing`), and they fail
identically on `2eb8da5`, before this step. They assert on the whole
database — "0 pairings checked", "no unmatched pairing left" — and the
dev database has held 7 pending demo pairings (rounds 201 and 202)
since Phase 4's live checks. Nothing in this step touches that code.
Fixed instead by the entry below.

### Between steps 1 and 2 — the integration tests' own database (2026-09-29)

The two failures above came from a design flaw, not from the two
tests: `TEST_DATABASE_URL` defaulted to the dev database, so every
integration test saw the dev site's rows, and any test could pass or
fail depending on what the maintainer had done there. Scoping
assertions one test at a time relied on every future test remembering
to do so.

- **A separate database, `ic_test`,** in the same Postgres container.
  `make test-integration` now depends on a new `make test-db`, which
  creates it on first use and migrates it on every run. Tests still
  roll back, so it stays empty between runs (checked: no users,
  rounds, games or settings after a full run). No Go change, no new
  dependency.
- **The two sync-games tests pass unchanged.**
- **One test had the opposite hidden dependency.**
  `TestDashboard_AuthorisationAndGames` only passed because the dev
  database held later rounds (201, 202). On an empty database its own
  round 21 became "this week", which links the twelfth finished game
  the test says must not appear. Its game in progress now sits in the
  latest round (22 instead of 1), as it would in a real league; the
  test passes on both databases.
- **Left as they are:** the workarounds written for the shared
  database (round numbers in the 9000s and 80000s,
  `isolatePairingPool`, the dashboard test's skip when published
  rounds exist). They are now unnecessary but harmless, and still
  protect a run pointed at a non-empty database.

Automated verification: `go vet` (both tags), gofmt, `make test` green;
`ic_test` dropped and recreated by `make test-db`, then every
integration test green on it with the test cache off (`-count=1`), none
skipped; `make test-integration` green again on the existing database.

### Step 2 — notification centre (2026-09-29)

Built as planned: migration `00012_notifications.sql`,
`internal/notify`, the bell, `/account/notifications`, the opt-out
checkboxes on `/account`, and the emitters that need no automation.

- **`notify.Send`** writes one notice in the caller's transaction, so
  it is committed with the change it announces or not at all. It skips
  a category the player turned off (never `registration`, `auto_pause`
  or `token`) and refuses a category it does not know. The category
  column is text; the list lives in `notify`, so a new category needs
  no migration.
- **Emitters:**
  - registration approved / rejected (`admin.go`), the rejection with
    the admin's reason;
  - level-up (`syncOneGame`, from `Recompute`'s `leveledUp`), key
    `level_up:<level>`, so a level lost to a settings change and
    regained is not announced twice;
  - round published: paired, double game (one notice for the
    volunteer, both games, white first) and bye (the dashboard's own
    sentence). Sent from `Publish` and from `Generate` under
    `auto_publish`. A player excluded for another reason gets none:
    the dashboard already says why.
- **UI:** a bell with the unread count on every page for a signed-in
  user; the list shows the latest 50, newest first, with *Mark as
  read* per notice and *Mark all as read*; on `/account`, one checkbox
  per opt-out-able category. Changing them writes a
  `profile.notifications` audit row; marking read does not.
- **Deviation: the dedupe key is unique per player, `UNIQUE (user_id,
  dedupe_key)`,** not across the table as decision 11 and spec §4.1
  said. The spec's own example, `round:201:paired:<pairing id>`, is
  sent to both players of the pairing: with a table-wide unique key
  the second player's notice would have been silently dropped. Spec
  §4.1 and §10 amended, with a §15 entry.
- **Decisions taken while building:**
  - The paired and double-game notices name the game to start — "a
    rated correspondence game at 2 days per move, you with white" —
    from the settings the round was generated under, because §7.3
    matching only finds a game with the right days per move and
    colours.
  - All five opt-out-able categories have a checkbox now, including
    `challenge`, `unstarted` and `missed_start`, which nothing sends
    until steps 6–7. A player's choice is then already stored when
    they start.
  - `ic import-pairings` sends no notices: it records the
    spreadsheet's rounds, whose players were told there.
  - The bye sentence moved from `internal/web` to `rounds.ByeSentence`
    (with `rounds.SnapshotOf`), so the dashboard and the notice say
    the same thing.
  - The double-game notice is worded from the games it finds rather
    than failing when there are not two: removing one of the
    volunteer's games already drops their `double_games` row, but a
    notice must never be what stops a round from publishing.
  - A failed unread count leaves the bell at zero rather than failing
    the page; the page's own queries report a database that is down.
- **Known limit:** sync-games writes without a transaction around the
  whole game, as before. A database error between the standing update
  and the level-up notice loses that one notice.
- **Found, not changed: hand-started games can only be matched if they
  are rated.** §7.3 matching (`internal/matching`) always requires a
  rated game, and sync-games compares days per move with today's
  setting, not the round's. With `pairing.rated = false`, no
  hand-started game would ever be found. This predates Phase 5, and
  bulk and challenge games (steps 5–6) are found by id, so it only
  affects hand-started ones.

Tests: `notify` — every category is either critical or opt-out-able;
a notice stored unread with its link; the dedupe key per player (both
players of a pairing, twice each, one notice each); no key, no
deduplication; an opt-out respected, ignored for a critical category;
an unknown category refused; marking read only the owner's. Rounds —
nobody told while the round is a draft; both players told once at
publication, with the time control and the key; a second `Publish`
sends nothing; `auto_publish` tells them at once; the volunteer gets
one notice for both games; the bye gets the `bye_only` sentence; a
paused player gets nothing. Level-up — both players of a first game
rise to level 1 and are told once, with the XP to the next level. Web
— sign-in required; the bell's count on any page; the list newest
first; mark one, mark all; another player's notice untouched, a bad
id refused; preferences saved, audited once, applied to `Send`,
reset; a rejected applicant has no checkboxes but sees their notice;
the approve / reject / change-of-mind notices.

Automated verification: `go build`, `go vet` (both tags), gofmt,
`make test` green; every integration test green on `ic_test` with the
test cache off, three runs in a row, none skipped (411 tests and
subtests, from 390). One new test first failed one run in three: the
volunteer's games came in pairing order, which depends on the random
user ids. The notice now always lists the white game first. `make css`
rebuilt `app.css` (the badge, `sr-only`, and an unrelated `.contents`
class the committed file was missing). Not checked in a browser.

---

## Verification

**Automated, after every step:** `go build`, `go vet` (both tags),
gofmt, `make test`, `make test-integration`. Key tests:

- Client: every method against httptest, including all 400 shapes;
  `BulkRejection.Error()`, job detail and job error text never contain
  a token.
- `create-games`: partition by token state; a double game in one bulk;
  `days` / `rated` from `settings_used`, not current settings; a
  rejection naming a token moves that pairing and resubmits; the last
  attempt gives up to hand-started games and notifies both players;
  running the job twice creates one bulk; a simulated crash after the
  Lichess call is reconciled, not duplicated; a retry attaches a
  hand-started game instead of creating a second; *Mark failed* during
  creation wins; notifications once.
- `validate-tokens`: revoked / missing scope / expired / valid; the
  grace boundary; a player with no token row excluded under `lichess`
  and paired under `manual`; the NULL-cap test still green.
- `evaluate-activity`: every pending pairing → failed; a missed start
  only for the challenged player of a challenge pairing, none for bulk,
  hand-started, bye or capacity skip; streak reset by a started game
  and by resume; warning at 1, pause at 2; the auto-paused player is
  excluded from the round generated right after; the 48h reminder sent
  once.

**Live, by the maintainer**, on a scratch database whose only approved
players are test accounts (an organiser account and two or three
players), `pairing.game_creation = lichess`:

1. Publish a two-player round: games appear on Lichess at once, ids on
   the pairings, the organiser's message arrives, `sync-games` ingests
   them.
2. A three-player round with a double game: one bulk, the volunteer
   has one white and one black.
3. Revoke one test account's token on Lichess: the publication probe
   catches it; that pairing becomes a challenge from the other player;
   accept it and check the game is found by id.
4. Let a challenge go unaccepted, generate again: pairing failed,
   missed start recorded, challenge withdrawn on Lichess.
5. Blank `LICHESS_ORG_TOKEN` and publish: creation gives up after its
   retries (shorten them for the test), players are told to start by
   hand; start one game by hand, restore the token, *Retry*: the
   hand-started game is attached, the other created.
6. `ic validate-tokens`; `/admin/tokens` and `/health` show the
   organiser token's expiry.
