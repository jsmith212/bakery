# Bakery

A multi-tenant build cache server: Yocto (sstate + hash equivalence + source premirror), Bazel-protocol remote cache (for moon), ccache/sccache, and a Docker pull-through proxy. One Go binary + Postgres.

## Read these first

- `docs/design/DESIGN.md` — the approved architecture and milestone plan. **Start here.**
- `docs/design/protocols/yocto.md` — wire-level sstate / hashserv / premirror reference.
- `docs/design/protocols/bazel-ccache-docker.md` — wire-level REAPI / ccache / OCI reference.
- `docs/design/protocols/client-config.md` — verified client config for every supported tool. Source of truth for the UI's snippet generator.

The protocol docs were built by reading upstream **client source**, not docs. Several findings contradict the obvious assumption. Do not "simplify" against them without re-verifying against source.

## Stack & conventions

Modeled on `../kbi` (adapt, don't copy blindly — kbi uses templ/htmx and go-envconfig; we use SvelteKit and Kong).

- **Go, stdlib-first.** `net/http` `ServeMux` (Go 1.22 method+pattern routing), `log/slog`. No chi/echo/gorilla.
- **Kong** (`alecthomas/kong`) for CLI + env binding. Replaces kbi's `sethvargo/go-envconfig`.
- **pgx/v5 + pgxpool** directly. Not `database/sql`.
- **sqlc** → `internal/db/repository/` (generated, gitignored). Schema is read from the migrations dir; no separate schema.sql.
- **golang-migrate**, migrations embedded via `go:embed` and applied at boot.
- **`go tool` directives** in go.mod pin codegen binaries (`go tool sqlc generate`). Modern replacement for `tools.go`.
- **SvelteKit** → static build → `//go:embed all:dist`. **The `all:` prefix is mandatory** — plain `//go:embed dist` silently skips SvelteKit's `_app/` directory and you ship a white page.
- **Tests:** table-driven, colocated, same-package, stdlib only. No testify, no gomock — hand-written fakes in `mocks_test.go`, matching kbi. Frontend: Vitest + Playwright.
- **Just** is the single source of truth; CI is a thin wrapper over it. Every recipe has a `#` doc comment.
- Conventional commits (commitizen), pre-commit hooks, `just check` = `check-format vet lint web-check`.
- **`internal/db/repository/` is generated and gitignored.** A fresh clone does not compile until `just generate` (`go tool sqlc`) has run. Every recipe, CI job and Docker stage that compiles depends on it. Bootstrap is: `npm ci` (in `web/`) → `just generate` → `go build ./...`, or just `just build`, which does all three.
- **`just test-db` runs the whole Go suite and fails if any test SKIPPED.** `dbtest` skips — it does not fail — when it can find neither docker nor `TEST_DB_URL`, which is right on a laptop and catastrophic in CI. This is the recipe CI runs; a suite that silently skips is not a passing suite.

## Backend: what exists (M1 has landed)

`internal/db` (pool + embedded migrations + boot lock + a `Store` whose `Tx` really rebinds sqlc's `Queries` onto the transaction) · `internal/db/dbtest` (a private migrated Postgres per test) · `internal/slug` · `internal/storage` (**local disk only — S3 is deferred**) · `internal/blob` (dedup, refcount, sharded LRU, singleflight, the headline metrics) · `internal/cache` (`Route`/`Deps`/`Backend`: the seams M2+ plug into — **no cache backend serves traffic yet**) · `internal/metrics` · `internal/auth` · `internal/api` (`/api/v1`) · `internal/cli` (the binary is also the API client) · `internal/server`.

`server.Boot` is the wiring, and it is what `bakery serve` runs *and* what the end-to-end test boots — not a lookalike. Boot order is load-bearing: connect and **ping** → advisory boot lock → migrate → metrics → auth → `EnsureOrgs` → `SeedDevLogin` → API → bind both listeners → serve. `bakery serve --headless` serves the API and metrics and simply does not route the SPA.

The auth model, the full `/api/v1` route table, and what M1 did *not* build are recorded in DESIGN.md under "M1 as landed". Read it before adding an endpoint.

## Invariants — do not violate these

**Storage ordering.** On delete: metadata first, then bytes. On create: bytes first, then metadata. Orphaned bytes are recoverable; dangling metadata is a permanent 500.

**`/ac/` is an opaque byte store.** Never parse it as a REAPI `ActionResult`. ccache, sccache, and moon-over-HTTP all put non-`ActionResult` payloads there. `/cas/` is content-addressed and MUST verify `key == sha256(body)`; `/ac/` MUST NOT. Getting this backwards breaks three clients at once.

**sstate misses return 404, never 403.** BitBake retries a 403 as a full-body GET. Never return a 200 with an empty body.

**`HEAD` is the sstate hot path, not `GET`.** BitBake fires a `BB_NUMBER_THREADS`-parallel HEAD storm over the whole setscene graph at build start.

**hashserv: one writer goroutine per connection, strictly-ordered responses.** The protocol has no request IDs. A single dropped or reordered response desynchronizes the connection permanently and *silently* — bitbake hangs forever with no error. If two goroutines can write to a connection, the design is already wrong.

**Parse ByteStream resource names by scanning for the `blobs`/`uploads`/`compressed-blobs` marker.** `instance_name` contains slashes (it's `{org}/{project}`). Never split positionally on `/`.

**Never re-serialize an OCI manifest.** A `json.Marshal` round-trip breaks `Docker-Content-Digest`, and it only reproduces on multi-arch index manifests — i.e. not in your test, and yes in production.

**The OCI upstream fetch requires a verified principal.** No exported way to construct one outside `internal/auth`. Otherwise we are an open relay serving Docker Hub with *our* rate-limit-bearing credentials.

**GC sweeps carry a write barrier, and it has TWO halves.** `created_at < gc_run.started_at` **and** `pg_visible_in_snapshot(live_xid, gc_runs.snapshot)`. The timestamp alone is *not* sufficient and it is not the invariant: a row inserted by a transaction that began before the run but committed after it satisfies the timestamp predicate and must still be spared. `TestGCWriteBarrierSparesAConcurrentBuild` asserts precisely that the timestamp predicate *does* select the row and the sweep spares it anyway. Drop the snapshot half and a build that starts mid-GC loses its freshly-minted unihashes.

**Prometheus labels use slugs, never keys or digests.** Label HTTP metrics on `r.Pattern`, not `r.URL.Path` — the latter creates one time series per sstate object and kills Prometheus inside a single build. Read `r.Pattern` **after** `next.ServeHTTP` returns: `ServeMux` sets it in place, so a middleware outside the mux sees `""` before, labels every request `""`, and still passes a naive smoke test.

**`/metrics` is served on `--metrics-addr` and NOWHERE else.** Its own `http.Server`, its own listener, its own graceful shutdown, loopback by default. It exposes every org and project slug and how many bytes each stores; on the public listener that is the customer list. Both listeners bind before either serves, and if one dies both come down — a live cache with a dead metrics listener is a silently unmonitored server. The assertion that proves it is on the *body*, never the status code: the SPA catch-all answers 200 for everything.

**`/readyz` really pings the pool; `/healthz` does not.** A readyz that returns 200 with a dead database keeps the node in rotation while every request behind it 500s, which is worse than having no readyz. Conflating the two in the other direction makes an orchestrator restart a healthy binary because Postgres blinked.

**Boot takes `pg_try_advisory_lock` and refuses a second instance** unless `--allow-multi-instance`. That refusal is what makes the in-process LRU and route cache sound. It is taken **before** the migrations, so two instances starting together cannot race through the same migration.

**`DEV_LOGIN_ENABLED` is settable only via env var / CLI flag.** No UI or API path may enable it. Defaults off. When off, the endpoint is not registered at all and **404s** — a 403 confirms the endpoint exists and tells a scanner what to come back for.

**Login reconciliation fails closed.** Site and org roles are derived from OIDC group claims on every login. A login that resolves to no groups (Azure AD's `_claim_names` overage does this to real, correctly-configured users) or to no mapped orgs is **refused**. It must never be treated as "zero memberships": passing an empty keep-set to the reconciler deletes every org membership the user has, cascading to their project roles and every API key they hold in those orgs, irreversibly.

**API-key scope is capped at grant time, and revoked in the same transaction as a role downgrade.** Validating a key is one zero-join index-only probe — deliberately no join onto `project_memberships`, because that would put a second query on the sstate HEAD storm. The cap and the revocation are what make that join-free grant safe. An API-key principal never inherits its owner's site admin: a delegation must not become a master key.

**`auth.Principal` is unforgeable by construction** — an interface sealed with an unexported method, no exported constructor, no usable zero value. Not a convention; the compiler enforces it (`internal/auth/forgery/` compiles four attacks on purpose and asserts they are rejected).

**Blob paths stay READ COMMITTED.** Under REPEATABLE READ the snapshot is taken at the transaction's *first* statement — the advisory-lock acquisition, before the lock is granted — so the post-lock `SELECT` reads a stale world and the `FOR UPDATE` + `refcount = 0` recheck protects nothing. A `pgx.TxOptions{IsoLevel: RepeatableRead}` anywhere on a blob path is a silent correctness bug.

**The LRU caches NEGATIVE results and is sharded.** On the first build against an empty cache *every* HEAD is a miss, so a positive-only cache sends the entire setscene graph to Postgres on every build — and no test that pre-populates the repo will ever show it. And a single-mutex LRU gets *slower* as parallelism rises (measured 170ns@8 → 246ns@64), which is exactly backwards under a `BB_NUMBER_THREADS` HEAD storm.

**`ServeMux` panics you will meet when the cache routes land.** `/cache/{org}/{project}/{kind}/{key}` alongside `/cache/{org}/{project}/sstate/{path...}` panics at registration — register `ac`/`cas` as **literal** segments. A method-less `/v2/{org}/{project}/` alongside `GET /` panics too. This is why the SPA is registered as a method-less `/` (it does its own method check) and `/api/v1/` mounts cleanly beside it.

## Frontend: how it's structured

The Claude Design handoff has **landed**. The design system is ported into the SvelteKit app under `web/` — build against it, don't redesign it.

- **Tailwind v4, CSS-first.** No `tailwind.config.js`. Tokens are CSS variables in `web/src/lib/styles/tokens/` (colors dark on `:root`, light under `[data-theme="light"]`), mapped onto Tailwind's `--color-*`/`--radius-*`/font namespaces via an `@theme` block in `web/src/app.css`. Utilities like `bg-bg-1 text-text-2 border-border-0 rounded-2` resolve to those vars, so flipping `<html data-theme>` recolors live with no rebuild. **No raw hex in markup**; never use `dark:` variants.
- **Theming** is `web/src/lib/theme.ts` (`dark|light|system`, persisted to `localStorage["bakery-theme"]`, default `system`), plus a no-FOUC script in `app.html`. The ConsoleNav footer toggle and User Settings "Appearance" control drive the same store — one source of truth.
- **Component library** in `web/src/lib/components/<group>/` (buttons, inputs, badges, table, navigation, feedback, content, data). These rebuild the `.bk-*` recipes as `.svelte` components using token utilities. Reuse them; do not ship `.bk-*` classes or reintroduce inline primitives. Status is a typographic glyph + color, never an icon: `●` hit, `✕` miss, `▲` stale, `○` idle, `∅` empty.
- **Routes**: a `(console)` group layout renders `<ConsoleNav/>` + the page; `/login` is full-screen outside it; `/` redirects to `/overview`. adapter-static SPA (`ssr=false`), built to `web/dist`, embedded via `//go:embed all:dist`.
- **All screens are built with mock data — API wiring is still pending.** When you add a backend, replace the in-component mock arrays with real data; the visual layer stays as-is. `web/FOUNDATION.md` is the binding contract for the utility vocabulary and component prop APIs.
- Fonts still load via a Google Fonts `@import`; production should self-host the woff2 for the offline embedded console (not a blocker).

Voice/fidelity is fixed by the design system: sentence case, terse and technical, no emoji, no exclamation points, second person; metrics in tabular numerals, mono for hashes/keys/config.

## Domain model

`Organization` → `Project` → `Cache Backend` (sstate | downloads | hashserv | bazel | oci). Users have org and project roles. API keys are project-scoped and per-user, read/write-scoped, shown once.

**sstate and hashserv are hard-coupled**: the sstate object filename embeds the *unihash*, so the two must be scoped, retained, and garbage-collected together. The unihash is the GC root; sstate objects are reachable-from-unihash. Always sweep hashserv before sstate.

## Addressing

```
/cache/{org}/{project}/sstate/{path...}
/cache/{org}/{project}/downloads/{basename}
/cache/{org}/{project}/hashserv                 (wss upgrade)
/cache/{org}/{project}/{ac,cas}/{hex}
/cache/{org}/{project}/docker/v2/...?ns=        (containerd)
/v2/{org}/{project}/...?ns=                     (BuildKit — prefix lands after /v2)
  gRPC REAPI: project comes from instance_name = "{org}/{project}"
/api/v1/...  /healthz  /readyz  /  (SPA)
```

Reserved slugs (forbid at org/project creation): `blobs`, `uploads`, `actions`, `actionResults`, `operations`, `capabilities`, `compressed-blobs`, `ac`, `cas`, `v2`, `api`, `cache`.
