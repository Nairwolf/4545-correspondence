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

`serve` runs the public site (`/`, `/standings`, `/levels`,
`/players/{name}`, `/health`, `/jobs`) and the river job runner together;
it shuts down cleanly on SIGINT/SIGTERM.

`sqlc` and `goose` are pulled in as `go tool` dependencies — no global
install needed. The CSS is built with the **Tailwind standalone binary**
(no Node): `make tailwind` fetches it into `.bin/`, then `make css`
regenerates `internal/web/static/app.css` from `web/input.css`. That
generated file is committed, so a plain `go build` needs no CSS step.

Override the database connection with `DATABASE_URL=... make <target>`.

See `CLAUDE.md` for architecture notes and working conventions in this repo.
