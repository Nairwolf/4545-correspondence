# Phase 3 implementation plan — Self-service

## Context

Phases 1 and 2 are built and closed out; their plans live in git history
(`git show 462c86b:PLAN.md`, `git show f4dc949:PLAN.md`). Phase 3 is
**self-service** (spec §12): the player dashboard of §8.3 — activity
toggle, concurrent-games cap, double-game opt-out, my games, my standing,
Lichess authorisation status, resume-quest. It is the third of the
spreadsheet's three failures (§1, "no self-service"): players stop
relaying activity and workload changes through an admin.

Everything a player changes here is what the pairing engine (Phase 4)
reads: `is_active`, `max_concurrent_games`, `accepts_double_game`,
`auto_paused_at` (§5.7). Phase 3 therefore ends with every
player-controlled eligibility input under the player's control, and
Phase 4 can consume them without building any UI.

Phase 3 is deliberately **UI only**: no migration, no new job, no new
Lichess endpoint. Decided with the maintainer on 2026-09-15:

- **Account deletion (§11, GDPR)** is **not** built here. Every later
  phase adds per-user tables (byes, double games, round exclusions,
  missed starts in Phase 4; notifications and preferences in Phase 5),
  so a deletion path written now would have to be re-opened in each of
  them or silently miss a table. It is written once, against the final
  schema, as a **hard precondition of opening registration publicly**
  (Phase 6). Until then the roster is test accounts and the documented
  path is an admin deleting rows by hand. The recommendation when it is
  built stands: anonymise the user and keep game rows, because
  opponents' standings are computed from those games.
- **`validate-tokens`** (§7) and revocation detection move to **Phase 5**
  with the rest of the token automation (grace-day deactivation, the
  token-revoked notification, the fallback challenge path). The
  dashboard's authorisation status therefore reads exactly what Phase 2's
  account page reads — stored, not expired, not marked revoked — and
  says so in its copy ("checked when you signed in"). Verified facts
  about `POST /api/token/test` are recorded at the end of this file for
  Phase 5.
- **Bye history, double-game count, "why didn't I get a game"** (§8.3)
  wait for Phase 4's tables. The dashboard leaves a marked slot.
- **Notifications** (§10), the **inactivity check-in** (§8.4) and
  **admin player management** (§8.5) are Phases 5, 5 and 4/6 respectively.

### What Phases 1–2 already provide (reused, not rebuilt)

- `player_profiles` has every column §8.3 toggles (`is_active`,
  `max_concurrent_games` NULL = unlimited with `CHECK (> 0)`,
  `accepts_double_game`, `paused_by_admin`, `paused_reason`,
  `auto_paused_at`). **No migration is needed.**
- `GetPlayerProfile`, `GetOAuthToken`, `CountOngoingGamesForUser`
  (§5.8's `ongoing_games`), `GetPlayerHeader`, `ListGamesForUser`,
  `CreateAuditLogEntry`.
- `standings.Recompute(ctx, q, userID, cfg)` — `is_eligible` is derived
  from `is_active`, so an activity change recomputes the row.
- `web`: `requireUser`, `currentUser`, `s.page`, `s.render`, `s.inTx`,
  `audit(...)`, `serverError`; the PRG + `?error=` pattern of `admin.go`;
  `resolveGames` / `playerGame` and the game-table markup in
  `player.html`; `handleAccount` + `account.html` (Phase 2 minimum —
  becomes the dashboard).
- `internal/settings` (`Defaults()`, `applyOverride`) — gains one key.
- The tx-scoped `testServer` harness and `addPlayer` helper.

---

## Decisions (maintainer, 2026-09-15)

1. **Deletion → Phase 6 launch gate; `validate-tokens` → Phase 5.** See
   above.
2. **The dashboard URL stays `/account`.** The nav, the callback
   redirect and Phase 2's page all point there; "Phase 3 grows it into
   the dashboard" was the plan.
3. **Pending applicants may set their preferences before approval.**
   They have a profile row; the settings take effect once approved.
   Rejected applicants see status only.

Process: as in Phase 2 unless the maintainer says otherwise —
implemented in session by the same model, one build step at a time,
stopping after each for review and commit.

---

## Dashboard (`/account`, `requireUser`)

Sections, top to bottom. Every state-changing control is a small plain
`<form method="post">` (boosted by htmx like the admin pages), PRG with
`?saved=<what>` for a one-line confirmation and `?error=<code>` for
validation messages — no custom JS.

1. **Banner** (only when relevant, above everything): authorisation not
   active → "Games can't be created for you automatically" + Re-authorise
   button (§3.3 step 2); `paused_by_admin` → "An admin has paused your
   quest" + `paused_reason`; `auto_paused_at` set → "Your quest is
   paused" + **Resume quest** button (§8.3).
2. **Membership** — Phase 2's status block, unchanged for pending /
   rejected wording; approved users get the sections below instead of
   just a link to their player page.
3. **Activity** — "I'm playing" / "Pause my quest" toggle on
   `is_active`. Copy: pausing takes effect for the next round; current
   games are unaffected and must still be finished.
4. **Games at once** — exactly §8.3's design: an unchecked "Limit how
   many games I play at once" checkbox; ticking it reveals the number
   input (Tailwind `has-[:checked]:` on the fieldset — CSS only)
   bounded 1…`player.max_concurrent_ceiling`. Beside it the live
   sentence from `CountOngoingGamesForUser`:
   - unlimited: "*6 games in progress — no limit set, you'll be paired
     every week.*"
   - under cap: "*3 of 4 games in progress — you'll be paired this
     week.*"
   - at/over cap: "*4 of 4 games in progress — you'll be skipped until
     one finishes.*" and, when over: "*You're above your new limit. You
     won't get new pairings until you're back under it. Nothing happens
     to your current games.*"
   Unticked → `max_concurrent_games = NULL`. **Never 0** (CLAUDE.md;
   `CHECK (> 0)` backs it, and a test asserts NULL).
5. **Double games** — toggle on `accepts_double_game`, on by default,
   §8.3's wording (occasional, respects the cap — needs two free slots,
   one white one black, no penalty for opting out). "You've absorbed N
   double games" is the Phase 4 slot.
6. **My standing** — reuse the `GetPlayerHeader` card row from
   `player.html` (rating, level + progress bar, W–D–L, last-5 score and
   perf); link to the public player page for the charts.
7. **My games** — ongoing first (opponent, colour, days since last
   move, Lichess link), then the ten most recent finished, "full history
   on your player page". Reuses `ListGamesForUser` + `resolveGames`;
   the days-since column is why ongoing is a separate table.
8. **Lichess authorisation** — Phase 2's block, with the copy made
   honest about what it knows: "Active — authorised on <issued_at>,
   valid until <expires_at>." Revocation on Lichess's side is detected
   from Phase 5; a player who revoked can still re-authorise from here.
   The Re-authorise button stays `hx-boost="false"`.
9. **Footer** — Sign out.

Routes:

```
GET  /account                   dashboard
POST /account/activity          active=on|off          → is_active; standings.Recompute; audit profile.activity
POST /account/capacity          limit=on + cap=N | –   → max_concurrent_games (NULL when off); audit profile.capacity
POST /account/double-games      accept=on|off          → accepts_double_game; audit profile.double_games
POST /account/resume            (no fields)            → auto_paused_at = NULL, only when set; audit profile.resume
```

Guards: all `requireUser`, mounted in one `r.Route("/account", …)` group
so a later route cannot forget it. POSTs additionally refuse `rejected`
(403) — they have nothing to configure. `paused_by_admin` does not stop
a player toggling `is_active`: the admin pause is a separate gate (§5.7)
and the banner explains it. Validation failures re-render the dashboard
with 422 and the message inline, like `/join`. Every write is one `inTx`
with its audit row, `before`/`after` being the changed field only.

---

## Queries and settings

No migration. sqlc queries added in a new `internal/db/queries/profile.sql`:

- `SetPlayerActive(user_id, is_active) :one` — returns the row so the
  audit `before` comes from the caller's earlier `GetPlayerProfile`.
- `SetPlayerCapacity(user_id, max_concurrent_games) :one` — `$2` is a
  nullable int; NULL is the unlimited default.
- `SetPlayerAcceptsDouble(user_id, accepts_double_game) :one`.
- `ClearAutoPause(user_id) :execrows` — `WHERE auto_paused_at IS NOT
  NULL`, so a stale double-submit is a detectable no-op (no audit row).

`internal/settings`: `MaxConcurrentCeiling` (`player.max_concurrent_ceiling`,
default 20) with its `applyOverride` case; both existing tests extended.
Nothing else in §4.2 is read by Phase 3.

---

## Module layout (additions)

```
internal/db/queries/profile.sql                 (new) the four queries above; make sqlc
internal/settings/settings.go                   + MaxConcurrentCeiling
internal/web/dashboard.go                       handleAccount moves here from auth.go; the four POST handlers;
                                                  dashboard view model (capacity sentence, ongoing/recent split)
internal/web/templates/account.html             rewritten as the dashboard
internal/web/router.go                          r.Route("/account") group with requireUser
internal/web/server.go                          unchanged page list ("account" already parsed); nav unchanged
```

Dependency direction unchanged; no new Go dependency (Tailwind `has-*`
variants are in the pinned v4 CLI; `make css` after the template).

---

## Build order

Each step is a self-contained commit point (the maintainer commits; see
`CLAUDE.md`). No test touches the live API.

1. **Foundation** — `profile.sql`, `make sqlc`, settings ceiling +
   tests. Small on purpose: it lets the queries be reviewed as SQL
   before any handler uses them.
   Suggested: `feat(profile): self-service queries and capacity ceiling setting`.

2. **Dashboard** — `dashboard.go`, `account.html`, router, `make css`.
   Integration tests (`testServer` + `addPlayer`, one per handler):
   - anonymous `/account*` → 302 `/login`; rejected user POST → 403;
     pending user can toggle;
   - activity off → `is_active=false`, `player_standings.is_eligible`
     false (Recompute ran), audit `profile.activity`, 303 back;
   - capacity: `limit=on&cap=4` → 4; `limit` absent → **NULL** (the
     §5.8/§11 test: the column is NULL, not 0); `cap=0`, `cap=21`,
     `cap=abc` → 422, column unchanged, no audit row; a ceiling override
     in `settings` is respected;
   - the three live-count sentences render for unlimited / under / at
     cap, and the over-cap explanation appears when `ongoing > cap`;
   - double games off/on; resume clears `auto_paused_at` and is a
     no-op (no audit) when it wasn't set; the Resume button is absent
     unless auto-paused; the admin-pause banner shows `paused_reason`;
   - authorisation banner when no token, `revoked_at` set or
     `expires_at` past; the page never contains the token plaintext;
   - my games: an in-progress game is listed under ongoing with days
     since last move; a finished one under recent.
   Suggested: `feat(web): player dashboard — activity, capacity, double games, my games`.

3. **Docs and close-out** — README (dashboard in the route list),
   `CLAUDE.md` repository-state paragraph, spec amendments below, and
   this file's verification section filled in.
   Suggested: `docs: close out Phase 3`.

---

## Spec amendments (applied to `docs/infinite-correspondence-spec.md` on "do it", with a §15 changelog entry)

- **§8.3** — mark what is built; pending applicants may set
  preferences; bye history / double-game count arrive with Phase 4's
  tables; the authorisation block reflects stored state until Phase 5's
  probe.
- **§11** — the deletion path is a **precondition of opening
  registration publicly**, built in Phase 6 against the final schema;
  until then the documented path is manual. Record the design intent
  (anonymise the user, keep game rows) and start a checklist of every
  table carrying a `user_id`, to be extended by Phases 4 and 5.
- **§12** — Phase 3 paragraph: what this plan builds; `validate-tokens`
  listed under Phase 5; deletion listed under Phase 6 as the launch gate.
- **§13** — decisions 1–3 above.

---

## For Phase 5 — `POST /api/token/test`, verified (`lichess-org/api` `doc/specs/tags/oauth/api-token-test.yaml`, fetched 2026-09-15)

Recorded here so the verification isn't repeated. Body `text/plain`, up
to **1000** tokens comma-separated, no bearer needed. Response: JSON
object keyed by token → `{userId, scopes, expires}` or **`null`** for an
invalid token; `scopes` is a comma-separated string (empty if none);
`expires` is epoch **ms** or `null` for a token that never expires. A
token should count as valid only if non-null **and** `userId` matches
the user's `lichess_user_id` **and** `scopes` contains `challenge:write`.
The request body carries plaintext tokens, so the client must never log
request bodies. One call covers the whole roster; river's interval
periodic jobs restart from process start, so anchor the run to
`pairing.cron` rather than a weekly interval.

---

## What was built (2026-09-15)

Steps 1 and 2 landed as planned, with these notes:

- **No deviation in scope or schema.** Four queries in `profile.sql`,
  one settings key, `dashboard.go`, the rewritten `account.html`, the
  `/account` route group. `handleAccount` moved out of `auth.go`.
- **A write that changes nothing writes nothing** — including no audit
  row. The handlers compare the submitted value with the current
  profile first, so a double-submit or an unchanged form is a plain
  redirect. Resume relies on `ClearAutoPause`'s row count for the same
  effect.
- **The capacity sentence is a pure function** (`capacityFor`) with a
  table test, so the wording is pinned without a database.
- **Three Phase 2 session tests were fixed on the way**
  (`test(web): scope session assertions to the test's own user`): they
  counted every row of `sessions`, and the dev database now holds the
  maintainer's live sessions outside the test transaction.
- `html/template` escapes the apostrophe in the dynamic sentence
  (`you&#39;ll`); the integration test matches the escaped form.

Automated verification, green at the end of step 2: `go build`,
`go vet` (both tags), gofmt, `make test`, `make test-integration`
(every package, including the new dashboard tests).

## Verification (end of Phase 3)

Live checks, by hand, still to be run by the maintainer:

1. `make test`, `make test-integration` green; `make css` output committed.
2. `make run`; sign in; `/account` shows the dashboard with the live
   ongoing count matching `/players/<me>`.
3. Toggle each setting; confirm the column in `make psql` and one
   `audit_log` row per change; set a cap below the current count and see
   the calm explanation, not an error; untick → column NULL.
4. Set `auto_paused_at` by hand in psql → banner + Resume button; resume
   → cleared, audit row. Set `paused_by_admin` + reason → banner, no
   button.
5. A pending applicant can set preferences; after approval they still
   hold. A rejected applicant sees status only and gets 403 on POST.
