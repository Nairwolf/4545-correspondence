DATABASE_URL ?= postgres://ic:ic@localhost:55432/ic?sslmode=disable
TEST_DATABASE_URL ?= $(DATABASE_URL)

.PHONY: db-up db-down migrate migrate-down sqlc css test test-integration run build

db-up:
	docker compose up -d postgres

db-down:
	docker compose down

migrate:
	DATABASE_URL="$(DATABASE_URL)" go run ./cmd/ic migrate

migrate-down:
	go tool goose -dir internal/db/migrations postgres "$(DATABASE_URL)" down

sqlc:
	cd internal/db && go tool sqlc generate

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
