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
make test-integration  # integration tests (needs TEST_DATABASE_URL)
make run               # run the server (HTTP site + background jobs)
```

`serve` runs the site and the river job runner together, and shuts down
cleanly on SIGINT/SIGTERM. The site:

- public pages `/`, `/standings`, `/levels`, `/players/{name}`, `/health`;
- sign-in and registration under `/join` and `/login`;
- the player dashboard at `/account`: this week's pairing, bye or
  reason for sitting out, activity, game limit, double games, my games,
  authorisation status;
- the admin area under `/admin`: the registration queue
  (`/admin/registrations`), job runs (`/admin/jobs`), and rounds
  (`/admin/rounds`: generate, review, edit, publish, cancel, with the
  pairing diagnostics on each round's page).

### Configuration for `serve`

`serve` needs the sign-in configuration below; the one-shot subcommands
need only `DATABASE_URL`. Put them in a `.env` file at the repo root
(git-ignored; the Makefile includes it) or export them:

```
LICHESS_CLIENT_ID=ic.localhost                 # any unique string; nothing is registered on Lichess
LICHESS_REDIRECT_URI=http://localhost:8080/auth/lichess/callback
TOKEN_ENCRYPTION_KEY=<openssl rand -hex 32>    # 64 hex chars; rotating it invalidates every stored token
SESSION_SECRET=<openssl rand -hex 32>          # at least 32 characters
ADMIN_LICHESS_USERNAMES=yourname               # optional, comma-separated bootstrap admins
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

The integration tests run against the dev database inside a transaction
that is rolled back, so real rows (your own sessions, for instance) stay
put and tests must scope their assertions to the rows they created.

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
ic generate-round                             # run the weekly round generation once
ic publish-round <round-number>               # publish a draft now
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
will need. `seed-players` only creates rows without a token — handy for
a local roster, not the way a real league is populated.

`serve` runs the jobs on their own schedule automatically (spec §7):
`sync-games` hourly, `refresh-ratings` and `recompute` daily,
`generate-round` on `pairing.cron`, `publish-round` when a draft's
review window ends, and `publish-round-sweep` hourly as the safety net
for any draft past its window. The subcommands above are for a manual
run or a first-time data load, not something normal operation needs.

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

**Publication.** A draft generated by the weekly job publishes itself
at the end of its window. One generated with *Generate now* or
`ic generate-round` is published by the hourly sweep, up to an hour
after its window ends. *Publish now* (or `ic publish-round <number>`)
publishes at once.

**After publication** (until Phase 5 creates games on Lichess): players
challenge each other by hand, and `sync-games` finds the game within
the hour. It only recognises a **rated, standard, correspondence** game
at `pairing.days_per_move` days per move (default 2), with the colours
the round assigned, started no earlier than a day before the round
published. A pairing nobody starts keeps counting towards both players'
games in flight: use *Mark failed* on the round page. The automatic
missed-start check arrives with Phase 5.

**If the server was down at generation time**, river does not catch up
the missed run: use *Generate now*.

Once a few rounds have gone out without edits, shorten the window, or
stop drafting altogether with `ic setting pairing.mode '"auto_publish"'`.

See `CLAUDE.md` for architecture notes and working conventions in this repo.
