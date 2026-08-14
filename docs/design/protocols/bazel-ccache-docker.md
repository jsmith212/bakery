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
| `ActionCache` | `GetActionResult`, `UpdateActionResult` | `digest_function: SHA256`, `inline_stdout/stderr = true` (⚠️ but see below) |
| `ContentAddressableStorage` | `FindMissingBlobs`, `BatchReadBlobs`, `BatchUpdateBlobs` | **`GetTree` is never called** |
| `google.bytestream.ByteStream` | `Read`, `Write` | `QueryWriteStatus` (⚠️ Bazel DOES call it — see below) |

**⚠️ CORRECTION — `QueryWriteStatus` "is never called" is true of moon, NOT of Bazel.** Bazel calls it on a **retried upload**. The conclusion (respond `Unimplemented`) survives — but for a **different reason** than the doc implies: `Unimplemented` is what Bazel *supports* as "no resumable upload", whereas the spec's own answer, `NOT_FOUND`, turns a resumable upload into a **failed** one. So do not "fix" it to `NOT_FOUND`. There is no resumable-upload primitive in `storage` and M4 must not invent one.

**⚠️ CORRECTION — inline `stdout`/`stderr`/`OutputFile.contents` are HINTS; M4 does zero inlining and is conformant.** The server MAY ignore them and the client falls back to a CAS download (`RemoteExecutionService.java:1318-1341`). If a client *uploads* an `ActionResult` that already carries them, **store it verbatim** — never rewrite an ActionResult we did not author. (moon sets `inline_stdout/stderr = true` on every lookup and treats an oversized `Code::OutOfRange` response as a **silent miss**, so if you ever DO inline, cap it — unbounded inlining yields a cache that looks healthy and never hits.)

Behavioral details that are load-bearing:

- moon rewrites `grpc://` → `http://`, `grpcs://` → `https://`.
- moon **disables the backend entirely** if `cache_capabilities` is absent, or if `digest_functions` doesn't contain `SHA256`.
- moon partitions batches against our advertised `max_batch_total_size_bytes`.
- `GetActionResult` → `NOT_FOUND` = miss. `OUT_OF_RANGE` also treated as a miss.
- `BatchReadBlobs` per-blob `status`: `OK`/`NOT_FOUND` tolerated; anything else logs a warning and drops the blob.
- **ByteStream upload chunk size = 1 MiB**; final chunk carries `finish_write: true`. moon validates `committed_size == digest.size` **or `-1`** for uncompressed writes. **⚠️ CORRECTION — `committed_size = -1` is COMPRESSED-ONLY for Bazel.** On an uncompressed write that ran to `finish_write`, Bazel accepts **only** `committed_size == the bytes it streamed` and otherwise throws `IOException("write incomplete")` (`ByteStreamUploader.java:262-295`). moon happens to also accept `-1`, so a moon-only test passes and Bazel breaks. On a normal uncompressed write return the exact size; `-1` is reserved for the duplicate-upload/compressed race, and even then only moon tolerates it.
- zstd is negotiated separately per path: `supported_compressors` gates ByteStream `compressed-blobs`; `supported_batch_update_compressors` gates `BatchUpdateBlobs`. moon compresses at **zstd level 1**.

### 1.3 GetCapabilities — what a cache-only server must return

```go
&repb.ServerCapabilities{
  CacheCapabilities: &repb.CacheCapabilities{
    DigestFunctions:               []repb.DigestFunction_Value{repb.DigestFunction_SHA256},
    ActionCacheUpdateCapabilities: &repb.ActionCacheUpdateCapabilities{UpdateEnabled: true}, // REQUIRED
    MaxBatchTotalSizeBytes:        4 << 20,  // 4 MiB — MUST be non-zero; NOT "just the default" (see ⚠️)
    SymlinkAbsolutePathStrategy:   repb.SymlinkAbsolutePathStrategy_ALLOWED,
    SupportedCompressors:            []repb.Compressor_Value{repb.Compressor_IDENTITY}, // IDENTITY only (see ⚠️)
    SupportedBatchUpdateCompressors: []repb.Compressor_Value{repb.Compressor_IDENTITY}, // IDENTITY only
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

**⚠️ CORRECTION — `MaxBatchTotalSizeBytes` is a CORRECTNESS switch, not "the de facto default".** It must be **non-zero and ≤ 64 MiB**. moon partitions every batch against the value **we** advertise; at `0` (which proto3 cannot distinguish from unset), moon's `create_batches` takes `chunk_into_batches`, which **never sets `stream: true`** and ignores blob size — a modest 504-blob task then goes out as a single ~48 MiB `BatchUpdateBlobs`, exceeding grpc-go's 4 MiB default and returning `RESOURCE_EXHAUSTED`, which moon **misreports to the user as "out of storage space"**. Advertise **4 MiB** and set grpc-go `MaxRecvMsgSize`/`MaxSendMsgSize` to 16 MiB (4× headroom). The 64 MiB ceiling is hard: moon builds its client with `max_decoding_message_size(64MB)`, so a larger `B` makes moon unable to decode its own `BatchReadBlobs` response. **[SRC]** moon `helpers.rs:173-183,275-280`; `grpc_remote_storage.rs:535-539`.

**⚠️ CORRECTION — `SupportedCompressors` / `SupportedBatchUpdateCompressors` advertise `IDENTITY` ONLY; drop `ZSTD`.** Advertising a compressor **obliges us to serve it**, in two incompatible framings (ByteStream = one continuous frame split at arbitrary block boundaries because Bazel never sets `setCloseFrameOnFlush`; `BatchUpdateBlobs` = one one-shot frame per blob via `zstd::encode_all`), plus the compressed `write_offset` mongrel and the 9-byte empty-zstd-frame case — and a half-correct zstd path **corrupts blobs under a valid digest**, strictly worse than not offering it. Both clients default compression **off**; a moon user who sets `compression: 'zstd'` gets one warning and a working uncompressed cache, a Bazel user who sets `--remote_cache_compression` gets a loud, self-explaining connection error. Any `compressed-blobs/` resource name → `codes.Unimplemented`. zstd is its own milestone with its own test matrix. **[SRC]** `RemoteServerCapabilities.java:243-247`; moon `falls_back_to_identity_when_compression_unsupported`.

**⚠️ CORRECTION — `ExecEnabled: false` is right, but the whole `ExecutionCapabilities` message is best omitted.** The proto's own guidance: *"CAS + Action Cache only endpoints should return `CacheCapabilities`"* (`remote_execution.proto:569-579`). Client behaviour is identical either way.

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

### 2.4 sccache

`SCCACHE_WEBDAV_ENDPOINT` + `SCCACHE_WEBDAV_KEY_PREFIX` (required) + `SCCACHE_WEBDAV_TOKEN`/`_USERNAME`/`_PASSWORD`.

**⚠️ CORRECTION — sccache is NOT "plain GET/PUT", and it is NOT on the `/ac/` mount.** The earlier claim ("WebDAV in name only — it needs `PUT`, not `PROPFIND`") is **false and dangerous**. sccache v0.16.0's WebDAV mode is opendal 0.55 (`webdav.rs` is 45 lines wrapping `services::Webdav`), whose `write()` calls `webdav_mkcol(get_parent(path))` **before every write** — and `webdav_mkcol` is a `PROPFIND` loop (`opendal backend.rs:250-258`, `core.rs:301-331`). So **every sccache write is PROPFIND(s) → MKCOL(s) → PUT.** A 405/400 on the PROPFIND maps to `ErrorKind::Unexpected` (`error.rs:29-41`), which sccache's `check()` swallows into `can_write = false` → `CacheMode::ReadOnly` for the **whole process** (`cache.rs:283-297`, `server.rs:471-493`): reads work perfectly, writes never happen, the cache never populates, one stderr line. This is M4's hashserv-401 trap.
Nor does it use `/ac/`: `normalize_key` shards **every** key — `format!("{}/{}/{}/{}", &key[0..1], &key[1..2], &key[2..3], &key)` (`sccache/src/cache/utils.rs:20-22`) — so the real path is `{endpoint}/{key_prefix}/a/b/c/<64-hex>`. It needs its OWN route (`/cache/{org}/{proj}/sccache/{path...}`, literal `sccache` segment) answering **PROPFIND** and **MKCOL**, in its own namespace. **`SCCACHE_WEBDAV_KEY_PREFIX` is required** and was omitted from the config in `client-config.md`. sccache's own docs list ccache's HTTP backend and Bazel Remote Caching as compatible backends.

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
- Mirror URLs must be **domain roots** — no path, fragment, or query. **⚠️ CORRECTION — this is true only of registry:2's own `remoteurl`, NOT of Docker Engine.** Docker Engine v28.5.2's `ValidateMirror` rejects only a query string, a fragment, or embedded userinfo in a `registry-mirrors` entry — a path is a passing test case (`config.go` + `config_test.go:67-72`, verified). That is exactly what makes Docker Engine supportable as a fourth client (§3.5): `registry-mirrors: ["https://bakery.corp/cache/{org}/{proj}/docker"]`, Hub-only, same path shape containerd uses.
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

**⚠️ CORRECTION — the HEAD digest fast path needs a HEADER PAIR, not one header.** containerd's resolver only takes the fast path when `Docker-Content-Digest` is present **AND** `Content-Length != -1` **on the same response** (`resolver.go:382`, verified: `if dgstHeader != "" && size != -1`). Missing either forces a full GET-and-hash — correct, but it burns the whole body transfer the HEAD was supposed to avoid. `Content-Type` is trusted verbatim as `desc.MediaType`, so it must be the STORED media type, never a guess.

**⚠️ CORRECTION — podman/CRI-O ping the HOST ROOT, not the mirror path.** containers/image's `GET /v2/` probe hits the bare upstream host — `https://bakery.corp/v2/`, outside any `/cache/{org}/{proj}/...` prefix — and hard-errors on anything but 200 or 401. containerd never issues this call during a pull; it is podman-and-friends-only, and it is **required**, not optional, for that family. `Docker-Distribution-API-Version: registry/2.0` is not enforced by any of the four clients — worth sending because a human curling the endpoint looks for it, not because a client checks it.

### 3.3 The token flow (both directions)

1. Client hits `/v2/…` unauthenticated → **401** + `WWW-Authenticate: Bearer realm="…",service="…",scope="repository:<name>:pull"`
2. Client `GET <realm>?service=…&scope=…` (optionally with `Authorization: Basic <user:pat>`)
3. → `{"token":"…","expires_in":300,"issued_at":"…"}`
4. Client retries with `Authorization: Bearer <token>`

We perform this dance **upstream** (per-repository, caching tokens until `expires_in`), and we must **also issue our own downstream 401 challenge** — otherwise we are an open relay for whatever upstream credentials we hold.

Upstream realms: Docker Hub `https://auth.docker.io/token` (service `registry.docker.io`), ghcr `https://ghcr.io/token`, quay `https://quay.io/v2/auth`.

**⚠️ ADDITION — containerd's hosts.toml has a static, zero-round-trip alternative to the whole dance.** A `[host."...".header]` table sets unconditional per-request headers — including `Authorization = "Bearer <token>"` — applied to every request with no challenge, no `/v2/token` fetch, and no negotiation. It is the recommended default for a token that is itself the credential (as ours is): the challenge flow exists for clients that must discover a realm at runtime, not for one that already knows exactly what to send. `client-config.md` documents both paths side by side.

**⚠️ ADDITION — the Basic-vs-Bearer asymmetry across clients is load-bearing.** BuildKit's Basic-auth code path only installs a per-host handler when **both** `username` and `secret` are non-empty (`authorizer.go`) — a credential-less BuildKit, or one with an empty password configured for a host, silently **skips the mirror entirely** rather than trying it unauthenticated. Bearer has no such gap: every one of the four clients falls through to an anonymous token request when no credential is configured. This is why the downstream challenge in §3.3 is **Bearer, never bare Basic** — a bare-Basic 401 would defeat BuildKit outright, and Bearer is the one scheme all four clients handle gracefully with no credential at all.

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

### 3.5 Path prefixes — four clients, two shapes

| Client | Config | Resulting request |
|---|---|---|
| **containerd** | `server = "https://bakery/cache/acme/proj/docker"` | `GET /cache/acme/proj/docker/v2/library/alpine/manifests/latest?ns=docker.io` |
| **Docker Engine** ⚠️ (M5 addition) | `registry-mirrors: ["https://bakery/cache/acme/proj/docker"]` | `GET /cache/acme/proj/docker/v2/library/alpine/manifests/latest` — **no `ns=`**, Hub-only |
| **BuildKit** | `mirrors = ["bakery/acme/proj"]` | `GET /v2/acme/proj/library/alpine/manifests/latest?ns=docker.io` |
| **podman/CRI-O** | `location = "bakery/acme/proj"` | `GET /v2/acme/proj/library/alpine/manifests/latest` — **no `ns=`** |

containerd **appends `/v2` itself** (and won't double-append if you write it). Docker Engine's `registry-mirrors` uses the SAME shape as containerd's `server=` — a path prefix before an implicit `/v2` — because `ValidateMirror` normalizes the configured URL to end in `/` before Go's URL-joining logic resolves the request path against it (`urls.go`'s `clonedRoute.URL`); an un-normalized path would silently drop the final segment. BuildKit puts the prefix **after** `/v2` — the opposite of both — and podman rewrites the reference the same way, so Bakery serves BuildKit and podman/CRI-O off ONE route family (`/v2/{org}/{project}/...`) and containerd/Docker Engine off the other (`/cache/{org}/{project}/docker/v2/...`).

**⚠️ ADDITION — podman/CRI-O also ping the bare host root**, outside any tenant prefix (`GET https://bakery/v2/`), and hard-error on anything but 200/401 — see the §3.2 correction above. This is a THIRD, tenant-less endpoint, distinct from both path shapes in the table.

`override_path = true` on containerd means "use the path as-is, don't append `/v2`" — which can be used to **normalize containerd onto BuildKit's shape** if we'd rather have one route. We do not: Docker Engine cannot be normalized this way (its mirror URL always gets an implicit `/v2`, with no `override_path` equivalent), so two shapes are unavoidable regardless of what we do with containerd.

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

⚠️ **As landed (M5), the guarantee is STRUCTURAL rather than a boot-time self-test.** An earlier revision of this doc called for a self-test at boot; that would require writing through a tenant `cache_backends` row before any tenant exists — the wrong layer. What ships instead is stronger: the storage key for every manifest **is** the sha256 Bakery computes over the bytes it actually received (`storage.KeyOf`), verified on ingest by `blob.VerifyDigest` — so re-serialized bytes *cannot be stored under the digest we advertise* — and the `oci-conformance` gate round-trips **vendored real multi-arch index bytes** through the full storage path and asserts the digest on every CI run. Nothing in `internal/cache/oci` parses manifest bytes at all.

⚠️ **containerd 1.7 LTS: verified compatible (v1.7.34, 2026-08-14).** Every claim this section family makes about containerd was re-verified against 1.7.34 (the most-deployed LTS on Kubernetes nodes), not just 2.x: `?ns=` behavior, the hosts.toml `/v2` append + `override_path`, the HEAD fast-path header pair, mirrors-before-`server=` ordering with silent fallback, and the POST-first token dance (verbatim-identical `authorizer.go`) all **hold**. One difference: 1.7 still *parses* Docker schema1 manifests if an upstream serves one, where 2.x hard-rejects them. Bakery never emits schema1, so this matters only if a proxied upstream still serves it — in which case a 1.7 client will accept what a 2.x client refuses; nothing for Bakery to do.

### 3.8 Manifest TTL and stale-while-revalidate

- Digest-pinned manifests: **immutable**, never expire, never revalidate.
- Tags: freshness is **derived** (`now > updated_at + tag_ttl`, default ~10 min), not a stored `expires_at` — no column can drift out of sync with the row it describes, and re-tuning the TTL takes effect on every already-cached tag instantly, with no migration.
- **On a stale tag, serve the cached manifest immediately and refresh in the background.** Zero added latency, Docker Hub rate limits stop mattering, and an upstream outage becomes a non-event. Also serve stale (not 5xx) when the upstream fetch fails, indefinitely.

**⚠️ CORRECTION — this is a DELIBERATE IMPROVEMENT over the ecosystem reference, not parity with it, and the doc previously implied the opposite.** `distribution`'s own `registry:2` pull-through proxy does **synchronous remote-first tag resolution on every single pull** — the remote is tried unconditionally and local is only a fallback on failure (`proxytagservice.go:22-39`, verified) — so every cached-image pull still pays a full upstream round trip. Its "TTL" is a **7-day content-eviction timer**, not a freshness horizon: nothing in the reference implementation ever serves a tag without asking upstream first. There is also no normative spec to be consistent with here — the OCI distribution spec's own content-negotiation document is still a TODO in upstream's repo. So "stale-while-revalidate" is not this design lagging behind, or matching, some spec; it is choosing NOT to put the upstream on the hot path of a cache hit, which is the entire point of running a mirror. Cite `distribution`'s source when explaining this to someone who expects registry:2 parity, not a spec section — there isn't one.

A build cache that fails closed when Docker Hub is down is a build cache that isn't doing its job.

### 3.9 Docker Hub rate limits

**⚠️ CORRECTION — the window is SERVER-SUPPLIED, never hardcode it.** The table below (originally "verified July 2026", citing a 6-hour window) is **already stale**: a live `curl` against `registry-1.docker.io` on 2026-08-14 returned `ratelimit-limit: 100;w=3600` — a **1-hour** window, not 6. Docker's own docs and the live header disagree, and the header is the one that is true at request time. Bakery MUST parse `w=` out of the `ratelimit-limit`/`ratelimit-remaining` response headers on every upstream request and publish it as `bakery_oci_upstream_ratelimit_remaining{upstream}` — never bake in a number from any doc, including this one. Treat the table as an illustration of the SHAPE of the limit, not its current value.

| Tier | Limit | Window (illustrative — read `w=` from the live header) |
|---|---|---|
| Unauthenticated | **100 pulls per IPv4 / IPv6 /64** | server-supplied |
| Personal (authenticated, free) | 200 pulls | server-supplied |
| Pro / Team / Business | Unlimited | — |

The widely-cited "10/hour unauthenticated" figures from the 2025 announcements are **superseded**.

A "pull" = a manifest version check **plus** any resulting download. Multi-arch images count **once per architecture**.

**⚠️ ADDITION — HEAD is free.** A `HEAD` manifest request (the digest fast path, §3.2) does not decrement the rate-limit counter (verified live). This matters directly for the stale-while-revalidate design in §3.8: a background revalidation that only needs to confirm a tag has not moved should HEAD, not GET, and costs nothing against the budget while doing it.

⇒ An authenticated pull-through cache moves the whole org from the shared-IP unauthenticated bucket (catastrophic behind NAT'd CI) into an authenticated one, and collapses N runners into one upstream pull per digest.

---

## Sources

- moon: [remote-cache guide](https://moonrepo.dev/docs/guides/remote-cache) · [`remote_config.rs`](https://github.com/moonrepo/moon/blob/master/crates/config/src/workspace/remote_config.rs) · [`grpc_remote_storage.rs`](https://github.com/moonrepo/moon/blob/master/crates/cache-remote/src/grpc_remote_storage.rs) · [`http_remote_storage.rs`](https://github.com/moonrepo/moon/blob/master/crates/cache-remote/src/http_remote_storage.rs)
- REAPI: [`remote_execution.proto`](https://github.com/bazelbuild/remote-apis/blob/main/build/bazel/remote/execution/v2/remote_execution.proto) · [`bytestream.proto`](https://github.com/googleapis/googleapis/blob/master/google/bytestream/bytestream.proto) · [buchgr/bazel-remote](https://github.com/buchgr/bazel-remote)
- ccache: [manual](https://ccache.dev/manual/latest.html) · [HTTP storage wiki](https://github.com/ccache/ccache/wiki/HTTP-storage) · [`httpstorage.cpp`](https://github.com/ccache/ccache/blob/master/src/ccache/storage/remote/httpstorage.cpp)
- sccache: [Webdav.md](https://github.com/mozilla/sccache/blob/main/docs/Webdav.md)
- Docker: [Registry API V2](https://distribution.github.io/distribution/spec/api/) · [token auth](https://distribution.github.io/distribution/spec/auth/token/) · [containerd hosts.md](https://github.com/containerd/containerd/blob/main/docs/hosts.md) · [Hub pull limits](https://docs.docker.com/docker-hub/usage/pulls/)
