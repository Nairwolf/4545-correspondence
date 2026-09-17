# Phase 4 implementation plan — Pairing

## Context

Phases 1–3 are built and closed out. Phase 4 is **pairing** (spec §12):
the pairing engine (§6.2), the round model and its state machine (§6.1),
scheduled generation on `pairing.cron`, the draft / review-window /
auto-publish flow, admin round management and diagnostics (§8.5), and
the dashboard's "why didn't I get a game / bye / double game" slot left
open by Phase 3 (§8.3). It is the first phase that *writes* league
state on its own initiative. The spec's "shadow mode against the
spreadsheet" is **not** built: the maintainer decided (2026-09-17) that
the site pairs for real from its first run; the review window, with a
long window for the first rounds, is the safety net instead.

What Phase 4 deliberately does **not** do — all of it Phase 5 (§12):
create games on Lichess (bulk pairing, `pairAt`, the cancel endpoint),
the challenge fallback, token validation and the `no_valid_token`
exclusion, missed starts / `evaluate-activity` / auto-pause,
notifications ("round published", "you got a bye", "draft awaiting
review"), and the `missed_starts` table (its only writer is
`evaluate-activity`). Admin player management, the settings UI and the
health page are Phase 6.

**Why a Phase 4 round is still useful without Lichess automation.** A
published round's pairings are created as `creation_method =
manual_external`, `status = pending` — exactly the shape `import-pairings`
already produces. Players challenge each other by hand as they do today,
and `sync-games` discovers the games through §7.3 matching with no new
code. Phase 4 therefore replaces the spreadsheet's `Pairing_Maker`
outright; Phase 5 only replaces the manual challenge.

### What Phases 1–3 already provide (reused, not rebuilt)

- `rounds` / `pairings` tables with the §4.1 shape (`00004`), including
  `publish_at` / `published_at` / `pair_at`, `generated_by` (`schedule`,
  `manual`, `imported`), `pairings.edited_by`, and the
  `UNIQUE (round_id, white_user_id, black_user_id)` import key.
- `player_standings.power_rating` and `color_score` (materialised by
  `standings.Recompute`), `CountOngoingGamesForUser`,
  `scoring.CapacityAllows(max *int, ongoing)`.
- `player_profiles`: every §5.7 input (`is_active`, `paused_by_admin`,
  `auto_paused_at`, `max_concurrent_games` NULL = unlimited,
  `accepts_double_game`).
- The job pattern: one `runX(ctx, db, …, riverJobID *int64)` per job in
  `cmd/ic`, writing `job_runs` under `context.WithoutCancel`, shared by
  the river worker and the one-shot `ic <job>` subcommand
  (`cmd/ic/recompute.go` is the canonical form); `jobs.NewClient` with
  one queue, one worker.
- `web`: the `/admin` route group behind `requireAdmin`, `s.inTx`, PRG
  with `?error=` / `?saved=`, the `<details>`-with-reason form and the
  amber diagnostics panel in `admin_registrations.html` / `jobs.html`,
  the `testServer` / `addPlayer` / `addAdmin` / `postAs` harness, and
  `createTestPairing` in `internal/standings`.
- `settings.Load` ignores unknown keys, so the new keys can be written
  into the table before the struct learns them.
- `internal/lichess.Fake` — unchanged; Phase 4 makes **no Lichess call**.

---

## Decisions

Decisions 1–3 and 7 were taken by the maintainer on 2026-09-17; the
rest are the plan's defaults, to be confirmed or overturned on review.

1. **Solver: greedy now, blossom later if real rounds need it.** §6.2
   step 4 prefers a minimum-weight perfect matching (blossom) and
   accepts greedy. The engine calls the solver through one small
   `Solver` interface; Phase 4 ships **greedy** (the spreadsheet's
   behaviour, ~40 readable lines). The blossom port (~1 000 lines, no
   maintained Go library) is step 6, explicitly deferrable — decided
   only after the first live rounds show whether greedy's short-sighted
   bottom-of-the-list pairings actually happen.
2. **Cron parsing: `robfig/cron/v3`.** `pairing.cron` is a cron
   expression (§4.2) and river's `PeriodicSchedule` needs a `Next()`;
   river already depends on `robfig/cron/v3` (indirect in the module
   graph today) and the spec's stack table names it. The commit body
   records why a hand-written five-field parser was rejected. This is
   the first wall-clock schedule in the codebase — the existing three
   jobs run on intervals from process start.
3. **No shadow mode.** The site pairs for real from its first run. The
   first rounds run in `review_window` mode with
   `pairing.review_window_hours` raised (e.g. 24) so each draft can be
   checked on `/admin/rounds` before it publishes itself. Nothing
   derived from history (recent opponents, bye and double-game
   rotation, dashboard explanations, `sync-games`) ever reads a draft
   or cancelled round, so an unwanted draft is simply cancelled.
4. **The relaxation ladder (§6.2 step 7) is recorded, not run.** Since
   `repeat_penalty` is a large *finite* number (§6.2 step 3, CLAUDE.md),
   the cost graph is complete and a perfect matching always exists (with
   the duplicated volunteer it is K_{n+1} minus one edge, still
   matchable for n ≥ 3); the solver already returns the matching with
   the fewest, least-recent repeats, so lowering `avoid_recent_rounds`
   or `color_weight` could never make a difference to solvability. The
   engine records on each pairing whether it is a repeat and of which
   round, and on the round how many repeats it accepted — what the admin
   needs to see — and does not iterate. Spec amendment listed below.
5. **Exclusion rows are written for approved members only**, for the
   reasons `inactive`, `paused`, `auto_paused`, `at_capacity`, `bye`,
   and `removed_by_admin` (new). Pending applicants are not loaded into
   the pool at all (they see their status on `/account`); the enum still
   carries `pending_approval` and `no_valid_token` for Phase 5.
6. **Token validity does not gate the Phase 4 pool.** §5.7 item 6 is
   "valid token **or** the fallback path is enabled"; in Phase 4 every
   game is created by hand, i.e. the fallback path *is* the path, and
   seeded players have no token at all. `no_valid_token` exclusions
   arrive with `validate-tokens` in Phase 5.
7. **In-flight games count for capacity**: `games.status =
   in_progress` plus pending pairings of *published* rounds that have
   no game yet. §5.8 says "status is created or in_progress": a pairing
   the player has been told to start is a game in flight before Lichess
   knows about it; without the second term a capped player would be
   re-paired every week for as long as they delayed their challenge. One
   query (`CountInFlightGamesForUser`) replaces `CountOngoingGamesForUser`
   everywhere — engine, standings `ongoing`, dashboard sentence — so the
   three can never disagree. Caveat: a pairing nobody starts counts until
   it is marked failed; Phase 5's missed-start job does that
   automatically, and Phase 4 gives the admin a *Mark failed* action on
   a published round's pending pairing for the meantime.
8. **A tiny `ic setting <key> <json>` subcommand** to set the review
   window, the mode and the weights before Phase 6's settings UI exists
   (`UpsertSetting` plus an audit row). Optional; without it the
   maintainer edits the `settings` table in `make psql`.
9. **Round numbering** (§14.2, already "continue the sheet's
   sequence"): the first generated round is `max(number) + 1` over
   non-cancelled rounds, i.e. one past the last imported round. Stated
   here so it is a decision, not an accident.

---

## Schema (one migration, `00011_pairing.sql`)

```sql
CREATE TYPE exclusion_reason AS ENUM
  ('at_capacity','inactive','paused','auto_paused','no_valid_token',
   'bye','pending_approval','removed_by_admin');
CREATE TYPE odd_pool_outcome AS ENUM ('even','double_game','bye');

CREATE TABLE round_exclusions (              -- §4.1, append-only
  id bigserial PRIMARY KEY,
  round_id int NOT NULL REFERENCES rounds(id),
  user_id uuid NOT NULL REFERENCES users(id),
  reason exclusion_reason NOT NULL,
  ongoing_games int, max_concurrent_games int,   -- populated for at_capacity
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (round_id, user_id)
);
CREATE TABLE byes         (id bigserial PRIMARY KEY, round_id …, user_id …, created_at …, UNIQUE (round_id, user_id));
CREATE TABLE double_games (id bigserial PRIMARY KEY, round_id …, user_id …, created_at …, UNIQUE (round_id, user_id));

ALTER TABLE rounds
  DROP CONSTRAINT rounds_number_key,
  ADD COLUMN pool_size int,
  ADD COLUMN odd_pool odd_pool_outcome,
  ADD COLUMN repeat_pairings int,
  ADD COLUMN settings_used jsonb;                          -- the §4.2 pairing keys as generated
CREATE UNIQUE INDEX rounds_number_live ON rounds (number) WHERE state <> 'cancelled';
CREATE UNIQUE INDEX rounds_one_draft  ON rounds ((true))  WHERE state = 'draft';

ALTER TABLE pairings
  ADD COLUMN position int,             -- order in the generated list (stable display)
  ADD COLUMN rating_gap int,           -- |power(white) − power(black)| at generation
  ADD COLUMN color_penalty int,        -- §6.2 colour_penalty for the colours actually assigned
  ADD COLUMN repeat_of_round int;      -- last round these two met within the window, else NULL
```

Why each:

- **Per-pairing diagnostics are stored, not recomputed.** Standings move
  after generation; the admin view (§8.5) must show what the engine saw.
  `settings_used` (a struct, marshalled — not a map — so the JSON is
  stable) pins the weights, including `rated` and `days_per_move` for
  Phase 5's publish, and makes a regeneration comparable. An admin edit
  **nulls** the three diagnostic columns on the touched rows; the view
  shows "edited" instead of numbers that would lie.
- **Partial unique index on `rounds.number`.** A cancelled draft no
  longer holds its number, so the next generation reuses it and the
  sequence has no gaps. `GetRoundByNumber` gains `AND state <>
  'cancelled'` (otherwise `:one` would return an arbitrary row);
  `import-pairings` finding a *draft* with its number refuses, as it
  already does for any other conflict.
- **`rounds_one_draft`** makes "one draft at a time" a database fact.
  The admin's *Generate now* runs in-request while the river worker may
  be generating in another goroutine; the loser gets a unique violation
  mapped to `ErrDraftExists` instead of a check-then-insert race.
- `cancel_reason` is not added: the existing `notes` column is it.
- **`UNIQUE (round_id, white, black)` is compatible with double games**:
  the volunteer's two pairings have different opponents, and
  `games.pairing_id UNIQUE` is one game per pairing, which is what two
  pairing rows give. No change. (A reversed-colour duplicate `(A,B)` +
  `(B,A)` would pass the index; an engine post-condition test forbids it.)
- `missed_starts` is **not** created (Phase 5).
- The §11 deletion checklist gains `round_exclusions`, `byes`,
  `double_games` (spec amendment).

### Queries (new `internal/db/queries/rounds.sql`; small edits to `pairings.sql`, `games.sql`)

- **`ListUnmatchedPairings` and `ListPairingsToRecheck` gain `AND
  r.state = 'published'`.** Without this the hourly `sync-games` would
  query Lichess for every white player of a *draft* and, with `pair_at
  − 1 day` as the cutoff, attach any rated 2-day game those two happen
  to have started into a round that does not exist yet; it would also
  block regenerate's `DELETE` through the `games.pairing_id` FK.
- `CountInFlightGamesForUser :one` (decision 7) replaces
  `CountOngoingGamesForUser`; callers in `standings.go` and
  `dashboard.go` switch over.
- `ListPairingPool :many` — one query, the engine's whole input:
  approved users with profile, standing (`power_rating`, `color_score`),
  in-flight count, last bye round number and bye count, last double-game
  round and count (from **published** rounds only), `ORDER BY
  power_rating DESC, user_id`. Eligibility is computed **live** from
  `users` / `player_profiles`, never from `player_standings.is_eligible`,
  which is only refreshed on ingest / toggle / approval. Readable SQL
  with lateral subqueries so it can be diffed against
  `Pairing_Maker_Backend`.
- `ListRecentOpponents :many` — `(user_a, user_b, round_number)` for
  pairings of the **last N published rounds by number** (not `number >
  current − N`, which breaks on gaps), status not in
  (`cancelled`, `failed`) — a game that was never played is not "met".
- `NextRoundNumber :one` — `COALESCE(MAX(number), 0) + 1` over
  non-cancelled rounds.
- `GetDraftRound :one`, `GetRoundByID :one`, `GetRoundForUpdate :one`
  (`FOR UPDATE`, taken by every publish/cancel/regenerate/edit path so
  Phase 5 can put Lichess calls between the lock and the flip),
  `ListRounds :many` (with pairing / bye counts),
  `ListPairingsForRound :many` (joined usernames and both power ratings),
  `ListExclusionsForRound`, `ListByesForRound`, `ListDoubleGamesForRound`.
- `CreateGeneratedRound :one` (all new columns), `InsertGeneratedPairing
  :one`, `InsertBye`, `InsertDoubleGame`, `InsertRoundExclusion`.
- `PublishRound :one` — `UPDATE … SET state='published', published_at=$2,
  pair_at=$2 WHERE id=$1 AND state='draft' RETURNING *`. `pair_at` is
  set to the real publish moment so an early publish never leaves §7.3's
  `pair_at − 1 day` cutoff ahead of games players create. The state
  guard is the idempotency of every publish path.
- `CancelRound :one` (same guard, sets `notes`),
  `CancelPairingsForRound :exec`.
- `DeleteRoundPairings/Byes/DoubleGames/Exclusions :exec` — regenerate.
- `UpdatePairingColours :one`, `DeletePairing :execrows`, `InsertEditedPairing`
  (swap = delete two, insert two in one tx, avoiding a transient hit on
  the `(round, white, black)` index), `MarkPairingFailed` (exists), all
  `WHERE` the round is a draft (or published, for *Mark failed*), setting
  `edited_by` and nulling diagnostics.
- `ListDraftsDue :many` — `state='draft' AND publish_at <= now()` for
  the sweep.
- Dashboard: `GetLatestPublishedRound`, `GetPairingForUserInRound`,
  `GetExclusionForUserInRound`, `CountByesForUser`,
  `CountDoubleGamesForUser`, `GetLastByeForUser`.

`internal/settings` gains, with defaults from §4.2 and `applyOverride`
cases: `PairingCron`, `PairingMode` (`review_window` | `auto_publish`),
`ReviewWindowHours` (≥ 0), `AvoidRecentRounds` (≥ 0),
`ColorWeight` (≥ 0), `RepeatPenalty` (> 0), `OddPoolStrategy`
(`double_then_bye` | `bye_only`), `Rated`. `Load` validates ranges and
enum values and fails naming the key: there is no settings UI until
Phase 6, and a bad row typed into psql must fail the next
`generate-round` run visibly rather than default silently.

---

## Engine — `internal/pairing` (pure, no I/O)

Mirrors `internal/scoring` and `internal/matching`: plain structs in,
plain structs out, exhaustively unit-tested. Integers throughout (power
ratings are `int32` in the database); no `time.Now()`, no float.

```go
type Player struct {
    ID            string       // users.id; the only tie-break key
    Status        Status       // Active, Inactive, Paused, AutoPaused
    PowerRating   int
    ColorScore    int
    InFlight      int          // decision 7
    MaxConcurrent *int         // nil = unlimited — never coerced
    AcceptsDouble bool
    LastDoubleRound, LastByeRound *int
    DoubleCount, ByeCount int
}
type Config struct {
    RoundNumber, AvoidRecentRounds, ColorWeight, RepeatPenalty int
    OddPool OddPoolStrategy    // DoubleThenBye | ByeOnly
}
type History map[[2]string]int   // unordered pair (ids sorted) → most recent round met; only ever looked up
type Pairing struct { White, Black string; RatingGap, ColorPenalty int; RepeatOfRound *int }
type Exclusion struct { ID string; Reason Reason; InFlight, Cap int }
type Result struct {
    Pairings   []Pairing
    Exclusions []Exclusion
    DoubleGame *string; Bye *string
    PoolSize, RepeatPairings int
}
func Generate(players []Player, history History, cfg Config, solve Solver) Result
```

Steps, each a named function with its own table test:

1. **Pool** (§6.2 step 1, §5.7, §5.8): status filter → exclusion rows;
   `scoring.CapacityAllows(max, inFlight)` → `at_capacity` rows carrying
   the count and the cap; each survivor exactly once.
2. **Order**: `slices.SortFunc` with the total order
   `power_rating DESC, ID ASC` (`sort.Slice` is not stable and is not
   used). This order is the greedy scan order and the stored `position`.
3. **Odd pool** (step 6), decided *before* solving so the volunteer's
   second slot is part of the matching. `bye_only` skips 6a. Otherwise
   the volunteer is chosen by **rotation only** — `AcceptsDouble`,
   `CapacityAllows(max, inFlight+2)` (nil passes), then longest since
   last double (never = infinity), fewest doubles, ID — never by cost
   or rating; the engine does not "try each candidate". No candidate →
   the bye: longest since last bye, fewest byes, ID; **never** rating,
   level, XP or results (the test shuffles ratings and asserts the same
   recipient). The bye player leaves the pool with a `bye` exclusion
   *and* a bye row; the volunteer is duplicated as a second slot. A pool
   of one is a bye with no pairings; a pool of zero is an empty round.
4. **Cost** (step 3): `|Δpower| + ColorWeight·colour_penalty(a,b) +
   (RepeatPenalty if met within the window)`. `colour_penalty` returns
   both the value and the argmin so step 6 cannot disagree with it. The
   volunteer's two slots are not an edge at all.
5. **Solve** via `Solver`. `Greedy`: first pair the volunteer's two
   slots with their two cheapest **distinct** partners (otherwise the
   scan can strand the two copies as the last unmatched pair), then walk
   the ordered list pairing each unmatched slot with its cheapest
   unmatched partner; ties → `(cost, lower ID, higher ID)`. `Blossom`
   (decision 1) is the later drop-in, fed edges pre-sorted by that same
   total order so its output does not depend on insertion order.
6. **Colours** (step 5): the argmin from the cost; on a tie the
   lower-rated player takes white, then lower ID. For the volunteer's
   two games the assignment is made **jointly**: of the two ways to give
   them one white and one black, take the smaller total leftover
   imbalance, then the tie-break — the hard one-white-one-black
   constraint of step 6a. The realised penalty (which may differ from
   the per-pair argmin used in the cost) is what gets stored.
7. **Diagnostics**: gap, penalty, repeat round per pairing;
   `RepeatPairings` on the result.
8. **Post-conditions** (asserted in tests on every fixture): each user
   appears in at most one pairing, except the volunteer in exactly two
   with one white and one black; no `(A,B)`+`(B,A)`; nobody excluded is
   paired; `PoolSize` = paired + bye.

Determinism is a test, not a comment: the same input in three different
slice orders yields identical results, and a round regenerated from the
stored inputs equals the stored round.

Tests (§11, table-driven, hand-built fixtures): colour balancing (both
worked examples from §6.2); repeat avoidance inside and just outside the
window; **the NULL-cap test** — a player with `MaxConcurrent == nil` and
40 in-flight games is paired in every one of 20 simulated rounds and
never appears in an exclusion; cap 4 reproduces the §5.8 worked-example
table; double game with and without a volunteer, two distinct opponents,
one white one black, `inFlight + 2` gate, opt-out respected, `bye_only`,
the `[A, B, V]` pool where A's cheapest partner is B; bye rotation
fairness over 200 simulated rounds with an odd pool and no volunteers
(max − min bye count ≤ 1); a pool of 2 who just met still pairs (repeat
recorded); pools of 1 and 0; greedy vs. brute force on every pool ≤ 6
— run against greedy regardless of decision 1, so the distance from
optimal is known and the double-game failure mode is pinned.

---

## Rounds service — `internal/rounds`

The I/O layer around the engine; every entry point is one transaction
and starts with `GetRoundForUpdate` where a round exists.

- `Generate(ctx, tx, cfg, now, source, scheduler)`: `NextRoundNumber`;
  `ListPairingPool` + `ListRecentOpponents` → `pairing.Generate`; insert
  the round — `review_window`: `draft`, `publish_at = pair_at = now +
  ReviewWindowHours`; `auto_publish`: `published`,
  `published_at = publish_at = pair_at = now`. Then pairings (`manual_external`,
  `pending`, `position`, diagnostics), byes, double games, exclusions,
  an audit row (`round.generate`, entity `round`). A `rounds_one_draft`
  violation is returned as `ErrDraftExists`. With a scheduler present
  (the server) and a draft, it `InsertTx`es the `publish-round`
  job with args `{RoundID, PublishAt}` and `UniqueOpts{ByArgs}` —
  `PublishAt` is in the args because river treats a completed job with
  the same args as a duplicate, so a regenerated round would otherwise
  keep the *old* schedule. The CLI passes no scheduler; the hourly sweep
  covers it (documented).
- `Publish(ctx, tx, roundID, now, actor)` — lock, `PublishRound`
  (guarded); zero rows means "already published or cancelled", which
  every caller treats as success (the scheduled job, the sweep and an
  admin's *Publish now* can race; the row guard makes the first one win).
  Audit `round.publish`.
- `Cancel(ctx, tx, roundID, reason, actor)` — drafts only; cancels the
  pairings; cancels the pending `publish-round` job (`JobCancelTx`) if
  any; audit `round.cancel`. Cancelling a **published** round is out of
  scope for Phase 4 (games may exist; §6.3's bulk cancel is Phase 5).
- `Regenerate(ctx, tx, roundID, cfg, now, actor)` — draft only: delete
  its pairings / byes / doubles / exclusions, rerun the engine into the
  same row (same number; `generated_at`, diagnostics and `settings_used`
  refreshed; `publish_at` unchanged), cancel and re-insert the publish
  job. Audit `round.regenerate` with the old pairing list as `before`.
- Pairing edits, drafts only, server-side enforced, each with
  `edited_by`, nulled diagnostics and an audit row (`pairing.flip`,
  `pairing.remove`, `pairing.swap`): flip colours; remove (both players
  get a `removed_by_admin` exclusion so the dashboard can say so; if the
  removed pairing was one of the volunteer's two, their `double_games`
  row goes too); swap opponents between two pairings (`A.white–B.black`,
  `B.white–A.black`, colours re-derived by the engine's colour step so a
  swap never worsens balance). *Mark failed* on a published round's
  pending pairing (`pairing.fail`) is the manual stand-in for Phase 5's
  missed-start job (decision 7).
- The `audit` helper in `auth.go` hard-codes `entity_type = "user"`;
  it gains an `entityType` parameter (`import_pairings.go` already
  writes `round` / `pairing` types directly — one helper, not two).
- `import-pairings` (`getOrCreateRound`): unchanged apart from the
  `GetRoundByNumber` guard; finding a `draft` with its number is
  refused like any other conflict. It stays a transition/dev tool.

### Jobs (`cmd/ic`, same pattern as `recompute.go`)

| Job | Trigger | Body |
|---|---|---|
| `generate-round` | `pairing.cron` via a `PeriodicSchedule` from `robfig/cron` (decision 2); also `ic generate-round` and the admin button | `rounds.Generate(source=schedule)`; `ErrDraftExists` is a *failed* run with that message, visible on `/admin/jobs` and `/health` |
| `publish-round` | scheduled once at `publish_at`, unique on `{RoundID, PublishAt}` | `rounds.Publish`; no-op if not draft |
| `publish-round-sweep` | `PeriodicInterval(time.Hour)` | `ListDraftsDue` → `Publish` each; the safety net of §7 |

`jobs.Handlers` gains `GenerateRound`, `PublishRound(roundID)`,
`PublishSweep`; `ops.jobNames` gains the three so `/health` watches
them. `pairing.cron` is read once at start-up; changing it needs a
restart until Phase 6's settings UI can call `PeriodicJobs().Remove/Add`
— documented in the README. River does not catch up a firing missed
while the binary was down; §6.1's manual generation covers that, and the
admin page says so.

---

## Admin — `/admin/rounds`

Routes in the existing `requireAdmin` group:

```
GET  /admin/rounds                          list: number, state, generated, publish_at, pairings, byes, source; "Generate now"
POST /admin/rounds/generate                 synchronous rounds.Generate(source=manual) recording a job_runs row; ?error=draft-exists
GET  /admin/rounds/{id}                     round view (below)
POST /admin/rounds/{id}/publish             Publish now (draft only)
POST /admin/rounds/{id}/cancel              reason required (the <details> pattern)
POST /admin/rounds/{id}/regenerate
POST /admin/rounds/{id}/pairings/{pid}/flip
POST /admin/rounds/{id}/pairings/{pid}/remove
POST /admin/rounds/{id}/pairings/{pid}/fail  published rounds, pending pairings only
POST /admin/rounds/{id}/pairings/swap       fields: a, b (pairing ids)
```

Round view, top to bottom: state banner (draft: "auto-publishes at …"
/ published / cancelled + reason); **diagnostics panel** (§8.5): pool size, odd-pool
outcome with who and why, repeat pairings accepted, settings used, the
§5.8 reminder that the pool is not recalculated during the review window
(regenerate to pick up changes); **exclusions** grouped by reason with
count / cap for capacity; **pairings table** — position, white, black,
both power ratings, gap, colour penalty, repeat-of-round, edited marker,
per-row Flip / Remove (drafts) or Mark failed (published, pending);
swap form; round actions. Generation runs in-request: the engine on a
200-player pool is milliseconds and there is no Lichess call, so the web
layer needs no river client (the publish job for an admin-generated
draft is picked up by the sweep, at most an hour late — or `Deps` gains
a narrow `Scheduler` interface; decided at step 4 by how small it is).
Nav gains **Rounds** (`nav = "rounds"`); page list gains `admin_rounds`,
`admin_round`. New Tailwind classes → `make css`.

## Dashboard slot (`/account`)

- **This week**: for the latest published round — your pairing
  (opponent, colour, a "challenge them on Lichess" link until Phase 5
  creates the game), or the bye sentence from §8.3, or the exclusion
  reason in plain words (at capacity with the numbers; paused; inactive;
  removed by an admin).
- **Double games**: "You've absorbed N double games" in the existing
  section. **Bye history**: count and last round.
- `standings.Recompute` sets `is_eligible = approved ∧ active ∧
  ¬paused_by_admin ∧ auto_paused_at IS NULL` (the `00006` TODO) as a
  display column; the pool query never reads it. The `dashboard.go`
  comment saying eligibility derives from `is_active` alone is updated.

---

## Module layout (additions)

```
internal/db/migrations/00011_pairing.sql
internal/db/queries/rounds.sql                 new queries; pairings.sql / games.sql edits above
internal/settings/settings.go                  eight pairing keys, validation, tests
internal/pairing/{pairing,pool,odd,cost,colours,solver_greedy}.go + _test.go   pure engine
internal/pairing/solver_blossom.go             step 6, optional
internal/rounds/{rounds,generate,publish,edit}.go + _integration_test.go
internal/jobs/jobs.go                          three handlers, cron schedule
cmd/ic/{generate_round,publish_round,setting}.go, main.go, serve.go, import_pairings.go
internal/web/rounds.go, templates/admin_rounds.html, admin_round.html, layout.html (nav), server.go (pages), router.go
internal/web/dashboard.go, templates/account.html   this-week slot
internal/web/auth.go                            audit() entity type parameter
internal/standings/standings.go                 is_eligible, in-flight count
README.md, CLAUDE.md, docs/infinite-correspondence-spec.md
```

Dependency direction: `web`, `cmd/ic` → `rounds` → `pairing`, `gen`;
`pairing` imports only `scoring`. New Go dependency: `robfig/cron/v3`
(decision 2) — none otherwise.

---

## Build order

Each step is a self-contained commit point; work stops after each for
review (`CLAUDE.md`). No test touches the live API.

1. **Schema, queries, settings** — migration, `rounds.sql`, the
   `sync-games` published-only filter (with a test: a draft pairing is
   never returned), `CountInFlightGamesForUser` and its two callers,
   `make sqlc`, settings keys with validation tests, the
   `GetRoundByNumber` guard.
   Suggested: `feat(db): rounds, byes, double games and exclusions for pairing`.
2. **Engine** — `internal/pairing` with the greedy solver and the full
   §11 unit suite (no DB).
   Suggested: `feat(pairing): deterministic pairing engine with greedy solver`.
3. **Rounds service and jobs** — `internal/rounds`, the three jobs,
   cron schedule, `ic generate-round` / `ic publish-round <n>` /
   `ic setting`; integration tests: a generated round round-trips the
   engine's result; the NULL-cap player is paired with 40 in-flight
   games; one exclusion row per reason; a review-window draft schedules
   exactly one publish job and regenerate reschedules it; auto-publish
   publishes in-tx with `pair_at = published_at`; a second generate
   fails with `ErrDraftExists`; publish is
   idempotent under three callers; regenerate replaces rows and keeps
   the number; history queries ignore drafts and cancelled rounds.
   Suggested: `feat(rounds): scheduled round generation, review window and publication`.
4. **Admin rounds** — pages, handlers, guard test (`TestAdmin_RoleChecks`
   pattern), one test per action (edits on a published round → 409),
   edited rows carry `edited_by`, nulled diagnostics and an audit row,
   `make css`.
   Suggested: `feat(admin): round management and pairing diagnostics`.
5. **Dashboard slot and eligibility** — this-week block, counts,
   `is_eligible`; a test per sentence.
   Suggested: `feat(web): this week's pairing, byes and exclusions on the dashboard`.
6. **Blossom solver** *(decision 1; may be deferred)* — port of a
   reference minimum-weight perfect matching, equivalence with brute
   force on all pools ≤ 8, determinism, then flip the default solver.
   Suggested: `feat(pairing): minimum-weight perfect matching solver`.
7. **Docs and close-out** — README (routes, subcommands, cron restart
   note, first-rounds runbook), `CLAUDE.md` repository-state paragraph,
   spec amendments below, this file's "what was built" section.
   Suggested: `docs: close out Phase 4`.

---

## Spec amendments (applied to `docs/infinite-correspondence-spec.md` on 2026-09-17, with a §15 changelog entry)

- **§4.1** — `Round` gains `pool_size`, `odd_pool`,
  `repeat_pairings`, `settings_used`; `number` unique among
  non-cancelled rounds; at most one draft. `Pairing` gains `position`,
  `rating_gap`, `color_penalty`, `repeat_of_round`; note that a
  generated round's pairings are `manual_external` until Phase 5
  overwrites the method at publish. `RoundExclusion` unique per
  (round, user), reason gains `removed_by_admin`.
- **§4.2** — ranges validated at load; `pairing.cron` applies on restart.
- **§5.7** — item 6: in Phase 4 the fallback path is the only path, so
  tokens do not gate the pool (decision 6).
- **§5.8** — `ongoing_games` counts in-progress games plus pending
  pairings of published rounds (decision 7).
- **§6.2 step 5 / 6a** — the odd-pool choice precedes solving; the
  volunteer's colours are assigned jointly after matching; the realised
  penalty is stored.
- **§6.2 step 7** — decision 4's wording: unreachable with a finite
  penalty; repeats are recorded, not relaxed.
- **§6.3** — Phase 4 publication is the state change only, `pair_at`
  becomes the actual publish time; pairings are discovered by §7.3.
- **§7** — `publish-round-sweep` named; `pairing.cron` read at
  start-up; missed firings are covered by manual generation.
- **§7.3** — only pairings of published rounds are matched.
- **§8.3 / §8.5** — mark what is built; removed pairings; *Mark
  failed*; generation runs in-request.
- **§11** — the deletion checklist gains the three tables.
- **§12** — Phase 4 paragraph rewritten to this scope; shadow mode
  replaced by "first rounds under a long review window"; `missed_starts`
  and `evaluate-activity` listed under Phase 5.
- **§13 / §14** — decisions 1–9 as answered; §14.2 closed with the
  concrete rule.

---

## Verification (end of Phase 4)

Automated: `go build`, `go vet` (both tags), gofmt, `make test`,
`make test-integration` green after every step.

Live, by the maintainer:

1. `make migrate`; `ic setting pairing.review_window_hours 24`; `ic
   generate-round` → a draft appears on `/admin/rounds` with
   diagnostics and the next number after the imported rounds; `/health`
   shows the run; `ic sync-games` makes no Lichess call for the draft.
2. Sanity-check the draft by hand against the standings: rating gaps
   small, colours going to the player who is due them, no repeat within
   the window unless the diagnostics say the pool forced it.
3. Cancel the draft with a reason; generate again → same number reused.
4. Generate from the admin page, edit a pairing,
   watch it auto-publish after the window (or publish now); a paired
   player's dashboard shows the opponent, a capped player's shows the
   at-capacity sentence with numbers, `pair_at` equals `published_at`.
5. Restart `serve` and confirm the cron schedule fires at the
   configured time (use a near-future cron for the check).
