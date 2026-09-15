# Local, git-ignored overrides — in particular the sign-in variables
# `serve` needs (see README). Every variable is exported so `go run`
# below sees them.
-include .env
export

DATABASE_URL ?= postgres://ic:ic@localhost:55432/ic?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)

TAILWIND_VERSION ?= v4.3.3

.PHONY: db-up db-down psql migrate migrate-down sqlc tailwind css test test-integration run build

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

# Interactive shell into the dev database — for ad-hoc inspection, not
# something app code or migrations should depend on.
psql:
	docker compose exec postgres psql -U ic -d ic

migrate:
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/ic migrate

migrate-down:
	go tool goose -dir internal/db/migrations postgres "$(DATABASE_URL)" down

sqlc:
	cd internal/db && go tool sqlc generate

# Fetch the Tailwind standalone binary (no Node). Linux x64 only; adjust
# the asset name for another platform.
tailwind:
	mkdir -p .bin
	curl -sL -o .bin/tailwindcss \
		https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-linux-x64
	chmod +x .bin/tailwindcss

css:
	.bin/tailwindcss -i web/input.css -o internal/web/static/app.css --minify

test:
	go test ./...

test-integration:
	TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go test -tags integration ./...

run:
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/ic serve

build:
	go build -o bin/ic ./cmd/ic
