# Bakery — Design & Implementation Plan

## Context

Bakery is a single-deployment, multi-tenant build cache server for a whole org's toolchain: Yocto (sstate + hash equivalence + source premirror), Bazel-protocol remote caching (for moon), ccache, and a Docker pull-through proxy. Today these are five separate, unmanaged, unauthenticated things — the Docker proxy alone is five `registry:2` containers with no volumes (cache evaporates on restart) and anonymous upstream auth (Docker Hub's 100-pulls-per-6h-per-IP bucket). There is no shared identity, no metrics, no retention policy, and no way to tell who is using what.

Bakery replaces all of it with one Go binary + Postgres: OIDC login, orgs → projects → cache backends, project-scoped API keys, Prometheus metrics, and local-disk or S3 storage. Easy to deploy, adequately scalable for a single org.

**Stack:** Go (stdlib-first, Kong, sqlc, golang-migrate, pgx) · SvelteKit + Tailwind embedded via `go:embed` · PostgreSQL · Docker multi-stage → minimal image.

---

## Decisions locked in this session

| Decision | Choice |
|---|---|
| Yocto mirror scope | Source premirror (`DL_DIR`/`PREMIRRORS`) only. **No** binary package feeds (ipk/deb/rpm) — that's a repository server, not a cache. |
| sstate write path | Authenticated HTTP `PUT` + a `bakery sstate push` CLI subcommand. A `bakery.bbclass` is a later option. |
| Docker clients | containerd / BuildKit / K8s only → the `?ns=<upstream>` convention, one endpoint for all upstreams. |
| Yocto releases | Scarthgap 5.0+ only. Buys us WebSocket transport, native hashserv auth, and GC. |
| S3 reads | Proxied through Bakery (not presigned redirects). Keeps every request authenticated and metered. |
| API keys | Project-scoped **and** per-user, read/write scoped, shown once, optional expiry. |
| Deployment | Single instance + Postgres. In-process LRU on hot paths. **No Valkey** — revisit only if we go multi-replica. |
| Frontend tests | Vitest (unit) + Playwright (e2e). |

**Approved dependencies:** `grpc-go`, `protobuf`, `bazelbuild/remote-apis` (pregenerated — no protoc), `coreos/go-oidc/v3`, `x/oauth2`, `prometheus/client_golang`, `klauspost/compress`, `aws-sdk-go-v2/s3`, `coder/websocket`, `google/go-containerregistry`, `x/net` (h2c), `alecthomas/kong`.

---

## Protocol ground truth (verified against upstream source, not docs)

These are the facts that shape the architecture. Each was confirmed by reading client source; several contradict the obvious assumption.

### Yocto sstate
- **No write protocol exists.** BitBake only does `HEAD` then `GET` (with `Range`, keep-alive). Every shop rsyncs. Our `PUT` + CLI is entirely our invention.
- **`HEAD` is the hot path, not `GET`.** `sstate_checkhashes` fires a `BB_NUMBER_THREADS`-parallel HEAD storm over the whole setscene graph at build start. This is what makes or breaks perceived performance.
- **Misses must return `404`, never `403`.** BitBake's `HTTPMethodFallback` retries a 403 as a full-body `GET`. Also: never return a `200` with an empty body.
- Object key: `[universal/]<h0h1>/<h2h3>/sstate:<PN>:<arch>:<PV>:<PR>:<pkgarch>:<ver>:<UNIHASH>_<task>.tar.zst` + `.siginfo`/`.sig` sidecars. **The hash embedded in the filename is the *unihash*, not the taskhash** — this is what couples sstate to hashserv.
- Colons arrive **percent-encoded** (`sstate%3Abusybox%3A…`). Route on the decoded path.
- `SSTATE_MIRRORS`'s `PATH` token expands to the full relative path, so an arbitrary multi-segment prefix works: `file://.* https://bakery/c/{org}/{proj}/sstate/PATH;downloadfilename=PATH` ✅

### Yocto source premirror
- Flat directory of basenames, incl. `git2_<host>.<path>.tar.gz`. `SOURCE_MIRROR_URL` with a multi-segment path works ✅
- **BitBake verifies `SRC_URI[sha256sum]` against premirror content.** A malicious premirror can only cause a fallback, not a compromise. (Contrast sstate, which has *no* checksums and is fully trusted — worth stating in our docs.)

### Yocto hashserv — the hard one
- **Not JSON-RPC.** Handshake `OEHASHEQUIV 1.1\n`, `needs-headers: <bool>\n`, headers, blank line. Then single-key JSON objects `{"<method>": <payload>}`. **No request IDs** — responses are strictly in-order, so one dropped or reordered response desynchronizes the connection *permanently and silently*.
- **Two framings.** WebSocket: one JSON doc per text message, no chunking. TCP: newline-delimited, plus a chunking protocol for messages ≥32767 chars (`{"chunk-stream": null}` sentinel, 32767-char chunks, bare empty line to terminate).
- **Stream mode has an evil wire detail:** server sends `"ok"` (JSON-quoted) to *enter*, and `ok` (raw, unquoted) to *exit*. Getting this wrong is a silent hang, not an error.
- Errors are `{"invoke-error":{"message":…}}` followed by **closing the connection** — there's no way to resynchronize without request IDs.
- **`BB_HASHSERVE_UPSTREAM` is PULL-ONLY.** The local hashserv reads from upstream but *never reports to it*. So the `BB_HASHSERVE="auto"` + upstream topology would mean **Bakery never receives a single hash report.** ⇒ We support and document *only* the direct topology.
- **Anonymous `ping` always succeeds upstream**, even with `@none` perms. So an RPC-level auth denial isn't discovered until mid-build and surfaces as an unhandled traceback. ⇒ **Reject unauthenticated connections at the HTTP upgrade with 401.**
- Upstream's `DEFAULT_ANON_PERMS` grants anonymous `@read` + `@report` + **`@db-admin`** (a stranger can `gc-sweep` your DB). **Do not copy this default.**
- Equivalence logic on `report`: insert the outhash; if the row is *new*, find a *different* taskhash with the same `(method, outhash)` and adopt the unihash of the **oldest** such row; else mint the reported one. `unihashes(method, taskhash)` is **write-once**.
- netrc gotcha to document: the `machine` token must be the **full `BB_HASHSERVE` URL** (`machine wss://bakery/c/org/proj/hashserv`), not the hostname. Exact string match.

### Bazel REAPI / moon / ccache / sccache — one endpoint, four clients
- moon defaults to **gRPC** REAPI v2. Its HTTP mode is degraded (no `FindMissingBlobs` ⇒ re-uploads every blob every build), so gRPC is not optional.
- moon calls: `GetCapabilities` (on connect — hard requirement), `Get/UpdateActionResult`, `FindMissingBlobs`, `BatchRead/UpdateBlobs`, `ByteStream.Read/Write`. It **never** calls `GetTree` or `QueryWriteStatus` → return `Unimplemented`.
- **gRPC cannot carry a URL path prefix** (tonic discards it). Project selector = REAPI `instance_name`, which **may contain slashes** and is passed verbatim by moon (unvalidated). So `instanceName: "acme/proj"` ✅
  ⇒ **Parse ByteStream resource names by scanning for the `blobs`/`uploads`/`compressed-blobs` marker — never split positionally on `/`.**
- moon sends `Authorization: Bearer` on **every** call including `GetCapabilities` ✅
- **ccache `layout=bazel` writes to `/ac/<40hex padded to 64hex>` — and never touches `/cas/`.** sccache's WebDAV mode is the same GET/PUT-blob-by-key.
  ⇒ **If we treat `/ac/` as an opaque blob store and never parse it as an `ActionResult`, we get ccache + sccache + moon-over-HTTP for free.** This is the single highest-leverage design decision in the project.
- Empty blob (`e3b0c442…`, size 0) must always report as present. Classic REAPI failure if missed.

### Docker / OCI
- containerd appends `/v2` to a configured path prefix itself, and sends `?ns=<upstream>` automatically whenever the mirror host ≠ the namespace. Not gated on anything ✅
- **BuildKit puts the prefix *after* `/v2`** (opposite of containerd). So we serve two path shapes.
- Manifests must be stored and served **byte-exact**. A single `json.Marshal` round-trip reorders keys and breaks `Docker-Content-Digest` — and it will *only* reproduce on multi-arch index manifests, i.e. not in your test and yes in production.
- We must issue our own downstream `401`/`WWW-Authenticate`, or we are an open relay serving Docker Hub with *our* credentials.

---

## Architecture

### Repo layout (extends `kbi`'s conventions; `main.go` at root, `internal/` packages)

```
main.go                    Kong CLI: serve | migrate | sstate push | downloads push | user
web/                       SvelteKit → static build, embedded
internal/
  config/                  Kong struct + env binding (replaces kbi's go-envconfig)
  db/                      pgxpool, migrations/ (go:embed, applied at boot), query/, sqlc.yaml
  auth/                    OIDC, sessions, API keys, Principal, Verifier
  storage/                 Store interface: local | s3 | instrumented decorator
  blob/                    Keyed blob service — dedup, refcount, LRU, singleflight, hit/miss metrics
  cache/
    backend.go             Backend / GRPCBackend / StreamBackend / Route / Deps contracts
    httpblob/              Shared GET/PUT/HEAD handler + per-backend Policy table
    sstate/  downloads/    (both are httpblob + a Policy)
    hashserv/              framing.go (highest-risk file), protocol.go, store.go
    bazel/                 grpc.go (REAPI), http.go (/ac /cas), resource.go
    ociproxy/
  api/                     Control-plane REST /api/v1
  middleware/  metrics/  gc/  server/  web/
```

**The load-bearing abstraction is `internal/blob`.** Every backend except hashserv routes through it, so hit/miss metrics, dedup, refcounting and the LRU are implemented exactly once. `cache.Deps` deliberately does **not** carry `*repository.Queries` — that structurally enforces "blob.Service is the only writer of object metadata."

**`internal/cache/httpblob` + a `Policy` table** is what makes four backends nearly free:

| mount | key grammar | Overwrite | Verify key == sha256(body) |
|---|---|---|---|
| sstate | `[universal/]<hh>/<hh>/sstate:…` | no | **no** |
| downloads | flat basename | no | **no** |
| bazel `/cas/` | 64 hex | no | **yes** |
| bazel `/ac/` | 64 hex | **yes** | **no** ← the trap |
| oci blobs | `<algo>:<hex>` | no | **yes** |

`/ac/` is the one mutable, opaque, unverified namespace. Getting `VerifyKeyIsDigest` backwards there breaks ccache, sccache, and moon-HTTP simultaneously.

### Addressing (verified against every client)

```
/cache/{org}/{project}/sstate/{path...}      SSTATE_MIRRORS = "file://.* https://…/PATH;downloadfilename=PATH"
/cache/{org}/{project}/downloads/{basename}  SOURCE_MIRROR_URL + INHERIT += "own-mirrors"
/cache/{org}/{project}/hashserv              wss://  (BB_HASHSERVE, direct topology only)
/cache/{org}/{project}/{ac,cas}/{hex}        ccache @layout=bazel · sccache · moon api=http
  gRPC REAPI (any path)                      instance_name = "{org}/{project}"
/cache/{org}/{project}/docker/v2/…?ns=       containerd
/v2/{org}/{project}/…?ns=                    BuildKit (prefix lands after /v2)
/api/v1/…   /healthz  /readyz   /   (SPA)
```

### Listener topology

**One port.** `h2c.NewHandler` wrapping a content-type demux (`application/grpc` + `ProtoMajor == 2` → gRPC server, else the HTTP mux). Not `cmux` (unmaintained, tangles graceful shutdown); not `connect-go` (moon speaks *real* gRPC with streaming ByteStream — don't bet the primary protocol on "should work"). Escape hatch: `--grpc-addr` to split gRPC onto its own listener if the shared path bottlenecks. `/metrics` on a **separate** `--metrics-addr` — serving it publicly leaks project names and sizes.

hashserv is a WebSocket upgrade on the shared mux (so it gets the same TLS + ingress + `Authorization` header). Raw TCP stays behind an off-by-default flag.

### Auth

One credential — a project-scoped API key — presented five ways:

| Backend | Presentation |
|---|---|
| sstate / downloads | HTTP Basic (bitbake supports nothing else) |
| ccache / sccache | Basic, or Bearer via `bearer-token` |
| moon gRPC | `authorization: Bearer` metadata (sent on every call) |
| hashserv | Native `auth` RPC (username + token), **401 at the WS upgrade if absent** |
| Docker | Registry Bearer token flow (`/v2/token`) |

Humans log in via OIDC (Google, Authelia, GitHub). `DEV_LOGIN_ENABLED` is settable **only** by env var / CLI flag — no UI or API path can enable it, and it defaults off.

All cache backends are auth-protected by default; auth can be disabled per-backend in the UI, **but writes always require a key regardless** (an unauthenticated write path is a cache-poisoning vector).

### Garbage collection — the sstate ↔ hashserv coupling

The asymmetry that dictates the design:

- Drop a unihash, keep the sstate object → dead bytes nobody can name. *Wasteful.*
- Drop the sstate object, keep the unihash → hashserv says "don't rebuild", bitbake 404s, rebuilds anyway. *Correct, but a silent performance cliff.*

⇒ **The unihash is the GC root; sstate objects are reachable-from-unihash. Always sweep hashserv before sstate, in one transaction per chunk.** Every sweep predicate carries a **write barrier** (`created_at < gc_run.started_at`) so a build that starts mid-GC can't have its freshly-minted unihashes deleted. Blob bytes are refcounted with a grace period **and** a `FOR UPDATE` + `refcount = 0` recheck (the grace period alone is luck, not correctness).

**Invariant: on delete, metadata first then bytes; on create, bytes first then metadata.** Orphaned bytes are always recoverable; dangling metadata is a permanent 500.

### Metrics

Label with **slugs, never keys or digests** (`{org, project, backend, kind}`). Headline series: `bakery_cache_requests_total{…,op,result}` (op = get|put|head|exists, result = hit|miss|stale|error), emitted from `blob.Service` so every backend is normalized for free. Plus storage gauges, `bakery_hashserv_*`, LRU/singleflight counters (ship these on day one — you'll need them to tune the HEAD path), GC and auth counters. HTTP middleware labels on `r.Pattern`, **not** `r.URL.Path` — the latter would create one time series per sstate object and kill Prometheus inside a single build.

---

## Milestones

Each is independently shippable and leaves the tree green.

- **M0 — Scaffolding.** Repo, Kong CLI, Justfile, pre-commit, commitizen, `.golangci.yml`, GitHub Actions (build/check/commit/image, ported from the gitea workflows), multi-stage Dockerfile (node → go → distroless), compose, `stack.env.tmpl`, SvelteKit skeleton embedded via `//go:embed all:dist` (the `all:` is **mandatory** — plain `//go:embed dist` silently skips SvelteKit's `_app/` and you ship a white page). Boots, serves `/healthz`.
- **M1 — Control plane + storage.** Postgres schema + sqlc; orgs, projects, users, roles, memberships, API keys; OIDC login + DEV_LOGIN; `storage.Store` (local, then S3) + `blob.Service` (dedup, refcount, LRU, singleflight). **Benchmark the HEAD path here, before any backend exists.**
- **M2 — Yocto sstate + downloads.** `httpblob` + two Policies + `bakery sstate push`. First thing a user can actually point bitbake at.
- **M3 — hashserv.** Framing-first, pure and DB-free, with an exhaustive table-driven suite. **Run upstream's own hashserv test suite and the real `bitbake-hashclient` against our server in CI — this is non-negotiable.**
- **M4 — Bazel REAPI (gRPC) + `/ac` `/cas` HTTP.** Ships moon, ccache, and sccache together.
- **M5 — Docker OCI pull-through proxy.** Byte-exact manifests, stale-while-revalidate, own 401 challenge.
- **M6 — GC, retention, quotas + UI polish.** (The GC *write barrier* and refcount tests land with M1, not here.)

**Frontend track (parallel):** design system + screens in Claude Design during M0–M1; implement in SvelteKit one milestone behind the backend so screens are built against real endpoints. Screens: login · org/project lists · project detail (overview / backends / members / keys / settings) · backend config + hit-rate charts · API keys (show-once) · admin. **The highest-value screen is a config snippet generator** — per backend, emit the exact `local.conf` / `workspace.yml` / `ccache.conf` / `hosts.toml` with a freshly-minted key baked in. Every one of these clients has a vicious config gotcha; this is what drives adoption.

---

## The three riskiest parts

1. **The hashserv protocol.** Stateful, no request IDs, two framings, two different encodings of "ok". A single extra newline in the stream-exit sequence hangs bitbake forever at "Checking sstate mirror object availability" with no error, four hours into someone's build. Mitigation: framing as pure code with exhaustive tests *before* touching Postgres; upstream's test suite in CI; **one writer goroutine per connection**, strictly-ordered response queue.
2. **The OCI proxy — open relay + digest integrity.** If any code path reaches the upstream fetch without a verified downstream principal, we serve Docker Hub with *our* rate-limit-bearing credentials to the internet. Fix is structural: `Upstream.Fetch` takes an `auth.Principal` and there's no exported way to build one outside `internal/auth`. Separately: never re-serialize a manifest.
3. **GC correctness under concurrency.** GC runs while builds are actively reporting. Three failure modes — mark-sweep with a live mutator (⇒ write barrier), refcount resurrection (⇒ grace period + `FOR UPDATE` recheck), crash between metadata and bytes (⇒ ordering invariant). All three *will* fire on a busy server.

Also cheap to defend now, expensive later: take a `pg_try_advisory_lock` at boot and **refuse to start a second instance** unless `--allow-multi-instance` — the negative-cache invalidation is exact only because there is one writer, and this converts a silent correctness bug into a startup error.

---

## Verification

- **Unit:** table-driven, colocated, stdlib only (matching kbi — no testify). hashserv framing gets an exhaustive suite.
- **Protocol conformance — the ones that actually prove it works:**
  - hashserv: upstream bitbake's own hashserv test suite + real `bitbake-hashclient` against Bakery, **in CI**.
  - Bazel: `bazelbuild/remote-apis-sdks`' client against our server; then a real `moon` run.
  - ccache: real `ccache` with `remote_storage = http://…/cache/o/p @layout=bazel`, assert a hit on rebuild.
  - Docker: real `containerd`/`nerdctl` pull through Bakery; assert `Docker-Content-Digest` matches on a **multi-arch** image (the case that breaks re-serialization).
  - Yocto: an actual `bitbake core-image-minimal` against a Bakery sstate + hashserv + premirror, twice — second build must be ~all setscene hits.
- **E2E:** Playwright over the SvelteKit UI (login → create org/project/backend/key → snippet generator).
- **Load:** simulate the sstate HEAD storm (thousands of parallel HEADs on pooled keep-alive connections) — this is the hot path and the one most likely to disappoint.

---

## Open items (not blocking; decide during implementation)

- `aws-sdk-go-v2/s3` vs `minio-go` — pick on first S3 contact; SDK is heavier but canonical.
- `bakery.bbclass` (push sstate on task completion) — deferred; the CLI is the v1 path.
- Binary package feeds (ipk/deb/rpm) — explicitly out of scope; revisit as its own project.
