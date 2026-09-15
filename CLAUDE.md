# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

Phases 1–3 (spec §12) are implemented. Phase 1, read-only parity:
schema/migrations, the Lichess client, `sync-games`/`refresh-ratings`/
`recompute` as both river-scheduled jobs and one-shot `ic` subcommands,
the pure scoring module, and the public web pages (`/`, `/standings`,
`/levels`, `/players/{name}`, `/health`). Phase 2, identity: Lichess
OAuth with PKCE (`internal/lichess/oauth.go`), tokens encrypted at rest
(`internal/tokencrypt`), server-side sessions (`internal/session`), the
join / sign-in / account flow and the admin registration queue
(`internal/web/auth.go`, `admin.go`); `/jobs` now lives at `/admin/jobs`.
Phase 3, self-service: the player dashboard at `/account`
(`internal/web/dashboard.go`, queries in `internal/db/queries/profile.sql`)
— activity, the optional concurrent-games cap (NULL = unlimited, bounded
by the `player.max_concurrent_ceiling` setting), double-game opt-out,
resume-quest, my games and stored authorisation status. Not built yet:
`validate-tokens` (Phase 5) and the GDPR deletion path (Phase 6, a gate
before public registration — see spec §11).
`docs/infinite-correspondence-spec.md`
is still the source of truth for behaviour; read it before changing
anything under `internal/`, and update it when a decision changes rather
than letting code and spec diverge. `PLAN.md` records what was actually
built at each step, including the live-verification results and the
handful of deliberate deviations from the spec's original sketch — read
it alongside the spec, not instead of it.

Build/test commands:

```
make db-up             # start Postgres
make migrate           # apply schema migrations
make test               # unit tests (no DB)
make test-integration   # integration tests (needs TEST_DATABASE_URL)
make run                # run the server (HTTP site + background jobs)
make psql               # psql shell on the dev database
```

See `README.md` for the full command list (including `make tailwind` /
`make css` and the one-shot `ic` admin subcommands) and `PLAN.md`'s build
order for what's done and what's next (the pairing engine onward,
Phase 4+, is not built yet).

## What the system does

An open-ended correspondence chess ladder on Lichess. Every Monday all active players are paired against each other; games are created on Lichess via the bulk-pairing API; finished games are ingested and feed standings, a rolling performance rating, and an XP/level layer. There are no seasons. The site orchestrates and reports — all chess happens on Lichess.

The rebuild exists to fix three failures of the spreadsheet: silent Apps Script failures, manual admin editing, and no player self-service.

## Mandated stack (spec §2.1 — not up for re-litigation)

Go 1.22+ · `net/http` + `chi` · `templ` or `html/template` SSR + htmx · Tailwind CLI · PostgreSQL · `sqlc` (hand-written SQL, **not** an ORM) · `golang-migrate`/`goose` · `river` for background jobs and periodic scheduling · `golang.org/x/oauth2` · stdlib `testing` + `testify`.

Rationale that constrains implementation choices:
- **`sqlc` over an ORM** because the standings and pairing queries must stay readable SQL that can be diffed against the spreadsheet's logic.
- **`river` over bare cron** because persisted job outcomes, retries, and an admin health view are the whole point of the rebuild.
- **No Redis** — production is one binary plus one Postgres.

## Architecture shape

Four concerns over one Postgres database (single source of truth): OAuth/auth flow, the scheduled pairing engine, scheduled sync workers, and an SSR web app (public / player / admin). All Lichess HTTP goes through a single `LichessClient` module with a shared rate limiter, timeouts and bounded retries — nothing else in the app touches HTTP.

Key modules and their properties:

- **Scoring (§5)** — a *pure module with no I/O*, so it can be unit-tested against historical spreadsheet values. Computes base rating, last-k window, FIDE performance rating, power rating, colour score, XP/levels.
- **Pairing engine (§6)** — must be **deterministic**: same inputs, same output. Tie-breaks seed from stable identifiers, never wall-clock time or map iteration order. This is what makes the admin review window meaningful and the engine testable.
- **Ingestion (§7.1)** — idempotent, keyed on the Lichess game ID. Always stores the full Lichess response in `Game.raw_payload` so derived metrics can be recomputed without re-fetching.
- **Aggregates** — `PlayerStanding` is materialised and recomputed incrementally on ingest, with a nightly full recompute as a self-healing backstop. Public pages never compute rolling performance ratings per request.

## Rules that are easy to get wrong

- **One pairing per player per round. Always.** `max_concurrent_games` caps *total ongoing games*, not pairings per round.
- **`max_concurrent_games IS NULL` means unlimited**, and it is the default for every player. A NULL→0 coercion would silently exclude the entire default population from every round while the engine appears to succeed. Spec §11 requires a dedicated test for this.
- **Base rating uses any correspondence rating — provisional or not — falling back to classical only when there's no correspondence rating at all** (§5.1). This deliberately differs from the spreadsheet's `MAX()`. When validating against historical values, expect mismatches for players whose classical exceeded their correspondence rating — verify the difference is explained by this rule, do not "fix" the code to match.
- **The FIDE `dp` table is indexed by score *percentage*, not raw score** (§5.3). Interpolate between the 10% steps so changing `pairing.last_k` keeps working. Indexing by raw score corrupts every number invisibly.
- **`repeat_penalty` is a large finite number (1e6), not infinity** — infinity makes the matching unsolvable rather than merely expensive.
- **Byes are never selected by rating, level, XP, or results** — longest time since last bye, then fewest byes, then user id. A bye must never look like punishment.
- **Capacity skips are not an inactivity signal.** They must never feed missed starts, auto-pause, or the inactivity check-in.
- **No email anywhere.** No SMTP, no address collected or stored. Notifications are on-site (always), optional Lichess PM, optional Discord webhook.

## Behaviour that is configuration, not code

Everything in spec §4.2 lives in the `Setting` table as runtime-editable JSON with an audit trail — cron, review window, `last_k`, `avoid_recent_rounds`, `color_weight`, `repeat_penalty`, XP awards, capacity ceiling, grace periods. Do not hard-code these values; read them from settings.

`RoundExclusion` and `MissedStart` are append-only explanation/audit tables — the pairing algorithm never reads them. `RoundExclusion` exists to answer "why didn't I get a game this week?" directly on the dashboard.

## Build order (spec §12)

1. Read-only parity (schema, Lichess client, completion sync, scoring, public pages) — validates the hardest logic first
2. Identity (OAuth, registration, admin queue)
3. Self-service (player dashboard)
4. Pairing (engine, rounds, review window, diagnostics) — run in shadow mode against the spreadsheet for a week or two
5. Automation (bulk pairing, challenge fallback, auto-pause, notifications)
6. Polish (admin settings, health page, Discord)
7. Optional history migration (§9) — only if maintainers confirm

Out of scope permanently: the spreadsheet's Awards / Awards_Backend sheets (the detection rule was lost and will not be carried over).

## Testing expectations (spec §11)

Table-driven unit tests for the whole scoring module using fixtures from real spreadsheet values; pairing tests covering colour balancing, repeat avoidance, capacity caps, the relaxation ladder, and the odd-pool paths (double-game selection with and without volunteers, two-distinct-opponents, one-white-one-black, cap at `ongoing + 2`, bye rotation fairness over many simulated rounds); integration tests for ingestion idempotency. A mocked Lichess client — **no test touches the live API**.

## Open questions (spec §14)

Unresolved with the maintainers: whether history is imported at all, round numbering (continue ~168+ or restart), which account owns the organiser `challenge:bulk` token, whether the registration timezone field has a use, and whether unrated players are pooled at the league median. Don't invent answers to these in code — ask.

## Git workflow

**Never run `git commit`.** The maintainer commits all work themselves, and commits must carry no `Co-Authored-By` trailer or any other attribution to Claude.

Work in atomic units: keep each change set to one coherent, self-contained piece of work rather than letting unrelated changes pile up in the working tree. When a unit is complete, say so explicitly — "this is a good point to commit" — and summarise what changed. Suggesting a commit message is welcome, but treat it as a starting point: the maintainer writes the final message and will usually rephrase it.

Suggested commit messages must follow [Conventional Commits](https://www.conventionalcommits.org/) — `type(optional scope): summary`, e.g. `feat(scoring): add FIDE performance-rating table` or `docs(spec): correct bulk-pairing endpoint`. Pick `type` from `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `build`, `ci`; add `!` or a `BREAKING CHANGE:` footer only for an actual breaking change.

## Go dependencies

Default to the standard library. Before adding a new Go dependency, ask the maintainer whether it's really necessary and whether the stdlib can do it with a reasonably small amount of code — do not add it preemptively on the assumption it will be wanted. `internal/config` reading `os.LookupEnv` directly instead of a struct-tag env-parsing library is the precedent: three fields didn't justify a dependency.

This does not apply to the libraries the spec itself mandates (§2.1: `chi`, `river`, `sqlc`, `pgx`, `goose`, `testify`, etc.) — those are settled. It applies to anything beyond that set, including a mandated library's optional companion packages.

When a new dependency is added (mandated or approved), record in the commit body: what it's for, and why the stdlib alternative was rejected (too much code to hand-roll, missing functionality, correctness/security risk in a hand-rolled version, etc.). A build-time-only tool dependency (`go tool`, never linked into the shipped binary) still needs the same approval, but its transitive dependency count is not by itself a reason to reject it — call that out explicitly rather than treating a large `go.sum` diff as disqualifying.

## Go style

Code is written to be read by a human, not just to compile — this outranks any mechanical formatting rule below when the two conflict.

When a function or method's parameters — at a call site or in its declaration — don't fit comfortably on one line (roughly 80+ characters), spread them one per line rather than wrapping mid-line. See `internal/lichess/client.go`'s `c.do(...)` call sites and its `do` method declaration for the pattern. This is about parameter lists specifically: a short one-line functional-option constructor (e.g. `func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.httpClient = hc } }`) that runs long because of its inline body, not its parameters, stays on one line — splitting the two short parameters wouldn't shorten anything or help a reader. Likewise, a chained call or boolean expression that runs long isn't "a call with multiple arguments" and shouldn't be force-wrapped into that shape; restructure it on its own terms if it's genuinely hard to read, don't just apply the one-argument-per-line pattern where it doesn't fit.
