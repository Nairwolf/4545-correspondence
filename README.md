# Infinite Correspondence

Web application for the Lichess "Infinite Correspondence" league, replacing
the Google Sheets system that ran it. Full technical specification:
[`docs/infinite-correspondence-spec.md`](docs/infinite-correspondence-spec.md).
Current implementation plan: [`PLAN.md`](PLAN.md).

## Development

Requires Go 1.22+ and Docker.

```
make db-up             # start Postgres (host port 55432 — 5432 is often taken locally)
make migrate           # apply schema migrations
make psql              # open a psql shell on the dev database, for ad-hoc inspection
make test              # unit tests
make test-integration  # integration tests, on their own database (needs make db-up)
make run               # run the server (HTTP site + background jobs)
```

`serve` runs the site and the river job runner together, and shuts down
cleanly on SIGINT/SIGTERM. The site:

- public pages `/`, `/standings`, `/levels`, `/players/{name}`, `/health`;
- sign-in and registration under `/join` and `/login`;
- the player dashboard at `/account`: this week's pairing, bye or
  reason for sitting out, activity, game limit, double games, my games,
  notifications, and the Lichess authorisation as its last check found
  it, with a banner when it has lapsed;
- the admin area under `/admin`: the registration queue
  (`/admin/registrations`), job runs (`/admin/jobs`), rounds
  (`/admin/rounds`: generate, review, edit, publish, cancel, with the
  pairing diagnostics on each round's page) and tokens
  (`/admin/tokens`: the players without a valid Lichess token, and the
  organiser token). A banner on every admin page says when a draft is
  waiting for review, a job's latest run failed or, when the site
  creates games, the organiser token cannot or soon will not work.

### Configuration for `serve`

`serve` needs the sign-in configuration below; the one-shot subcommands
need only `DATABASE_URL`, except `ic validate-tokens`, which also needs
`TOKEN_ENCRYPTION_KEY` to read the stored tokens. Put them in a `.env` file at the repo root
(git-ignored; the Makefile includes it) or export them:

```
LICHESS_CLIENT_ID=ic.localhost                 # any unique string; nothing is registered on Lichess
LICHESS_REDIRECT_URI=http://localhost:8080/auth/lichess/callback
TOKEN_ENCRYPTION_KEY=<openssl rand -hex 32>    # 64 hex chars; rotating it invalidates every stored token
SESSION_SECRET=<openssl rand -hex 32>          # at least 32 characters
ADMIN_LICHESS_USERNAMES=yourname               # optional, comma-separated bootstrap admins
LICHESS_ORG_TOKEN=lip_...                      # optional while pairing.game_creation is manual: the league account's challenge:bulk token
```

Lichess OAuth is a public PKCE flow: there is no client secret and no
app to register. Locally, `http://localhost:8080/...` works as the
redirect URI as long as you open the site at exactly that origin (cookies
are only marked `Secure` when the redirect URI is `https`).

Trying it end to end: `make run`, open http://localhost:8080/join, tick
the agreement and continue — Lichess shows a consent screen for
"Create, accept, decline challenges", then you land on `/account` as a
pending applicant. Sign in with an account listed in
`ADMIN_LICHESS_USERNAMES` (it is created approved and admin on its first
join) and approve the application at `/admin/registrations`. The new
member's `/account` then shows the dashboard: pause/resume, an optional
limit on games in progress, the double-game opt-out and their games.

The integration tests run against their own database, `ic_test`, next
to the dev one in the same container. `make test-integration` creates
it on first use and applies the migrations on every run. Each test runs
inside a transaction that is rolled back, so `ic_test` stays empty: a
test sees only the rows it creates, and nothing you do on the dev site
can change a test's result. To run them elsewhere, set
`TEST_DATABASE_URL`; the target migrates that database but only creates
`ic_test`.

`sqlc` and `goose` are pulled in as `go tool` dependencies — no global
install needed. The CSS is built with the **Tailwind standalone binary**
(no Node): `make tailwind` fetches it into `.bin/`, then `make css`
regenerates `internal/web/static/app.css` from `web/input.css`. That
generated file is committed, so a plain `go build` needs no CSS step.

Override the database connection with `DATABASE_URL=... make <target>`.

### `ic` subcommands

`make run` / `make build` wrap the `ic` binary (`go run ./cmd/ic <cmd>` /
`bin/ic <cmd>`), which also has one-shot admin commands beyond `serve`
and `migrate`:

```
ic seed-players <usernames.txt>              # create approved players from a username list, one per line (dev/testing; no tokens)
ic import-pairings [--pair-at RFC3339] <round.csv>  # create a round + pairings from a CSV export of the sheet
ic sync-games                                 # run the hourly Lichess sync job once
ic refresh-ratings                            # run the daily rating-snapshot job once
ic recompute                                  # rebuild every player_standings row from scratch
ic evaluate-activity                          # retire unstarted pairings, record missed starts, now (generation runs it first)
ic generate-round                             # run the weekly round generation once
ic publish-round <round-number>               # publish a draft now
ic create-games <round-number>                # retry a published round's game creation (queued for serve to run)
ic validate-tokens                            # test every stored Lichess token, and the organiser's, now
ic setting <key> <json-value>                 # set a spec §4.2 setting (validated, audited)
```

Until the admin settings page exists (Phase 6), `ic setting` is how a
setting is changed. A string value needs its JSON quotes, so the shell
needs them quoted too. For example, to choose the optimal pairing solver
(spec §6.2 step 4; `greedy` is the default):

```
ic setting pairing.solver '"blossom"'
```

The admin's *Generate now* and *Regenerate* buttons pick up a changed
pairing setting on the next click. Regenerating a draft under each
solver is the way to compare them: the round page shows which one
produced it.

Players normally enter the league through `/join` (Lichess OAuth plus an
admin's approval), which is also what collects the token bulk pairing
needs. `seed-players` only creates rows without a token — handy for
a local roster, not the way a real league is populated.

`serve` runs the jobs on their own schedule automatically (spec §7):
`sync-games` hourly, `refresh-ratings` and `recompute` daily,
`validate-tokens` an hour before each `pairing.cron` firing,
`generate-round` on `pairing.cron` (every generation runs
`evaluate-activity` first), `publish-round` when a draft's review
window ends, `publish-round-sweep` hourly as the safety net for any
draft past its window, `create-games` when a round is published while
the site creates games, and `cancel-challenge` when a challenge nobody
accepted is retired. The subcommands above are for a manual
run or a first-time data load, not something normal operation needs.

`validate-tokens` marks a token Lichess refuses as revoked and tells
its player on-site; signing in again is the re-authorisation. Tokens
only gate pairing once `pairing.game_creation` is `lichess`: a player
with no token is then left out at once, and one whose token lapsed
after `token.invalid_grace_days` (14). Under the default, `manual`,
nobody is left out for a token. `/health` reports the organiser token
from the latest run, and turns 503 when the site creates games and that
token cannot. After replacing `LICHESS_ORG_TOKEN` and restarting, run
`ic validate-tokens` so `/health` and the admin banner see the new
token before the next weekly run.

### Running the first rounds

The site pairs for real from its first round; there is no shadow mode.
The safety net is the review window: each Monday's round is a draft an
admin can check, edit or cancel before it publishes itself.

**Before the first Monday.**
- Lengthen the window so every draft gets looked at:
  `ic setting pairing.review_window_hours 24` (the default is 6).
  Keep `pairing.mode` at `review_window`.
- Check when generation runs. `pairing.cron` defaults to
  `0 12 * * 1`, Monday at noon **in the server's own timezone**. To
  pin it, prefix a zone: `ic setting pairing.cron '"CRON_TZ=UTC 0 12 * * 1"'`.
  `UTC` always works; a named zone such as `Europe/Paris` needs the
  timezone database on the host, and `serve` refuses to start without
  it rather than pairing at the wrong hour. `pairing.cron` is read at
  start-up: restart `serve` after changing it.
- The first generated round is numbered one past the last imported
  round.

**When a draft appears** on `/admin/rounds`:
- Read its diagnostics: pool size, how an odd pool was resolved
  (double game or bye, and for whom), repeats accepted, the solver.
  Then the exclusions (who sits out, and why) and the pairing table
  (rating gaps, colour penalties, rematches).
- To compare solvers, switch with `ic setting pairing.solver
  '"blossom"'` (or `'"greedy"'`) and press *Regenerate*.
- Fix what needs fixing: flip colours, swap opponents between two
  pairings, remove a pairing. *Regenerate* reruns the engine on the
  current pool, keeping the number and the review window. The pool is
  **not** recalculated on its own during the window, so regenerate
  after a player pauses or a game finishes. *Cancel* (reason required)
  discards the draft and frees its number for the next one.

**Publication.** A draft publishes itself at the end of its window,
whether the weekly job, *Generate now* or `ic generate-round` made it.
The publication runs in `serve`: if it is down when the window ends,
the draft publishes as soon as it starts again. *Publish now* (or
`ic publish-round <number>`) publishes at once.

**After publication**, while the site does not create games
(`pairing.game_creation = manual`, the default; see below): players
challenge each other by hand, and `sync-games` finds the game within
the hour. It only recognises a **rated, standard, correspondence** game
at `pairing.days_per_move` days per move (default 2), with the colours
the round assigned, started no earlier than a day before the round
published. A pairing still not started 48 hours after publication gets
one reminder, and the next generation retires it (marks it failed),
with no penalty, so it stops counting towards anyone's games in
flight; *Mark failed* on the round page does that at once.

**If the server was down at generation time**, river does not catch up
the missed run: use *Generate now*.

Once a few rounds have gone out without edits, shorten the window, or
stop drafting altogether with `ic setting pairing.mode '"auto_publish"'`.

### Switching game creation on

With `pairing.game_creation = lichess` the site creates each round's
games on Lichess when it publishes the round (spec §6.3): one bulk
pairing from the league's organiser account for every pairing whose
two players' tokens work, a challenge from the player whose token works
when the other's has lapsed, and by hand, as above, for the rest. Turn
it on only once the organiser account is settled (spec §14.3) and the
live checks at the end of `PLAN.md` have passed on a scratch database.

**Before switching.**
- Create the organiser token: a personal access token of the league's
  Lichess account with the `challenge:bulk` scope
  (lichess.org/account/oauth/token). Set it as `LICHESS_ORG_TOKEN`,
  restart `serve`, and run `ic validate-tokens`: `/admin/tokens` and
  `/health` then show it as valid, with its account and expiry.
- Read `/admin/tokens`. Once the switch is on, an approved player who
  has never signed in is left out of pairing at once, and one whose
  token lapsed more than `token.invalid_grace_days` (default 14) ago is
  left out until they sign in again. The page lists them already, so
  they can be asked to sign in first.
- `pairing.days_per_move` must be a value Lichess accepts (1, 2, 3, 5,
  7, 10 or 14); `ic setting` refuses any other.

**Switching.** `ic setting pairing.game_creation '"lichess"'`, with no
restart: the next generation applies the token rule and the next
publication creates games. A draft already waiting is published under
the new setting.

**At each publication.** The round page says the games are being
created, then when they were created and the bulk's id, and each
pairing says how its game was made. Players get an on-site notice with the
game's link, and Lichess sends the players of each bulk game a message
from the organiser account. A `sync-games` run follows at once, so the
games show on the Overview within minutes.

**When Lichess refuses.** `create-games` retries for about four hours
(10 attempts). If it still fails, it gives up: the players are told to
start their games by hand, `/health` and the admin banner turn red, and
once the cause is fixed *Retry game creation* on the round page (or
`ic create-games <round-number>`) tries again. The retry first looks for
games players have already started by hand and attaches them instead
of creating a second. To stop automation altogether,
`ic setting pairing.game_creation '"manual"'`: a round still waiting
for its games gives up at its job's next attempt, without calling
Lichess, and the league carries on by hand.

**Every week after.** `validate-tokens` runs an hour before generation
and tells each player whose token has lapsed to sign in again; their
dashboard shows a banner with the days of grace left. A player who
must accept a challenge and doesn't is reminded after 48 hours; at the
next generation the challenge is withdrawn on Lichess and counted as a
missed start. The first warns them; `activity.missed_starts_to_pause`
(default 2) in a row pause them until they press *Resume quest*. The
admin banner warns 14 days before the organiser token expires: replace
it, restart `serve`, and run `ic validate-tokens`.

See `CLAUDE.md` for architecture notes and working conventions in this repo.

## Licence

Copyright (C) 2026 nairwolf and contributors.

This program is free software: you can redistribute it and/or modify it
under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or (at
your option) any later version (SPDX: `AGPL-3.0-or-later`). It is
distributed in the hope that it will be useful, but WITHOUT ANY
WARRANTY; without even the implied warranty of MERCHANTABILITY or
FITNESS FOR A PARTICULAR PURPOSE. See [`LICENSE`](LICENSE) for the full
text.

**Running a modified copy.** Section 13 of the licence requires anyone
who runs a modified version as a site to offer its users that modified
source. Every page's footer links to the source code
(`internal/web/templates/layout.html`): point it at your own repository.

**Third-party code** keeps its own licence:
- `internal/pairing/solver_blossom.go` is a port of NetworkX's
  `max_weight_matching` (BSD-3-Clause); the copyright notice at the top
  of the file must stay with it.
- `internal/web/static/htmx.min.js` is htmx 2.0.4 (0BSD).
- `internal/web/static/app.css` is generated by Tailwind CSS (MIT), whose
  notice the build keeps at the top of the file.
