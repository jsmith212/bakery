# syntax=docker/dockerfile:1

FROM node:25-bookworm-slim AS web

WORKDIR /src/web

COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts

COPY web/ ./
RUN npm run build


FROM golang:1.26.1-bookworm AS build

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

RUN CGO_ENABLED=0 go build -o /bakery .


FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /bakery /bakery

EXPOSE 8080

ENTRYPOINT ["/bakery"]
CMD ["serve"]
