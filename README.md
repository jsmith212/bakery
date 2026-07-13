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

M1 needs Postgres: the server takes an advisory boot lock, applies its embedded migrations, and only
then binds a listener, so it will not start without a database. The local loop is:

```sh
just bootstrap        # install the pre-commit hooks

just db-up            # ephemeral Postgres on 127.0.0.1:5432; prints the export line below
export DB_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable
export DEV_LOGIN_ENABLED=true   # optional: seeds a dev site admin + an unauthenticated dev-login
                                # endpoint so you can get a session with no OIDC provider. Dev only.

just run              # builds the frontend, migrates, then `bakery serve`
```

`DB_URL` is required — `bakery serve` refuses to start without it — and `just run` does **not** read
`stack.env` (that file is the compose stack's, and its DB host is `db`, not localhost), so export the
variables into your shell. When you are done, `just db-down` removes the test database.

The server listens on `$HOST:$PORT` (default `0.0.0.0:8080`) and serves the control-plane API under
`/api/v1`, the embedded SPA at `/`, and the liveness/readiness probes `GET /healthz` and `GET /readyz`
(`/readyz` really does ping the pool). Metrics live on a **separate**, loopback-by-default listener
(`--metrics-addr`, default `127.0.0.1:9090`); `/metrics` leaks org and project slugs and byte counts,
so it is deliberately never on the public listener.

Migrations are applied automatically at boot, but you can also drive them directly with
`bakery migrate up`, `bakery migrate down --yes`, and `bakery migrate version` (each takes `DB_URL`).

To run the full stack in containers instead, `cp stack.env.tmpl stack.env`, fill in
`POSTGRES_PASSWORD` and the matching password in `DB_URL`, then `just start` (`just stop` to tear it
down). See the comments in `stack.env.tmpl` for the OIDC and group-map settings.

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

## Authorization

Roles live on four independent planes. One OIDC group lookup used to answer three questions at once;
it now answers one each.

| Plane | Question | Source of truth |
|---|---|---|
| Login gate | May this human use Bakery at all? | `login_groups` in the group map. **Empty or unset admits any successful OIDC auth** — the IdP has already decided who may authenticate. |
| Site role | May they run the platform? | Hybrid: `site_admin_groups` **or** an in-app grant (`ALLOW_LOCAL_SITE_ADMINS`, default on). |
| Org membership | Which orgs are they in? | Hybrid: the group map **or** an in-app grant. The effective role is the greater of the two. |
| Project role | What may they do in a project? | In-app only. Never group-mapped. |

So there are two ways into an org, and they compose: a directory group can grant membership centrally,
and an org admin can grant it in the console. **Login reconciliation writes only the claim-derived half**,
so a login can never delete a grant a human made. Removing someone's group removes their claim-derived
membership — and if that was their only source, the row goes, which cascades away their project roles and
every API key they held in that org. If they also hold a local grant, they stay.

Creating an org makes you its owner, in the same transaction (`ALLOW_SELF_SERVE_ORGS`, default on —
turn it off to restrict creation to site admins). So the first-run flow works with no directory at all:
sign in, create an org, create a project, mint a key.

**A groups claim we cannot *read* is a refused login.** "The IdP says you are in zero groups" and "we
could not read your groups" are different facts, and only the first is safe to act on: Azure AD replaces
a large groups claim with a `_claim_names` overage pointing at Graph, and reading that as "zero groups"
would cascade away a real user's entire access. An empty `groups: []` is fine and ordinary — it means
"local memberships only".

**Break-glass.** A fresh deployment with no `site_admin_groups` has no site admin, and every path to
making one requires already being one. `bakery user site-admin <email>` writes straight to the database
(it needs `DB_URL`) and has no HTTP or API path at all — reaching it takes infrastructure access, not a
session.

See `groups.example.json` for the group map, and the comments in `stack.env.tmpl` for the flags.
