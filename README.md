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

`serve` runs the site (public pages `/`, `/standings`, `/levels`,
`/players/{name}`, `/health`; sign-in and registration under `/join`,
`/login`, `/account`; the admin area under `/admin`) and the river job
runner together; it shuts down cleanly on SIGINT/SIGTERM.

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
join) and approve the application at `/admin/registrations`.

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
```

Players normally enter the league through `/join` (Lichess OAuth plus an
admin's approval), which is also what collects the token bulk pairing
will need. `seed-players` only creates rows without a token — handy for
a local roster, not the way a real league is populated.

`serve` runs `sync-games`, `refresh-ratings` and `recompute` on their own
schedule automatically (spec §7) — the subcommands above are for a manual
run or a first-time data load, not something normal operation needs.

See `CLAUDE.md` for architecture notes and working conventions in this repo.
