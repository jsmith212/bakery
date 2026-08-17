# syntax=docker/dockerfile:1

FROM node:25-bookworm-slim AS web

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts

COPY web/ ./
RUN npm run build


# Minor-pinned, not patch-pinned: go.mod's `go` directive moves with toolchain
# patch releases, and the golang image sets GOTOOLCHAIN=local, so a patch-pinned
# tag here strands the image build the first time go.mod gets ahead of it (it
# did: go.mod 1.26.3 vs golang:1.26.1 broke the very first image job that ran).
# A real minor bump still fails loudly here, which is the right moment to notice.
FROM golang:1.26-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# web/dist is excluded from the build context, so the only copy of the frontend
# that can reach the embed is the one the node stage just produced. //go:embed
# all:dist resolves relative to web/embed.go, so this path is the contract.
COPY --from=web /src/web/dist ./web/dist

# internal/db/repository is sqlc output: generated, gitignored, and therefore not
# in the build context. Without this the build fails on a missing package.
RUN go tool sqlc -f internal/db/sqlc.yaml generate

# The stamp is the ONLY way the image can know its version: .dockerignore
# excludes .git/, so buildVersion()'s VCS fallback reads nothing here. CI passes
# the tag name (or sha-<sha> on main); a bare local `docker build` reports "dev".
ARG VERSION=""
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /bakery .


FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /bakery /bakery

# Public HTTP (8080), Prometheus metrics (9090, loopback by default -- see
# METRICS_ADDR), Bazel REAPI gRPC (9092, loopback by default -- see
# GRPC_ADDR). Documentation only: EXPOSE does not publish a port by itself,
# and a k8s Deployment's containerPort list is authoritative regardless.
EXPOSE 8080 9090 9092

ENTRYPOINT ["/bakery"]
CMD ["serve"]
