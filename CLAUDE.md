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
- Conventional commits (commitizen), pre-commit hooks, `just check` = `check-format vet lint`.

## Invariants — do not violate these

**Storage ordering.** On delete: metadata first, then bytes. On create: bytes first, then metadata. Orphaned bytes are recoverable; dangling metadata is a permanent 500.

**`/ac/` is an opaque byte store.** Never parse it as a REAPI `ActionResult`. ccache, sccache, and moon-over-HTTP all put non-`ActionResult` payloads there. `/cas/` is content-addressed and MUST verify `key == sha256(body)`; `/ac/` MUST NOT. Getting this backwards breaks three clients at once.

**sstate misses return 404, never 403.** BitBake retries a 403 as a full-body GET. Never return a 200 with an empty body.

**`HEAD` is the sstate hot path, not `GET`.** BitBake fires a `BB_NUMBER_THREADS`-parallel HEAD storm over the whole setscene graph at build start.

**hashserv: one writer goroutine per connection, strictly-ordered responses.** The protocol has no request IDs. A single dropped or reordered response desynchronizes the connection permanently and *silently* — bitbake hangs forever with no error. If two goroutines can write to a connection, the design is already wrong.

**Parse ByteStream resource names by scanning for the `blobs`/`uploads`/`compressed-blobs` marker.** `instance_name` contains slashes (it's `{org}/{project}`). Never split positionally on `/`.

**Never re-serialize an OCI manifest.** A `json.Marshal` round-trip breaks `Docker-Content-Digest`, and it only reproduces on multi-arch index manifests — i.e. not in your test, and yes in production.

**The OCI upstream fetch requires a verified principal.** No exported way to construct one outside `internal/auth`. Otherwise we are an open relay serving Docker Hub with *our* rate-limit-bearing credentials.

**GC sweeps carry a write barrier** (`created_at < gc_run.started_at`) or a build that starts mid-GC gets its freshly-minted unihashes deleted.

**Prometheus labels use slugs, never keys or digests.** Label HTTP metrics on `r.Pattern`, not `r.URL.Path` — the latter creates one time series per sstate object and kills Prometheus inside a single build.

**`DEV_LOGIN_ENABLED` is settable only via env var / CLI flag.** No UI or API path may enable it. Defaults off.

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
