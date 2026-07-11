# Bazel REAPI · ccache · Docker — implementation brief

Verified against primary sources (protos, server source, client source) in July 2026. Where the docs and the source disagree, the source wins and is flagged ⚠️.

---

## 1. Bazel Remote Cache API (REAPI v2), as consumed by moon

### 1.1 What moon actually speaks

**Both, but gRPC is the default.** HTTP was added in moon v1.32 as a fallback.

- moon docs list required features: action cache, CAS, **SHA256 digests**, gRPC.
- Config key is `remote:` (top-level in `.moon/workspace.yml`). `unstable_remote:` was the pre-stabilization name.

```yaml
remote:
  api: 'grpc' | 'http'          # default grpc
  host: 'grpc://host:9092'      # grpc:// grpcs:// http:// https://
  auth:
    token: 'ENV_VAR_NAME'       # NAME of an env var, not the token itself
    headers:
      'X-Custom': '...'
  cache:
    compression: 'none' | 'zstd'   # default none
    instanceName: 'moon-outputs'   # default
  tls: { cert, domain, assumeHttp2 }
  mtls: { caCert, clientCert, clientKey, domain, assumeHttp2 }   # takes precedence over tls
```

### 1.2 The minimal server surface moon actually calls

| Service | RPCs | Notes |
|---|---|---|
| `Capabilities` | `GetCapabilities` | **Called on connect. Hard requirement.** |
| `ActionCache` | `GetActionResult`, `UpdateActionResult` | `digest_function: SHA256`, `inline_stdout/stderr = true` |
| `ContentAddressableStorage` | `FindMissingBlobs`, `BatchReadBlobs`, `BatchUpdateBlobs` | **`GetTree` is never called** |
| `google.bytestream.ByteStream` | `Read`, `Write` | **`QueryWriteStatus` is never called** |

Behavioral details that are load-bearing:

- moon rewrites `grpc://` → `http://`, `grpcs://` → `https://`.
- moon **disables the backend entirely** if `cache_capabilities` is absent, or if `digest_functions` doesn't contain `SHA256`.
- moon partitions batches against our advertised `max_batch_total_size_bytes`.
- `GetActionResult` → `NOT_FOUND` = miss. `OUT_OF_RANGE` also treated as a miss.
- `BatchReadBlobs` per-blob `status`: `OK`/`NOT_FOUND` tolerated; anything else logs a warning and drops the blob.
- **ByteStream upload chunk size = 1 MiB**; final chunk carries `finish_write: true`. moon validates `committed_size == digest.size` **or `-1`** for uncompressed writes.
- zstd is negotiated separately per path: `supported_compressors` gates ByteStream `compressed-blobs`; `supported_batch_update_compressors` gates `BatchUpdateBlobs`. moon compresses at **zstd level 1**.

### 1.3 GetCapabilities — what a cache-only server must return

```go
&repb.ServerCapabilities{
  CacheCapabilities: &repb.CacheCapabilities{
    DigestFunctions:               []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
    ActionCacheUpdateCapabilities: &repb.ActionCacheUpdateCapabilities{UpdateEnabled: true}, // REQUIRED
    MaxBatchTotalSizeBytes:        4 << 20,  // 4 MiB — the de facto value (gRPC's default msg limit)
    SymlinkAbsolutePathStrategy:   repb.SymlinkAbsolutePathStrategy_ALLOWED,
    SupportedCompressors:            []repb.Compressor_Value{repb.Compressor_IDENTITY, repb.Compressor_ZSTD},
    SupportedBatchUpdateCompressors: []repb.Compressor_Value{repb.Compressor_IDENTITY, repb.Compressor_ZSTD},
  },
  ExecutionCapabilities: &repb.ExecutionCapabilities{
    DigestFunction: repb.DigestFunction_SHA256,
    ExecEnabled:    false,   // WE ARE A CACHE. Say so, or a client will try to Execute.
  },
  LowApiVersion:  &semver.SemVer{Major: 2, Minor: 0},
  HighApiVersion: &semver.SemVer{Major: 2, Minor: 3},
}
```

If `action_cache_update_capabilities.update_enabled` is not `true`, clients never call `UpdateActionResult` and the cache stays empty forever.

### 1.4 ByteStream resource_name — the parsing trap

```
upload uncompressed: {instance}/uploads/{uuid}/blobs/{hash}/{size}{/optional_metadata}
upload compressed:   {instance}/uploads/{uuid}/compressed-blobs/{compressor}/{hash}/{size}{/…}
read uncompressed:   {instance}/blobs/{hash}/{size}
read compressed:     {instance}/compressed-blobs/{compressor}/{hash}/{size}
```

- **`instance_name` MAY CONTAIN SLASHES** (it's `**` in the proto's HTTP annotation). Ours is `{org}/{project}`.
  ⇒ **Scan segments left-to-right for the first of `uploads` / `blobs` / `compressed-blobs`. Everything before it is the instance name. NEVER split positionally on `/`.**
- The `{digest_function}` segment MUST be omitted for SHA256 (inferred from hash length). moon always omits it. Parse defensively.
- Reserved path keywords: `blobs`, `uploads`, `actions`, `actionResults`, `operations`, `capabilities`, `compressed-blobs`. Forbid these as org/project slugs at creation time.
- Compressed writes: the digest in the name is the **uncompressed** digest; verify after decompression, return `INVALID_ARGUMENT` on mismatch.
- Compressed reads: return `INVALID_ARGUMENT` if `read_limit != 0`.
- Duplicate-upload race: terminate without error, `committed_size = -1`.
- **"Servers MUST behave as though empty blobs are always available."** Special-case `e3b0c442…b855` / size 0 — never store it, always report present, `BatchReadBlobs` returns empty bytes.

### 1.5 Auth

REAPI has no auth in the spec — it's transport-level. moon supports all of:
- `auth.token` names an env var whose value becomes `authorization: Bearer <token>` gRPC metadata.
- `auth.headers` — arbitrary header map, injected into every gRPC **and** HTTP request.
- `tls` / `mtls` for transport certs.

**Headers are applied before `GetCapabilities`**, so auth is present on every call including the first.

⚠️ If the env var named by `auth.token` is empty, moon silently disables the remote cache. Users will report "the cache doesn't work" with no error.

### 1.6 gRPC cannot carry a URL path prefix

tonic's `AddOrigin` discards the endpoint's path (`http::uri::Parts` → only `scheme` + `authority` are kept). gRPC's `:path` is always `/build.bazel.remote.execution.v2.ActionCache/GetActionResult` etc.

⇒ **The project selector for gRPC MUST be `instance_name`.** moon does not validate or sanitize `instanceName` — it's `pub instance_name: String` with no `validate =` — and passes it verbatim into every request and every ByteStream resource name. `instanceName: "acme/proj"` works.

### 1.7 Go libraries

| Library | Use |
|---|---|
| `github.com/bazelbuild/remote-apis` | **Pregenerated `.pb.go` — no protoc needed.** Gives `RegisterActionCacheServer`, `RegisterContentAddressableStorageServer`, `RegisterCapabilitiesServer` + all messages. |
| `google.golang.org/genproto/googleapis/bytestream` | `ByteStreamServer` interface + messages. |
| `github.com/bazelbuild/remote-apis-sdks` | **Client-side** SDK. Ships `go/pkg/fakes/server.go` — useful as a reference impl and as a test client. |

We vendor nothing and generate nothing. `go get github.com/bazelbuild/remote-apis` is sufficient.

Reference cache-only servers to crib from: `buchgr/bazel-remote`, `buildbarn/bb-storage`.

---

## 2. The HTTP blob API — one endpoint, four clients

### 2.1 The bazel-remote HTTP protocol

```
GET|PUT|HEAD  /cas/<sha256-hex>     or  /<instance>/cas/<sha256-hex>
GET|PUT|HEAD  /ac/<64-hex>          or  /<instance>/ac/<64-hex>
GET           /status                    (moon probes this on connect; 404 is tolerated)
```

- zstd: `Accept-Encoding: zstd` on GET. PUT with `Content-Encoding: zstd` **must** also send `X-Digest-SizeBytes: <uncompressed size>`.
- URL paths go through `path.Clean` — normalize/reject `//`, `./`, `../`.

### 2.2 ⚠️ moon's HTTP mode is NOT bazel-remote-wire-compatible for the AC

moon builds `{host}/{instance_name}/{path}/{hash}` and stores **its own `Manifest` struct as JSON** at `/ac/`, not a REAPI `ActionResult`:

```json
{"files":[{"digest","is_executable","modified_at","path","unix_mode"}],
 "symlinks":[…],"exit_code","stdout_digest","stderr_digest",
 "upload_started_at","upload_completed_at"}
```

Also: moon's HTTP mode has **no `HEAD`**, so `find_missing_blobs()` returns *every* digest as missing — it re-uploads all blobs every build. This is why gRPC is not optional.

### 2.3 ccache

Backends: `file`, `http`, `redis`, `redis+unix`, `crsh`. We implement `http`.

- **Verbs:** `GET`, `PUT`, `DELETE`, `HEAD` (a `HEAD` precedes the `PUT` when `Overwrite::no`).
- **Status codes:** ccache checks `status < 200 || status >= 300` → failure. A non-2xx `GET` is a **miss**, not an error.
- **Headers:** `User-Agent: ccache/<version>`, `Content-Type: application/octet-stream` on PUT, `Authorization: Basic` (URL userinfo) or `Bearer` (`bearer-token` attribute), plus arbitrary `header=Key=Value` attributes.

**The `layout` attribute (server-visible):** ccache keys are **160-bit → 40 hex chars**.

| layout | path | example |
|---|---|---|
| `subdirs` (default) | `<url-path>/<K[0:2]>/<K[2:]>` | `/cache/ab/cdef…` |
| `flat` | `<url-path><K>` | `/cache/abcdef…` |
| `bazel` | `<url-path>ac/<K padded to 64 hex>` | `/ac/<40hex><first 24 of the same hex>` |

The bazel padding is literally `FMT("ac/{}{:.{}}", hex_key, hex_key, 64 - 40)` — append the first 24 chars of the key to itself so it *looks* like a SHA-256.

⚠️ **`layout=bazel` puts BOTH ccache entry types (manifest and result) under `/ac/` — nothing ever goes to `/cas/`.** The value is ccache's own binary blob, not an `ActionResult`.

Config:
```ini
remote_storage = http://bakery/cache/acme/proj @layout=bazel
remote_storage = http://user:token@bakery/cache/acme/proj @layout=bazel
# legacy pipe syntax still accepted:
CCACHE_REMOTE_STORAGE='http://bakery/cache/acme/proj|layout=bazel|read-only=true'
```

The URL path prefix is preserved verbatim (ccache appends `/` if absent — no trailing-slash requirement).

### 2.4 sccache — free

`SCCACHE_WEBDAV_ENDPOINT` + `SCCACHE_WEBDAV_TOKEN`/`_USERNAME`/`_PASSWORD`. "WebDAV" in name only — it needs `PUT`, not `PROPFIND`. sccache's own docs list ccache's HTTP backend and Bazel Remote Caching as compatible backends.

### 2.5 ⇒ The design consequence

**Treat `/ac/` as an opaque byte store — round-trip the body verbatim, echo the content type, never parse it as an `ActionResult`.** That single decision makes one implementation simultaneously serve:

- ccache (`@layout=bazel`)
- sccache (WebDAV mode)
- moon (`api: http`)
- Bazel itself (`--remote_cache=http://…`)

`/cas/` is content-addressed and MUST verify `key == sha256(body)`. `/ac/` is opaque and MUST NOT. Getting this backwards breaks three clients at once.

Redis backend: **skip it.** Separate protocol surface (RESP), zero incremental value — anyone who wants Redis can point ccache at a real Redis.

---

## 3. Docker / OCI pull-through proxy

### 3.1 Why not `registry:2`

- *"It's currently possible to mirror only one upstream registry at a time."* One `remoteurl` per instance.
- Mirror URLs must be **domain roots** — no path, fragment, or query.
- Docker daemon's `registry-mirrors` **only supports mirrors of Docker Hub**.
- Any private image the configured upstream user can access becomes available via the mirror. Auth in front is mandatory.

### 3.2 Endpoints a pull-through cache must serve

```
GET    /v2/                              -> 200 + Docker-Distribution-API-Version: registry/2.0
                                            (or 401 + WWW-Authenticate)
HEAD   /v2/<name>/manifests/<ref>        -> 200, Content-Length, Docker-Content-Digest
GET    /v2/<name>/manifests/<ref>        -> 200, body, Docker-Content-Digest, Content-Type
HEAD   /v2/<name>/blobs/<digest>         -> 200, Content-Length, Docker-Content-Digest
GET    /v2/<name>/blobs/<digest>         -> 200 (Range -> 206 + Content-Range)
```

Everything else can 404/405. `Docker-Content-Digest` on manifest responses is load-bearing — clients use it to pin.

### 3.3 The token flow (both directions)

1. Client hits `/v2/…` unauthenticated → **401** + `WWW-Authenticate: Bearer realm="…",service="…",scope="repository:<name>:pull"`
2. Client `GET <realm>?service=…&scope=…` (optionally with `Authorization: Basic <user:pat>`)
3. → `{"token":"…","expires_in":300,"issued_at":"…"}`
4. Client retries with `Authorization: Bearer <token>`

We perform this dance **upstream** (per-repository, caching tokens until `expires_in`), and we must **also issue our own downstream 401 challenge** — otherwise we are an open relay for whatever upstream credentials we hold.

Upstream realms: Docker Hub `https://auth.docker.io/token` (service `registry.docker.io`), ghcr `https://ghcr.io/token`, quay `https://quay.io/v2/auth`.

`google/go-containerregistry` (`pkg/v1/remote` + `pkg/authn`) implements the full client side of this. Use it for upstream; write our own storage + downstream handler.

### 3.4 Multi-registry routing — the `?ns=` convention

containerd appends `?ns=<namespace>` whenever the mirror host differs from the image's namespace:

```go
const namespaceQueryArg = "ns"
func (r *request) addNamespace(ns string) error {
    if !r.host.isProxy(ns) { return nil }
    return r.addQuery(namespaceQueryArg, ns)
}
```
`isProxy(refhost)` is true iff `refhost != h.Host` (with a `docker.io`↔`registry-1.docker.io` special case). **Not gated on `capabilities`. Not gated on `override_path`.** Applied to manifests, blobs, and pushes.

⇒ One Bakery endpoint mirrors *any* number of upstreams. This is what collapses the 5×`registry:2` setup into one service.

**⚠️ `?ns=` is a containerd convention, not in the OCI spec.** Docker Engine and podman never send it. ⇒ the backend config **must** carry a `default_upstream` used when `?ns=` is absent.

### 3.5 Path prefixes — three clients, two shapes

| Client | Config | Resulting request |
|---|---|---|
| **containerd** | `server = "https://bakery/cache/acme/proj/docker"` | `GET /cache/acme/proj/docker/v2/library/alpine/manifests/latest?ns=docker.io` |
| **BuildKit** | `mirrors = ["bakery/cache/acme/proj/docker"]` | `GET /v2/cache/acme/proj/docker/library/alpine/manifests/latest?ns=docker.io` |
| **podman/CRI-O** | `location = "bakery/cache/acme/proj"` | `GET /v2/cache/acme/proj/library/alpine/manifests/latest` — **no `ns=`** |

containerd **appends `/v2` itself** (and won't double-append if you write it). BuildKit puts the prefix **after** `/v2` — the opposite. So we serve both shapes.

`override_path = true` on containerd means "use the path as-is, don't append `/v2`" — which can be used to **normalize containerd onto BuildKit's shape** if we'd rather have one route.

### 3.6 containerd config — the migration note

The deprecated `plugins."io.containerd.grpc.v1.cri".registry.mirrors` style (one endpoint per registry, no `ns=`) must be replaced with `config_path` + `hosts.toml`. containerd 2.0 requires this anyway.

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"
[host."https://bakery.corp/cache/acme/proj/docker"]
  capabilities = ["pull", "resolve"]
```
One file per upstream namespace, **all pointing at the same Bakery endpoint**. No image-ref rewriting.

### 3.7 ⚠️ Manifest integrity

**Store and serve manifest bytes verbatim.** A single `json.Marshal` round-trip reorders keys, changes whitespace, and breaks `Docker-Content-Digest`. It will **only** reproduce on multi-arch index manifests — i.e. not in your unit test, and yes in production.

Add a boot-time self-test that round-trips a known multi-arch manifest through the full storage path and asserts the digest.

### 3.8 Manifest TTL and stale-while-revalidate

- Digest-pinned manifests: **immutable**, never expire, never revalidate.
- Tags: `expires_at = now() + manifest_ttl` (default ~10 min).
- **On an expired tag, serve the cached manifest immediately and refresh in the background.** Zero added latency, Docker Hub rate limits stop mattering, and an upstream outage becomes a non-event. Also serve stale (not 5xx) when the upstream fetch fails.

A build cache that fails closed when Docker Hub is down is a build cache that isn't doing its job.

### 3.9 Docker Hub rate limits (verified July 2026)

| Tier | Limit | Window |
|---|---|---|
| Unauthenticated | **100 pulls per IPv4 / IPv6 /64** | per 6 hours |
| Personal (authenticated, free) | 200 pulls | per 6 hours |
| Pro / Team / Business | Unlimited | — |

The widely-cited "10/hour unauthenticated" figures from the 2025 announcements are **superseded**.

A "pull" = a manifest version check **plus** any resulting download. Multi-arch images count **once per architecture**.

⇒ An authenticated pull-through cache moves the whole org from the shared-IP unauthenticated bucket (catastrophic behind NAT'd CI) into an authenticated one, and collapses N runners into one upstream pull per digest.

---

## Sources

- moon: [remote-cache guide](https://moonrepo.dev/docs/guides/remote-cache) · [`remote_config.rs`](https://github.com/moonrepo/moon/blob/master/crates/config/src/workspace/remote_config.rs) · [`grpc_remote_storage.rs`](https://github.com/moonrepo/moon/blob/master/crates/cache-remote/src/grpc_remote_storage.rs) · [`http_remote_storage.rs`](https://github.com/moonrepo/moon/blob/master/crates/cache-remote/src/http_remote_storage.rs)
- REAPI: [`remote_execution.proto`](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) · [`bytestream.proto`](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto) · [buchgr/bazel-remote](https://github.com/buchgr/bazel-remote)
- ccache: [manual](https://ccache.dev/manual/latest.html) · [HTTP storage wiki](https://github.com/ccache/ccache/wiki/HTTP-storage) · [`httpstorage.cpp`](https://github.com/ccache/ccache/blob/master/src/ccache/storage/remote/httpstorage.cpp)
- sccache: [Webdav.md](https://github.com/mozilla/sccache/blob/main/docs/Webdav.md)
- Docker: [Registry API V2](https://distribution.github.io/distribution/spec/api/) · [token auth](https://distribution.github.io/distribution/spec/auth/token/) · [containerd hosts.md](https://github.com/containerd/containerd/blob/main/docs/hosts.md) · [Hub pull limits](https://docs.docker.com/docker-hub/usage/pulls/)
