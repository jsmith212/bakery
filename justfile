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

# Build the server
build: web
  CGO_ENABLED=0 go build -o ./build/bakery .

# Run the server
run: web
  go run .

# Run unit tests (Go + frontend)
test: web web-test
  go test -v ./...

# Run the race detector
race: web
  go test -race ./...

# Run tests with coverage (frontend unit tests run first so CI gates on them)
coverage: web web-test
  mkdir -p build
  go test -v -coverprofile=build/coverage.out ./...
  go tool cover -func=build/coverage.out
  go tool cover -html=build/coverage.out -o build/coverage.html

# Run all code checks
check: check-format vet lint web-check

# Check the format of the code
check-format:
  if [ -n "$(gofmt -l .)" ]; then gofmt -l .; exit 1; fi

# Run the vet tool
vet: web
  go vet ./...

# Run the golangci-lint tool
lint: web
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
