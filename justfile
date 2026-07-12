default: help

# Install the pre-commit hooks
bootstrap:
  pre-commit install --hook-type commit-msg --hook-type pre-push

# Install the frontend dependencies (skipped when node_modules is already present)
web-deps:
  [ -d web/node_modules ] || (cd web && npm ci)

# Build the SvelteKit app into the directory the Go binary embeds
web: web-deps
  cd web && npm run build

# Run the frontend unit tests
web-test: web-deps
  cd web && npm test

# Type-check the frontend
web-check: web-deps
  cd web && npm run check

# Generate the sqlc repository from internal/db/migrations + internal/db/query
generate:
  go tool sqlc -f internal/db/sqlc.yaml generate

# Fail if the committed queries and the generated repository have drifted
generate-check:
  go tool sqlc -f internal/db/sqlc.yaml diff

# Create a new migration pair (just add-migration add_widgets)
add-migration migration:
  #!/usr/bin/env bash
  # The name is not cosmetic. sqlc reads the migrations dir as its schema and
  # skips rollbacks BY FILENAME SUFFIX, so a down file that is not named
  # *.down.sql gets parsed as schema and its DROP TABLEs corrupt sqlc's catalog.
  # And sqlc applies up-files in LEXICAL order, so the sequence must be
  # zero-padded or 10_ sorts before 9_.
  set -euo pipefail
  seq=$(printf '%06d' $(( $(ls internal/db/migrations/*.up.sql | wc -l) + 1 )))
  touch "internal/db/migrations/${seq}_{{migration}}.up.sql"
  touch "internal/db/migrations/${seq}_{{migration}}.down.sql"
  echo "created internal/db/migrations/${seq}_{{migration}}.{up,down}.sql"

# Build the server
build: web generate
  CGO_ENABLED=0 go build -o ./build/bakery .

# Run the server
run: web generate
  go run .

# Run unit tests (Go + frontend). DB tests spawn an ephemeral Postgres via docker,
# or use TEST_DB_URL if it is exported.
test: web generate web-test
  go test -v ./...

# Run the race detector
race: web generate
  go test -race ./...

# Run tests with coverage (frontend unit tests run first so CI gates on them)
coverage: web generate web-test
  mkdir -p build
  go test -v -coverprofile=build/coverage.out ./...
  go tool cover -func=build/coverage.out
  go tool cover -html=build/coverage.out -o build/coverage.html

# Start a shared Postgres for the local test loop (faster than a container per package)
db-up:
  docker run -d --name bakery-testdb -p 127.0.0.1:5432:5432 \
    -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=postgres \
    postgres:18-alpine
  @echo "export TEST_DB_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable"

# Stop the shared test Postgres
db-down:
  -docker rm -f bakery-testdb

# Run all code checks
check: check-format vet lint web-check

# Check the format of the code
check-format:
  if [ -n "$(gofmt -l .)" ]; then gofmt -l .; exit 1; fi

# Run the vet tool
vet: web generate
  go vet ./...

# Run the golangci-lint tool
lint: web generate
  go tool golangci-lint run

# Format all code
format:
  go tool golangci-lint fmt

# Clean up all build artifacts
clean:
  go clean
  -rm -rf ./build
  -rm -rf ./tmp
  -rm -rf ./internal/db/repository
  -rm -rf ./web/.svelte-kit
  -rm -rf ./web/node_modules
  -find ./web/dist -mindepth 1 ! -name '.gitkeep' -delete

# Build the server docker image
image:
  docker build -t ghcr.io/jsmith212/bakery:latest .

# Start the application stack
start: image
  docker compose up -d

# Stop the application stack
stop:
  docker compose down

# Print this help message
help:
  just --list
