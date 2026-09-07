# Infinite Correspondence

Web application for the Lichess "Infinite Correspondence" league, replacing
the Google Sheets system that ran it. Full technical specification:
[`docs/infinite-correspondence-spec.md`](docs/infinite-correspondence-spec.md).
Current implementation plan: [`PLAN.md`](PLAN.md).

## Development

Requires Go 1.22+ and Docker.

```
make db-up            # start Postgres (host port 55432 — 5432 is often taken locally)
make migrate           # apply schema migrations
make test              # unit tests
make test-integration  # integration tests (needs TEST_DATABASE_URL)
make run                # run the server
```

`sqlc` and `goose` are pulled in as `go tool` dependencies — no global
install needed. Override the database connection with `DATABASE_URL=... make
<target>`.

See `CLAUDE.md` for architecture notes and working conventions in this repo.
