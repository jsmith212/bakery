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

**S3 client: `aws-sdk-go-v2/s3`, decided.** Not `minio-go` — not used here. S3-compatible endpoints (Ceph, Garage, R2) are still reachable via `BaseEndpoint` + `UsePathStyle`.

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
- **Anonymous `ping` always succeeds upstream**, even with `@none` perms. ⇒ ~~Reject unauthenticated connections at the HTTP upgrade with 401.~~ **This conclusion was wrong and is superseded — see "M3: verified before implementation" below. Deny auth IN-BAND, never at the upgrade.**
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

**~~One port.~~ SUPERSEDED by M4 (see "M4 as landed").** The plan was `h2c.NewHandler` wrapping a content-type demux. Empirical spikes killed it: `grpc.Server.ServeHTTP` has an unimplemented `Drain`, so `GracefulStop` panics on the shared path when an RPC is in flight; and a shared h2c port lets an ingress silently break the M3 hashserv WebSocket (coder/websocket can't serve WS-over-HTTP/2). **gRPC now serves on a DEDICATED `--grpc-addr` listener** (a third listener beside public + metrics), which is where `GracefulStop` actually drains. `/metrics` on a **separate** `--metrics-addr` — serving it publicly leaks project names and sizes. Three listeners, three protocols; all bind before any serves, and if one dies all come down.

hashserv is a WebSocket upgrade on the **public** mux (same TLS + ingress + `Authorization`). Raw TCP stays behind an off-by-default flag. Note the deployment constraint the dedicated gRPC listener creates: the public port stays HTTP/1.1 (so hashserv survives), and REAPI clients take an explicit `grpc://host:port` endpoint — the tenant still rides in the REAPI `instance_name = "{org}/{project}"`, so the addressing scheme is unchanged; only the port moves.

hashserv is a WebSocket upgrade on the shared mux (so it gets the same TLS + ingress + `Authorization` header). Raw TCP stays behind an off-by-default flag.

### Auth

One credential — a project-scoped API key — presented five ways:

| Backend | Presentation |
|---|---|
| sstate / downloads | HTTP Basic (bitbake supports nothing else) |
| ccache / sccache | Basic, or Bearer via `bearer-token` |
| moon gRPC | `authorization: Bearer` metadata (sent on every call) |
| hashserv | Native `auth` RPC (username + token). Deny **in-band** with `{"invoke-error":…}` + close. **Never 401 the upgrade** — see the M3 note |
| Docker | Registry Bearer token flow (`/v2/token`) |

Humans log in via OIDC (Google, Authelia, GitHub). `DEV_LOGIN_ENABLED` is settable **only** by env var / CLI flag — no UI or API path can enable it, and it defaults off.

All cache backends are auth-protected by default; auth can be disabled per-backend in the UI, **but writes always require a key regardless** (an unauthenticated write path is a cache-poisoning vector).

### Garbage collection — the sstate ↔ hashserv coupling

The asymmetry that dictates the design:

- Drop a unihash, keep the sstate object → dead bytes nobody can name. *Wasteful.*
- Drop the sstate object, keep the unihash → hashserv says "don't rebuild", bitbake 404s, rebuilds anyway. *Correct, but a silent performance cliff.*

⇒ **The unihash is the GC root; sstate objects are reachable-from-unihash. Always sweep hashserv before sstate, in one transaction per chunk.** Every sweep predicate carries a **write barrier** so a build that starts mid-GC can't have its freshly-minted unihashes deleted. M1's schema shows the barrier has **two halves, and the timestamp alone is not enough**: `created_at < gc_run.started_at` *plus* `pg_visible_in_snapshot(live_xid, gc_runs.snapshot)`. A row inserted by a transaction that began before the GC run but committed after it satisfies the timestamp predicate and must still be spared; `TestGCWriteBarrierSparesAConcurrentBuild` asserts exactly that, and it is the snapshot half that saves it. Blob bytes are refcounted with a grace period **and** a `FOR UPDATE` + `refcount = 0` recheck (the grace period alone is luck, not correctness).

**Invariant: on delete, metadata first then bytes; on create, bytes first then metadata.** Orphaned bytes are always recoverable; dangling metadata is a permanent 500.

### Metrics

Label with **slugs, never keys or digests** (`{org, project, backend, kind}`). Headline series: `bakery_cache_requests_total{…,op,result}` (op = get|put|head|exists, result = hit|miss|stale|error), emitted from `blob.Service` so every backend is normalized for free. Plus storage gauges, `bakery_hashserv_*`, LRU/singleflight counters (ship these on day one — you'll need them to tune the HEAD path), GC and auth counters. HTTP middleware labels on `r.Pattern`, **not** `r.URL.Path` — the latter would create one time series per sstate object and kill Prometheus inside a single build.

---

## Milestones

Each is independently shippable and leaves the tree green.

- **M0 — Scaffolding. ✅ DONE.** Repo, Kong CLI, Justfile, pre-commit, commitizen, `.golangci.yml`, GitHub Actions (build/check/commit/image, ported from the gitea workflows), multi-stage Dockerfile (node → go → distroless), compose, `stack.env.tmpl`, SvelteKit skeleton embedded via `//go:embed all:dist` (the `all:` is **mandatory** — plain `//go:embed dist` silently skips SvelteKit's `_app/` and you ship a white page). Boots, serves `/healthz`.
- **M1 — Control plane + storage. ✅ DONE** (see "M1 as landed" below). Postgres schema + sqlc; orgs, projects, users, roles, memberships, API keys; OIDC login + DEV_LOGIN; `storage.Store` (**local only — S3 is deferred**) + `blob.Service` (dedup, refcount, LRU, singleflight). The HEAD-path benchmark and its three gates landed with it, before any backend exists.
- **M1.5 — Hybrid role model. ✅ DONE** (see "M1.5 as landed" below). ([spec](specs/2026-07-12-hybrid-role-model.md)) M1 derives site *and* org roles purely from OIDC group claims, which forces a group-per-org into the directory and leaves a freshly-created org unusable by its own creator. M1.5 splits the one group lookup that was answering three questions into four planes: a **login gate** (`login_groups`, independent of org mapping — empty admits any successful OIDC auth), a **hybrid site role**, **hybrid org membership** (claims *and* in-app grants, effective role `greatest(oidc, local)` computed by the database), and **project roles in-app only**. Org creation grants the creator a local owner role. Fail-closed moves from *"no orgs ⇒ refuse"* to *"an **unreadable** groups claim ⇒ refuse and reconcile nothing"* — empty is not unreadable, and only the former is safe to act on. Also fixes `RevokeAPIKeysForMembership`, which has never worked (pgx cannot encode an enum array without the type registered on the pool). **Lands before the SPA→API wiring wave**, which depends on its membership screens.
- **M2 — Yocto sstate + downloads. ✅ DONE** (see "M2 as landed" below). `httpblob` + two Policies + `bakery sstate push`. First thing a user can actually point bitbake at.
- **M3 — hashserv. ✅ DONE** (see "M3 as landed" below). ([spec](specs/2026-07-13-m3-hashserv.md)) WebSocket-only, framing-first, one writer goroutine per connection, auth denied in-band. Upstream bitbake's own hashserv suite **and** the real `bitbake-hashclient` both run against Bakery in CI, and the gate earned its keep on day one — it caught a real divergence the design review had not.
- **M4 — Bazel REAPI (gRPC) + `/ac` `/cas` `/sccache` HTTP. ✅ DONE** (see "M4 as landed" below). Ships moon (gRPC), Bazel (gRPC + HTTP), ccache and sccache together.
- **M5 — Docker OCI pull-through proxy.** Byte-exact manifests, stale-while-revalidate, own 401 challenge.
- **M6 — GC, retention, quotas + UI polish.** (The GC *write barrier* and refcount tests land with M1, not here.)

### M3: what the pre-implementation review got right, and what it got wrong

M3 was preceded by a source-reading review that produced four findings. It is worth recording how they
fared, because the score is the argument for the CI gate.

**Findings 1, 2 and 4 held exactly, and all three are now enforced by tests.**

1. **Auth must be denied IN-BAND. A 401 at the WS upgrade is the SILENT failure, not the loud one.** 401 → `websockets` raises `InvalidStatus` → `asyncrpc/client.py` re-raises as `ConnectionError` → `_send_wrapper` retries 3× then re-raises → **`siggen.py` catches `ConnectionError`, `bb.warn()`s, and sets `unihash = taskhash`**. The build *completes*, with a silently degraded cache — sstate object filenames embed the unihash, so every task the server would have remapped now misses its object. The loud path is `{"invoke-error":…}` + close: `InvokeError` is in no retry tuple and is caught nowhere on the build path, so it halts the build. `TestAuthIsDeniedInBand` and the conformance gate both assert it.
2. **A stock client sends NO `Authorization` header on the upgrade.** Credentials go in-band via the `auth` RPC. Gating the upgrade rejects the connection before the client can present anything. `TestUpgradeCarriesNoCredentialAndStillSucceeds` asserts the upgrade returns 101 with no credential even when `read_auth_required` is set.
4. **`coder/websocket`'s default 32768-byte read limit; `SetReadLimit` is mandatory.** `TestHugeSiginfo` sends upstream's own 128 KiB `outhash_siginfo` in one frame.

**Finding 3 was half right, and the half it missed set the whole auth design.** It correctly saw that
`setUp`/`tearDown` call `client.remove()` and that the 15 `test_auth_*` tests skip without
`BB_TEST_HASHSERV_USERNAME`. It concluded the gate therefore "holds without letting a cache client mint
credentials." But it missed that `setUp`'s client is **anonymous** unless those vars are set — so running
the suite anonymously would demand an anonymous `@db-admin`, i.e. an unauthenticated write, which the
invariants forbid outright. It also undercounted the skips (18, not 15: three more call `start_server()`).
The resolution: run **authenticated** with a real `bkry_` write key and exclude the deliberately-absent
surface by name. See "M3 as landed".

**Two things the review missed entirely, both of which would have shipped as silent, build-hanging bugs:**

5. **Over WebSocket, the handshake is ONE MESSAGE PER LINE with no newlines.** `protocols/yocto.md`
   documented it as "each line `\n`-terminated" without saying that is **stream-transport-only**.
   `setup_connection` sends through the polymorphic `send()`: `StreamConnection` appends `\n`,
   `WebsocketConnection` does not — it emits one WS message per line, and the header terminator is an
   **empty message**. A server built to the documented spec waits forever for a frame that never comes,
   and so does the build. The doc is now fixed.

6. **The 64-hex `UNIHASH_REGEX` does not exist in bitbake 2.8.** It is a **2.10+** addition — which
   `yocto.md`'s own version table already said, while §2.1 and §2.5 of the same document claimed
   otherwise. Scarthgap's `handle_report` validates nothing, and upstream's own suite reports **40-hex**
   unihashes. Enforcing 64 refused a legitimate Scarthgap client and **failed 11 of the 17 gate tests**.
   We accept `^[0-9a-f]{1,64}$` — the union of every supported release, still hex-only because the
   unihash lands in an sstate *filename*.

   The method lesson is sharper than the bug: the finding came from reading git branch `2.8`, which
   tracks later 2.8.x, while the build pins tag **`2.8.0`**. **Verify against the tree the build actually
   pins.** (A third bug — `get-outhash` keying on `taskhash`, when upstream keys on `(method, outhash)`
   alone — was caught in test rather than by the gate; it would have delivered an open mirror that served
   every request, looked healthy, and produced *zero* equivalence.)

**The score is the point.** Three of four findings held; one was half wrong; two of the three bugs that
actually shipped into the branch were invisible to source review and were caught only by running the real
client. This is why the gate is non-negotiable, and why "verified by reading" is not the same as
"verified".

**Inherited gap, unrelated to the protocol:** `cache_objects.gc_root` — the unihash GC root — was promised by migration `000006` ("it lands in M2 with sstate, before any production rows exist") and **never landed**. M2 shipped and sstate rows now exist. It stays deferred (it is derivable by parsing the unihash out of the sstate key at sweep time), but M6 must not assume the column is there. Note that `hashserv_unihashes` and `hashserv_outhashes` **do** carry both halves of the GC write barrier (`created_at` *and* `live_xid`) from birth, precisely so M6 does not inherit the same problem twice.

### M1 as landed

The plan above is what M1 aimed at. This is what it *is*, so the next session onboards from the code rather than from the intention.

**Packages.** `internal/db` (pgxpool + embedded golang-migrate + the boot advisory lock + `Store`, whose `Tx` really rebinds sqlc's `Queries` onto the `pgx.Tx`) · `internal/db/dbtest` (a private, migrated Postgres per test, cloned from a template, over docker or `TEST_DB_URL`) · `internal/slug` (mirrors the DB's `bakery_slug_ok`, so the reserved-slug denylist cannot drift) · `internal/storage` (content-addressed local disk + an instrumented decorator) · `internal/blob` (dedup, refcount, 64-shard LRU with **negative caching**, singleflight) · `internal/cache` (`Route`/`Deps`/`Backend` — the seams M2+ plug into; no backend exists yet) · `internal/metrics` · `internal/auth` · `internal/api` · `internal/cli` (the binary is also the API client) · `internal/server` (the wiring: `server.Boot` is what `bakery serve` runs, and what the end-to-end test boots).

**Storage is LOCAL ONLY. S3 is deferred** — there is no storage-driver column in the schema, no S3 code in the binary, and `--storage-dir` is the only knob. `storage.Store` is the interface a future S3 driver implements; nothing above it knows which driver it has.

**Serve modes.** `bakery serve` = SPA + `/api/v1` + `/healthz` + `/readyz` on the public listener, `/metrics` on `--metrics-addr` (loopback by default). `bakery serve --headless` = the same minus the SPA, which is simply **not routed** (an unknown path is a 404 from the mux, never a 500). `/readyz` really pings the pool; `/healthz` does not, because liveness and readiness must not be the same answer.

**Boot order** (`internal/server/boot.go`): connect and **ping** → take `pg_try_advisory_lock` (refuse a second instance unless `--allow-multi-instance`) → migrate → metrics → auth → `EnsureOrgs` → `SeedDevLogin` → API → bind both listeners → serve. The lock is taken *before* the migrations so two instances starting together cannot race through the same migration.

**The API surface** (`/api/v1`; every route declares its required role at registration, and a handler never sees a path id — `guard` resolves slugs to ids and authorizes them):

| Route | Access |
|---|---|
| `GET /auth/config` · `GET /auth/login` · `GET /auth/callback` · `POST /auth/logout` | public |
| `POST /auth/dev-login` | public, **and only registered when `DEV_LOGIN_ENABLED`** (otherwise the route does not exist: a 404, not a 403) |
| `GET /me` | any principal |
| `GET /orgs` · `POST /orgs` | authenticated · **site admin** |
| `GET|PATCH|DELETE /orgs/{org}` | org view · org admin · org owner |
| `GET /orgs/{org}/members` · `PUT|DELETE .../members/{user}` | org view · **409, always** — org roles are claim-derived *(M1 only — **M1.5 makes these real writes**; see the route table below)* |
| `GET|POST /orgs/{org}/projects`, `GET|PATCH|DELETE /orgs/{org}/projects/{project}` | org view · org admin · project read/admin |
| `GET|PUT|DELETE /orgs/{org}/projects/{project}/members[/{user}]` | project read · project admin |
| `GET|POST|DELETE /orgs/{org}/projects/{project}/keys[/{key}]` | project read |
| `GET|POST|GET|PATCH|DELETE /orgs/{org}/projects/{project}/backends[/{kind}]` | project read · project admin |

Every non-2xx carries the same envelope, `{"error":{"code","message","field?"}}`, with a **closed** code vocabulary. Branch on `code`, never on `message`.

**The auth model, as built.**

- Humans: OIDC authorization-code flow (state + nonce + PKCE; we verify the nonce ourselves, because go-oidc does not) → an scs session in Postgres, over a hand-written pgxpool store.
- CLI: OIDC **device grant**; `bakery login` caches tokens 0600 under `~/.config/bakery`, presents the ID token as a Bearer, and the server verifies it per request.
- **Site and org roles come from OIDC group claims and are reconciled on EVERY login, fail-closed.** A login that resolves to no groups or no mapped orgs is *refused* — it is not treated as "zero memberships", which would delete every membership the user has. This is why the org-membership write endpoints return 409: an org role hand-edited here is either reverted at the next login or grants authority the IdP never granted. **A brand-new org therefore has no members until the group map names it — not even the site admin who created it.**
  > **⚠ SUPERSEDED BY M1.5.** That last sentence *is* the dead-end M1.5 exists to abolish, and the fail-closed rule as stated here is now wrong in its second half: a local-only user legitimately resolves to **zero mapped orgs** and must be admitted. The rule moved to *"an **unreadable** groups claim ⇒ refuse and reconcile nothing"*. See "M1.5 as landed".
- **Project roles are managed in-app** and are the only roles a human edits. *(Still true under M1.5 — deliberately. They are the one plane that never comes from a claim.)*
- API keys are `bkry_` + 256 random bits, stored as raw SHA-256, shown exactly once, project-scoped **and** per-user, capped at the holder's project role *at creation* (validation deliberately does not join `project_memberships` — that would put a second probe on the sstate HEAD storm). An API-key principal never inherits its owner's site admin.
- `auth.Principal` is an interface sealed by an unexported method: it cannot be constructed or implemented outside `internal/auth`. M5's OCI upstream fetch depends on that.

### M1.5 as landed

[Spec.](specs/2026-07-12-hybrid-role-model.md) M1's single group lookup answered three questions at once — *may this human use Bakery? may they run the platform? which orgs are they in?* — so the only way to answer the first was to answer the third. M1.5 splits them into **four independent planes**:

| Plane | Question | Source of truth |
|---|---|---|
| **Login gate** | May this human use Bakery at all? | `login_groups` (claims). **Empty/unset ⇒ any successful OIDC auth is admitted.** |
| **Site role** | May they run the platform? | Hybrid: `site_admin_groups` **+** local grants. |
| **Org membership** | Which orgs are they in, and how? | Hybrid: the group map **+** local grants. Effective = `greatest(oidc_role, local_role)`. |
| **Project role** | What may they do in a project? | **In-app only.** Unchanged from M1, deliberately. |

**Schema (000008, 000009).** `org_memberships` gains `oidc_role`, `oidc_group`, `local_role`, `granted_by`, `granted_at`; `role` becomes `org_role GENERATED ALWAYS AS (greatest(oidc_role, local_role)) STORED` — the enums are declared in privilege order, so `greatest()` *is* the max-wins rule and no Go code can get it wrong. `users` gets the same treatment (`site_role_oidc`, `site_role_local`, `site_granted_by`, `site_granted_at`, generated `site_role`). Two CHECKs hold the shape: a row must have at least one source, and `granted_at` is present iff `local_role` is.

**It is ONE ROW per `(user, org)`, and that is the whole architecture.** `project_memberships` carries `FOREIGN KEY (user_id, org_id) REFERENCES org_memberships (user_id, org_id) ON DELETE CASCADE`, and that cascade — org membership → project roles → API keys — is what revokes every key a user holds in an org when they leave it, which is what makes the join-free key validation on the sstate HEAD storm safe. It needs `(user_id, org_id)` UNIQUE. A `PRIMARY KEY (user_id, org_id, source)` would destroy it and silently reopen the revocation hole. **The two sources are columns. Never rows.**

**Reconciliation.** The reconciler writes **only** the `oidc_*` columns — so a login *cannot* clobber a local grant, structurally rather than by convention. When claims no longer justify a membership it NULLs `oidc_role`/`oidc_group` and deletes the row **iff `local_role IS NULL`** (which fires the cascade). Fail-closed **moved**: *"resolves to no mapped orgs ⇒ refuse"* is gone (a local-only user legitimately maps to zero orgs, and refusing them was the bug); it is replaced by **"an *unreadable* groups claim ⇒ refuse the login and reconcile nothing"**. `groups: []` is admissible and means "local memberships only"; an absent claim, or Azure AD's `_claim_names`/`_claim_sources` overage, is *not knowing* — and reconciling *that* as "zero groups" would cascade a real user's entire access away. `auth.Identity` carries `GroupsPresent` because a bare `[]string` cannot express the difference. The rule holds whether or not the login gate is enabled.

**New/changed API:**

| Route | Access |
|---|---|
| `POST /orgs` | **any signed-in user** (`--allow-self-serve-orgs`, default on; off ⇒ site admin). The creator gets a **local owner grant in the same transaction** — this is the fix for M1's dead-end. Never reachable by an API key. |
| `PUT|DELETE /orgs/{org}/members/{user}` | org admin. **Real writes now** (M1: 409 always). `PUT` writes `local_role` + provenance; `DELETE` clears the local half only, and **says so in the response when a claim-derived membership survives it** — LDAP owns that one and the API cannot remove it. |
| `GET /site-admins` · `PUT|DELETE /site-admins/{user}` | site admin (`--allow-local-site-admins`, default on). The listing reports **source** — `ldap: platform-admins` vs `local: granted by …` — because a local grant that outlives an LDAP revocation must be visible, or it is a backdoor. |

Membership responses expose provenance: `role`, `oidc_role`, `oidc_group`, `local_role`, `granted_by`, `granted_at`.

**Break-glass.** With `login_groups` empty and no `site_admin_groups`, a fresh deployment has no site admin and every path to making one requires already being one. `bakery user site-admin <email>` writes `site_role_local` straight to the database, needs `DB_URL`, and has **no HTTP or API path at all** — mirroring `DEV_LOGIN_ENABLED`. Reaching it requires infrastructure access, not a session, and anyone with that could `UPDATE` the column by hand anyway. `--allow-local-site-admins` does not gate it (gating it would make a fresh deployment unbootstrappable — the exact deadlock it exists for).

**Also fixed:** `RevokeAPIKeysForMembership` had **never worked** — pgx cannot build an encode plan for a slice of a custom enum unless the type is registered on the connection, and it had no caller, so nothing proved it. `db.NewPool`'s `AfterConnect` now `LoadType`s and registers every custom enum **and its array type**, failing the connection if one is absent. This was a whole class of bug: any future sqlc query taking an enum array failed identically, at encode time, on its first caller.

**What M1.5 did NOT build:** group→project mapping (the group explosion this exists to escape), SCIM, a third identity source (the column-per-source shape does not scale past two — a third is a migration to a grants table, and that is a fine trade for YAGNI today), and the SPA wiring for any of it. That is the next wave.

**Frontend track (parallel):** design system + screens in Claude Design during M0–M1; implement in SvelteKit one milestone behind the backend so screens are built against real endpoints. Screens: login · org/project lists · project detail (overview / backends / members / keys / settings) · backend config + hit-rate charts · API keys (show-once) · admin. **The highest-value screen is a config snippet generator** — per backend, emit the exact `local.conf` / `workspace.yml` / `ccache.conf` / `hosts.toml` with a freshly-minted key baked in. Every one of these clients has a vicious config gotcha; this is what drives adoption.

### M2 as landed

M2 builds the **first two backends that serve traffic**, and it is the first milestone a user can point bitbake at. Both are one shared HTTP handler over a per-backend Policy; nothing about sstate or downloads is special-cased in the handler.

**Packages.** `internal/cache/httpblob` — the shared handler (`Backend`, `Policy`, `NewSstate`/`NewDownloads`), a DB-backed **in-process route resolver** (`CachedResolver`: caches *positive* resolutions only; a hit is zero queries, which the boot advisory lock makes sound), and the `Authenticator`/`Principal`/`RouteResolver` seams. `internal/auth` gains `AuthenticateCache` — the HTTP **Basic**-scheme bridge bitbake speaks. `internal/cli` gains `bakery sstate push` / `bakery downloads push`. `internal/api` gains the **config-snippet generator**. `server.Boot` constructs the two backends with a `cache.Deps` (`Blobs`, `Metrics`, `Logger` — **no `*repository.Queries`**, per the seam's rule) and mounts them on the public mux, **in headless mode too** — "no console" does not mean "no cache".

**Addressing.** `/cache/{org}/{project}/sstate/{path...}` (the key has slashes — `[universal/]hh/hh/name` — and colons arrive percent-encoded; route on the decoded path) and `/cache/{org}/{project}/downloads/{basename}` (flat). A project with **no configured row for that kind 404s cleanly** — it never mounts a mount point it cannot serve, and never 500s. Both mount `GET|HEAD|PUT` on a **literal** 4th segment, which is what lets them coexist with each other and with the methodless `/api/v1/` and SPA `/` without the ServeMux "neither is more specific" panic.

**The read path is a dumb static file server, and the protocol traps are load-bearing:** HEAD is the hot path (served from `blob.Service.Exists`/`.Stat`, never a GET whose body is discarded); a **miss is 404, never 403** (bitbake retries a 403 as a full-body GET, turning the HEAD storm into a GET storm) and **never a 200 with an empty body** (bitbake's post-check requires a non-zero file); GET honors **Range → 206** (wget `--continue`) and **416** on an unsatisfiable range, via `http.ServeContent`. The `.siginfo`/`.sig` sidecars are just more objects at their own keys.

**Auth.** Reads are gated by the backend row's `ReadAuthRequired`; when required the credential is an **API key presented as HTTP Basic**, constant-time, never logged. **Writes ALWAYS require a WRITE-scoped key** regardless of `ReadAuthRequired` — an unauthenticated write is a cache-poisoning vector — and answer `401` (bad/absent credential) or `403` (authenticated, not write-authorized), never a silent accept. A Bakery key is **one opaque `bkry_` token, not an `id:secret` pair**; `AuthenticateCache` reads it from the Basic **password field and falls back to the username**, so a client that puts the token in either field authenticates.

**The write path is our invention — bitbake has none.** Authenticated `PUT` to the same address routes bytes through `blob.Service.Put` (dedup/refcount/ordering for free); Overwrite is **no** per policy, so a PUT of an existing immutable key is a `200` idempotent no-op, not a 409 and not a content swap. `bakery {sstate,downloads} push <org> <project> <dir>` walks the on-disk cache, HEADs each object, and PUTs only the misses (a warm cache is a cheap no-op); it reads its key from `--key`/`BAKERY_API_KEY`. It walks the on-disk cache **only** — it does not talk to hashserv.

**The config-snippet generator** (`POST /api/v1/orgs/{org}/projects/{project}/snippet`, project-read floor, write-scope capped in `auth.CreateAPIKey`) is the first slice of DESIGN's highest-value screen. It **mints a fresh key** and returns the exact **verified** Yocto `local.conf` (`SSTATE_MIRRORS` with the mandatory `downloadfilename=PATH` suffix + `own-mirrors`/`SOURCE_MIRROR_URL`) with this server's host baked in, the `~/.netrc` line carrying the token, and the `bakery … push` commands. The response shape is `{tool, host, base_url, local_conf, netrc, push_commands[], api_key{…,token}}` — recorded here for the SPA wiring wave, which replaces the screen's mock data with a call to this endpoint. The default key name carries entropy so **regenerating a snippet never collides** on the per-`(project,user)` name index.

**Metrics.** The headline `bakery_cache_requests_total{org,project,backend,kind,op,result}` is emitted by `blob.Service`, so every backend is normalized for free and a backend **must not re-emit it**. It is labeled from the resolved `Route.Ref`, never `r.URL.Path`: a HEAD storm over thousands of distinct sstate keys collapses onto **one series per `(kind,op,result)`** (proven end-to-end: 60 HEADs across 22 distinct keys stayed at 32 total series), not one per object. Served on `--metrics-addr` only; the public listener 404s `/metrics`.

**What M2 did NOT build:** hashserv (M3), bazel (M4), OCI (M5), the GC loop (M6), S3, a `bakery.bbclass` (the CLI is the v1 write path), and the SPA wiring for the snippet screen (the endpoint exists; the screen still renders mock data). `PUT` overwrite, sig verification on `/cas`, and the other Policy flags exist as fields for later backends but are `Overwrite=false`/`Verify=NoVerify` for both M2 mounts.

### M3 as landed

([spec](specs/2026-07-13-m3-hashserv.md)) `internal/cache/hashserv/` — the one backend that does **not**
route through `blob.Service` (it stores hash rows, not objects), so it carries its own narrow `Queries`
surface and owns its own metrics. It implements `cache.StreamBackend`, the seam M1 wrote for it.

**Mount:** `/cache/{org}/{project}/hashserv`, a WebSocket upgrade on the shared mux. Registered in
headless mode too — "no console" is not "no cache".

**Scope is deliberately smaller than upstream's, and the reason is a grep.** Every hashserv client method
was checked against bitbake's `lib/bb` (the build path). A real build calls only `ping`, `auth`, `get`,
`get-stream`, `exists-stream`, `report`, `report-equiv`. The GC RPCs (`gc-mark`, `gc-sweep`, `gc-status`,
`clean-unused`, `get-db-usage`, …) and the user-admin RPCs (`new-user`, `set-user-perms`, `become-user`,
`refresh-token`, …) have **zero call sites** there — they exist for the `bitbake-hashclient` operator CLI
and for upstream's own tests.

So they are **not implemented**, and that is a position, not an omission:

- **No GC RPCs.** Bakery garbage-collects **in-process** in M6, under the two-half write barrier.
  Exposing upstream's `gc_mark` mark-and-sweep would put a *second, competing* GC mechanism on the same
  tables, reachable by any cache client, for no production benefit. There is therefore **no `gc_mark`
  column and no `config` table**.
- **No user-admin RPCs.** Credentials are minted by the Bakery API. A cache client never mints one.
- `remove` **is** served (project-scoped, write-key-gated): an operator legitimately wants "purge this
  project's hash equivalence", and it is what the conformance harness resets with.

**Transport: WebSocket only.** Raw TCP/unix are deferred. This was free risk reduction — the chunked
`chunk-stream` framing that this document called the highest-risk file **exists only on the raw-TCP
path**. Over WebSocket a message is one JSON document, whole. Not implementing TCP means never writing it.

**Permissions** (`perms.go`). An API-key principal is structurally hollow — `CanAdminProject` is
hard-false for `MethodAPIKey` — so only two questions can ever be true of a cache credential:

| hashserv perm | Source | Grants |
|---|---|---|
| `@read` | `CanReadProject`, **or anonymous when `read_auth_required = false`** | `get`, `get-stream`, `exists-stream`, `get-outhash`, `ping` |
| `@report` | `CanWriteProject` — a write-scoped key, **always** | the write path of `report` / `report-equiv` |
| `@db-admin` | `CanWriteProject` | `remove`, and nothing else |
| `@user-admin` | **unreachable** | nothing; the RPCs do not exist |

There is **no anon-perms knob**, deliberately: upstream's `DEFAULT_ANON_PERMS` hands a stranger
`@read + @report + @db-admin`, and an unauthenticated write must not be a representable state. An
anonymous `report` on an open mirror takes upstream's `report_readonly` path — answered, never written —
and is **metered** (`bakery_hashserv_reports_dropped_total`), because a silent non-write that nobody can
see is how a cache quietly stops earning its keep.

**Schema** (`000010_hashserv`): `hashserv_unihashes` + `hashserv_outhashes`, both scoped by `backend_id`
— the multi-tenancy boundary, and a parameter of every query rather than a filter someone can forget.
`(backend_id, method, taskhash) → unihash` is **write-once**. Both tables carry **both halves of the M6
GC write barrier** (`created_at` *and* `live_xid`) from birth. `outhash_siginfo` is a `text` column, as
upstream does.

**Upstream chaining** is in, and disableable. A backend may name an upstream in its `config`
(`{"upstream": "wss://hashserv.yoctoproject.org/ws"}`); absent means off, which is the default. On a
`get-stream` miss Bakery asks upstream, answers the build **immediately**, and backfills the row behind
it — the write never touches the hot path. `--hashserv-disable-upstream` / `HASHSERV_DISABLE_UPSTREAM` is
the kill switch for the day the public server is slow and it is showing up in customer builds. Unlike the
reference implementation, which opens **one upstream connection per downstream client**, Bakery pools
them.

**The CI gate** (`just hashserv-conformance`, wired into `image.needs`) has **two halves, and upstream
only gives you one:**

- **Half 1 — upstream bitbake's own suite**, external mode, driving the real server over the real
  multi-tenant WebSocket mount. It runs **authenticated** with a real `bkry_` write key, because
  `setUp`/`tearDown` call `remove()`. `ran=17 failures=0 errors=0 skipped=3`, and **all four counts are
  pinned** — `get_env()` skips on an empty string, so a harness that handed through an empty
  `BB_TEST_HASHSERV` would skip everything and report a triumphant green. The 22 excluded tests each
  carry a written reason, and `TestUpstreamSuiteCoversEveryTest` asserts the partition against unittest's
  own introspection, so a bitbake bump that adds a test **fails the gate instead of slipping past it**.
- **Half 2 — the real `bitbake-hashclient`**, which upstream's external suite **never runs**: all 27 of
  its `run_hashclient` call sites live in a class that spawns its own local `unix://` server. So this
  half is Bakery-authored. It also carries the one-writer-per-connection proof (1000 concurrent lookups
  over 20 connections, cold then warm, none crossed) — because upstream's `test_stress` is **vacuous in
  2.8.0**: it calls `Client(...)`, a name `tests.py` never imports, so all 100 threads die with a
  `NameError` that `threading` swallows and `assertFalse(failures)` passes.

**What M3 did NOT build:** raw TCP/unix transports, the GC and user-admin RPCs (above), `get-stats` /
`reset-stats`, and the SPA screen for hashserv config (the backend CRUD API already covers it).

### M4 as landed

`internal/cache/bazel/` — the REAPI backend: four gRPC services (Capabilities, ActionCache,
ContentAddressableStorage, ByteStream) on a **dedicated `--grpc-addr` listener**, plus the HTTP `/ac`
and `/cas` mounts it owns and delegates to `httpblob`. It implements `cache.GRPCBackend`
(`RegisterGRPC(*grpc.Server)` — the concrete type, because ByteStream's legacy genproto codegen demands
it). One `bazel` `cache_backends` row serves all of it; `sccache` is a fifth `httpblob` mount in its own
namespace. Ships **moon (gRPC default), Bazel (gRPC + HTTP), ccache (`@layout=bazel`, plaintext + bearer
token), and sccache (WebDAV)**.

**The design was settled by two research waves (13 source-reading agents) and six empirical Go spikes
against the repo's pinned deps** — the spikes overturned the pre-implementation plan on three points, and
the correction table lives in the durable rulings (scratch `SPIKE-final.md` / the workflow journals).
Every load-bearing decision is now a CLAUDE.md invariant. Highlights:

- **Listener topology reversed** (see above): one-port h2c → dedicated listener. `GracefulStop` panics on
  the `ServeHTTP` path; a shared h2c port silently breaks M3 hashserv. Both proven with real programs.
- **The `/ac` namespace is SPLIT** into `ac` (opaque HTTP) and `ac-grpc` (typed gRPC), because moon uses
  the same action digest over both transports with different encodings — sharing is a hard client error.
- **`/cas` needs `cache_objects` rows** (a pure `blobs` lookup is a cross-tenant existence oracle and a
  GC-eats-it-immediately bug). `blob.ExistsBatch` (one `key = ANY($n::text[])` query) answers the
  `FindMissingBlobs` storm; the CAS PUT skip-ingest kills the redundant-PUT storm (the inverse of the
  sstate HEAD storm) while keeping the body drain except on `Expect: 100-continue`.
- **GetCapabilities is a gate**: `update_enabled=true`, `max_batch_total_size_bytes=4 MiB` (never 0),
  IDENTITY-only compressors, api 2.0–2.3 — three silent-catastrophe fields, each with a named regression
  test. No zstd, no `GetTree`/`Execute`, no AC completeness check (a SHOULD, and forbidden by `/ac`
  opacity — it becomes the M6 GC ordering constraint above), no inlining.

**The CI gate** (`just bazel-conformance`, wired into `image.needs`) has three clients: the
`remote-apis-sdks` Go client (all 8 RPCs over a real gRPC listener — runs everywhere), and real `ccache`
and `sccache` (which assert a **write** round-trips, because the sccache failure mode is a silently
read-only backend). It also fixed a **shipped M2/M3 bug found during the research**: the SPA catch-all
answered unrouted `/cache/…` and `/v2/…` GETs with `200 + index.html` (a poisoned cache hit); now they
404 (its own commit, ahead of the feature).

**What M4 did NOT build:** zstd / `compressed-blobs` (Unimplemented; IDENTITY-only — both clients default
compression off), a `--grpc-addr`-less shared-port mode, the AC completeness check, inlined
stdout/stderr, `cache_objects.accessed_at` (an M6 call), moon in the CI gate (a manual `moon` run is in
the DoD; `just moon-conformance` is a followup), and the SPA snippet wiring (the endpoint grows to
moon/ccache/sccache/bazel; the screen still renders mock data — and the snippet's gRPC endpoint still
assumes a single-ingress host, which the SPA-wiring wave must resolve).

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

- `bakery.bbclass` (push sstate on task completion) — deferred; the CLI is the v1 path.
- Binary package feeds (ipk/deb/rpm) — explicitly out of scope; revisit as its own project.
