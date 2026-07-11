# Bakery

A multi-tenant build cache server: Yocto (sstate + hash equivalence + source premirror),
Bazel-protocol remote cache, ccache/sccache, and a Docker pull-through proxy. One Go binary +
Postgres.

See [`docs/design/DESIGN.md`](docs/design/DESIGN.md) for the architecture and milestone plan, and
[`CLAUDE.md`](CLAUDE.md) for the conventions and invariants this codebase holds to.

## Requirements

Go 1.26+, Node 24+, [`just`](https://github.com/casey/just), `pre-commit`, and Docker (with buildx)
for the image and compose stack.

## Quickstart

```sh
just bootstrap        # install the pre-commit hooks
cp stack.env.tmpl stack.env
just run              # builds the frontend, then runs the server
```

The server listens on `$HOST:$PORT` (default `0.0.0.0:8080`) and serves `GET /healthz` plus the
embedded SPA at `/`.

## Common tasks

`just` is the single source of truth; CI is a thin wrapper over it.

```sh
just              # list every recipe
just build        # frontend, then the static Go binary into build/bakery
just test         # unit tests
just check        # check-format, vet, lint
just format       # rewrite Go source in place
just start        # build the image and bring the compose stack up
just stop
```

The SvelteKit app in `web/` is built into `web/dist` and embedded into the binary with
`//go:embed all:dist`, so every recipe that compiles or vets Go builds the frontend first. There is
no separate "run the frontend" step for normal work; use `npm run dev` inside `web/` if you want
Vite's dev server.

## Configuration

The binary is configured by flags or environment variables (Kong binds both). `stack.env.tmpl` is
the template for the compose stack's `stack.env`, which is gitignored.
