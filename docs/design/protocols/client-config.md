# Client configuration — verified reference

Every one of these was verified by reading the client's source, not its docs. Several have gotchas that will cost a day if you don't know them. **This document is the source of truth for the UI's config snippet generator.**

Throughout: `{org}` / `{proj}` are Bakery slugs, `{key}` is a project-scoped API key.

**A Bakery cache key is ONE opaque `bkry_` token, not an `id:secret` pair.** There is no secret half to split off. `auth.AuthenticateCache` reads the token from the HTTP Basic **password** field and falls back to the **username**, so a client that puts the whole token in either field (or both) authenticates. Every snippet below carries the token verbatim — never as `{key_id}:{key_secret}`, which is a credential that cannot exist.

---

## Yocto — `conf/local.conf`

```bash
# --- sstate mirror (read) ---
SSTATE_MIRRORS ?= "file://.* https://bakery.corp/cache/{org}/{proj}/sstate/PATH;downloadfilename=PATH"

# --- source premirror (read) ---
INHERIT += "own-mirrors"
SOURCE_MIRROR_URL ?= "https://bakery.corp/cache/{org}/{proj}/downloads"
BB_GENERATE_MIRROR_TARBALLS = "1"     # if you want git repos mirrored as tarballs

# --- hash equivalence ---
BB_SIGNATURE_HANDLER = "OEEquivHash"  # already the oe-core default
BB_HASHSERVE = "wss://bakery.corp/cache/{org}/{proj}/hashserv"
# Do NOT set BB_HASHSERVE = "auto".
# Do NOT set BB_HASHSERVE_UPSTREAM.  See the warning below.
```

Credentials go in the environment (and must be in `BB_ENV_PASSTHROUGH_ADDITIONS`):

```bash
export BB_HASHSERVE_USERNAME="{key}"
export BB_HASHSERVE_PASSWORD="{key}"
```

The token is opaque and goes in **both** fields — there is no id/secret split.

…or in `~/.netrc`. **The netrc gotcha:** oe-core calls `netrc.authenticators(BB_HASHSERVE)` — an **exact string match on the full URL**, not the hostname:

```
machine wss://bakery.corp/cache/{org}/{proj}/hashserv login {key} password {key}
```

`machine bakery.corp` does **not** work. This is the single most common way to get a silently-unauthenticated build.

For the sstate/downloads HTTP Basic credentials, `~/.netrc` keyed by hostname works normally (token in both fields):
```
machine bakery.corp login {key} password {key}
```

### ⚠️ Why `BB_HASHSERVE_UPSTREAM` is forbidden

`BB_HASHSERVE = "auto"` spawns a **local** hashserv that proxies to `BB_HASHSERVE_UPSTREAM`. That link is **pull-only** — bitbake has no code path that reports a unihash upstream (`bin/bitbake-hashserv` says it plainly: *"Upstream hashserv to pull hashes from"*). It is also **anonymous** — `create_async_client(self.server.upstream)` passes no credentials, and no `BB_HASHSERVE_UPSTREAM_USERNAME` variable exists on any branch.

⇒ In that topology Bakery would **never receive a single hash report**. Use the direct connection.

What you give up: no local sqlite cache, so every lookup is a network round trip. Heavily mitigated by bitbake's batched/streamed queries (`get-stream` pipelines the whole setscene query set into ~1 RTT). Not a correctness issue.

### Pushing to the cache

BitBake cannot write to an sstate mirror — no upload path exists. After a build:

```bash
bakery sstate push {org} {proj} build/sstate-cache
bakery downloads push {org} {proj} build/downloads
```

The CLI HEADs (or batch-`_exists`) to skip what's already there, then PUTs the rest in parallel.

---

## moon — `.moon/workspace.yml`

```yaml
remote:
  api: 'grpc'
  host: 'grpcs://bakery.corp:443'
  auth:
    token: 'BAKERY_TOKEN'          # the NAME of an env var, NOT the token
  cache:
    instanceName: '{org}/{proj}'   # ← this is the project selector for gRPC
    compression: 'none'            # Bakery advertises IDENTITY only; 'zstd' just warns and falls back
```

```bash
export BAKERY_TOKEN="{key}"
```

**Gotchas:**
- `auth.token` is an **env var name**. If the named variable is empty, moon **silently disables the remote cache** with no error. Users will report "caching doesn't work."
- gRPC **cannot carry a URL path** (tonic discards it). The project MUST come from `instanceName`. Slashes are legal and unvalidated — moon passes it verbatim.
- `api: 'http'` also works and gets path routing for free, but moon's HTTP mode has no `FindMissingBlobs`, so it **re-uploads every blob on every build**. Prefer gRPC.

---

## ccache — `~/.config/ccache/ccache.conf`

```ini
remote_storage = http://{key}:@bakery.corp/cache/{org}/{proj} @layout=bazel @connect-timeout=1000
```

or:
```bash
export CCACHE_REMOTE_STORAGE='http://bakery.corp/cache/{org}/{proj}|layout=bazel|bearer-token={key}|connect-timeout=1000'
```

For a **read-scoped** key, add `@read-only=true` (file form) / `|read-only=true` (env form): a 403 on a PUT latches the whole backend — reads included — off for that translation unit, so a read-only client must never issue the PUT.

**⚠️ `http://` ONLY — ccache cannot speak https.** Its built-in HTTP backend has no `https` scheme; it refuses the URL before it opens a connection, and TLS termination in front of Bakery does not help (`storage.cpp` scheme map → `unknown remote storage scheme: https`). This backend is a cleartext-HTTP deployment mode. The upstream-blessed exit is a `ccache-storage-https` helper binary; until then, plaintext only.

**⚠️ The userinfo MUST carry a colon.** ccache's URL ctor throws `core::Fatal("Expected username:password in URL")` on a bare `http://{key}@host` — so the token is the **username** and the password is **empty**: `http://{key}:@host/…`. Bakery's password-then-username fallback authenticates it.

**`@layout=bazel` is required.** It makes ccache write to `/ac/<hash>`. Without it, ccache uses `subdirs` layout (`/<ab>/<cdef…>`) which Bakery does not route on this mount — every GET 404s and the first PUT 404 latches the backend off.

**`@connect-timeout=1000` (ms) — the default is 100 ms**, too tight for a real network round trip.

ccache keys are 40 hex chars; the bazel layout pads them to 64 by appending the first 24 chars of the key to itself (it hashes nothing). Bakery must not assume a `/ac/` key is a real SHA-256.

---

## sccache

```bash
export SCCACHE_WEBDAV_ENDPOINT="https://bakery.corp/cache/{org}/{proj}"
export SCCACHE_WEBDAV_KEY_PREFIX="sccache"
export SCCACHE_WEBDAV_TOKEN="{key}"
```

**⚠️ `SCCACHE_WEBDAV_KEY_PREFIX` is REQUIRED** and was missing from earlier drafts of this doc. sccache shards **every** key under it — the real path is `{endpoint}/{key_prefix}/{a}/{b}/{c}/{64-hex}` (`normalize_key`) — so without a prefix the keys land where Bakery does not serve them.

`SCCACHE_WEBDAV_TOKEN` becomes an `Authorization: Bearer {key}` header, which `AuthenticateCache` already accepts by delegating to the Bearer arm of `Authenticate`.

**⚠️ sccache's WebDAV mode is NOT "plain GET/PUT".** It goes through opendal, whose WebDAV `write()` issues `PROPFIND` + `MKCOL` on the parent directory **before** the `PUT`. It is not on the `/ac/` mount at all — it needs its own `sccache` route that answers PROPFIND and MKCOL, or writes silently degrade to read-only. See `bazel-ccache-docker.md §2.4` for the full correction.

---

## Bazel — `.bazelrc`

```
build --remote_cache=grpcs://bakery.corp:443
build --remote_instance_name={org}/{proj}
build --remote_header=authorization=Bearer {key}
```

- **The project rides in `--remote_instance_name`, not the URL.** gRPC cannot carry a URL path (tonic discards it), so `{org}/{proj}` is the instance name — slashes are legal and passed verbatim.
- **The endpoint needs a scheme and an explicit port.** `grpcs://host:443` for TLS, `grpc://host:PORT` for cleartext. A bare hostname is rejected by the gRPC channel setup.
- **The credential is a `--remote_header`**: `authorization=Bearer {key}`. One opaque token, no colon.
- **⚠️ Do NOT set `--remote_cache_compression`.** Bakery advertises `IDENTITY` only. Bazel **hard-fails the connection** (not degrades) if compression is requested and ZSTD is not advertised — the build errors at channel setup, before a single cache RPC.

---

## containerd — `/etc/containerd/certs.d/<registry>/hosts.toml`

One file **per upstream registry namespace**, all pointing at the **same** Bakery endpoint (the `/cache/{org}/{proj}/docker` mount — containerd appends `/v2` itself).

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"

[host."https://bakery.corp/cache/{org}/{proj}/docker"]
  capabilities = ["pull", "resolve"]

  # DEFAULT: no credential configured -- containerd follows Bakery's WWW-Authenticate:
  # Bearer challenge automatically (the ping answers it even on 200; see the warning
  # below). Zero config beyond this file, and it is enough against an OPEN backend.

  # ALTERNATIVE: skip the challenge round trip entirely with a static per-request
  # header. Mutually exclusive with the default above:
  # [host."https://bakery.corp/cache/{org}/{proj}/docker".header]
  #   Authorization = "Bearer {key}"
```
```toml
# /etc/containerd/certs.d/ghcr.io/hosts.toml
server = "https://ghcr.io"
[host."https://bakery.corp/cache/{org}/{proj}/docker"]
  capabilities = ["pull", "resolve"]
```
…and the same for `quay.io`, `gcr.io`, `registry.k8s.io`.

containerd appends `/v2` to the path itself, and automatically sends `?ns=docker.io` (etc.) because the mirror host differs from the namespace. That query param is how one Bakery endpoint serves every upstream.

Enable the config path (kind example):

```yaml
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
```

**⚠️ Migration note:** the older `[plugins."…".registry.mirrors."docker.io"] endpoint = [...]` style does **not** produce `?ns=` and is removed in containerd 2.0. It must be replaced with `config_path` + `hosts.toml`.

**⚠️ Credentialed containerd (a `docker config.json` entry for this host, not the header alternative above) sends a POST first, not GET.** containerd's authorizer issues an OAuth2 form POST to the token endpoint whenever it holds a secret, falling back to GET only on 405/404/401/400 — and the 405 fallback additionally requires a non-empty username, so an `identitytoken` credential (which has none) has no fallback at all if the server is GET-only. Bakery's token endpoint answers **both** GET and POST for exactly this reason — nothing to configure, just don't assume GET-only if you crib this endpoint elsewhere.

---

## Podman / CRI-O / skopeo — `/etc/containers/registries.conf`

```toml
[[registry]]
  location = "docker.io"

  [[registry.mirror]]
    location = "bakery.corp/{org}/{proj}"
```

**containers/image never sends `?ns=`.** Unlike containerd and BuildKit, podman/CRI-O/skopeo rewrite the image reference itself rather than appending a query hint — so Bakery cannot learn the upstream from the request. The project's OCI backend **must** have `default_upstream` configured (server-side, `cache_backends.config`); absent that, only whatever `default_upstream` names is reachable through this mirror.

**⚠️ Credentials do NOT inherit from a `docker.io` login.** containers/image strips the upstream registry's credentials whenever the mirror's domain differs from the image's own domain (`docker_image_src.go`, verified) — a login stored against `docker.io` never reaches `bakery.corp`. Authenticate to the mirror host directly:

```bash
podman login bakery.corp -u {key} -p {key}
```

**⚠️ podman/skopeo/CRI-O ping the bare host root, not the mirror path** — `GET https://bakery.corp/v2/`, outside any tenant prefix — and hard-error on anything other than 200 or 401. This is a required endpoint for this client family specifically; containerd never issues it during a pull.

---

## BuildKit — `/etc/buildkit/buildkitd.toml`

```toml
[registry."docker.io"]
  mirrors = ["bakery.corp/{org}/{proj}"]
  http = false
```

**⚠️ BuildKit puts the path prefix AFTER `/v2`** (`path.Join("/v2", mirrorPath)`) — the opposite of containerd and Docker Engine. So the request is:

```
GET /v2/{org}/{proj}/library/alpine/manifests/latest?ns=docker.io
```

Bakery serves this off a SEPARATE route family (`/v2/{org}/{proj}/...`) from containerd's (`/cache/{org}/{proj}/docker/v2/...`) — the mirror value above carries no `/cache` and no `/docker` segment; that shape belongs to containerd only.

**⚠️ BuildKit's Basic-auth path only installs a handler when BOTH `username` AND `secret` are non-empty.** A `docker config.json` entry for this host with an empty password does not error — it silently **skips the mirror** for that host, with no log line a user is likely to find. Configure both fields with the token, or configure neither and rely on the anonymous Bearer flow against an open backend:

```bash
docker login bakery.corp -u {key} -p {key}
```

**⚠️ On an OPEN backend (`read_auth_required = false`), `docker login bakery.corp` appears to succeed with ANY password — including a wrong one.** That is not a bug: unrecognized credentials on the read path are deliberately treated as anonymous (Docker Engine forwards real Docker Hub logins to its mirror, and rejecting them would break every `docker login`'d engine), and the token endpoint therefore answers 200 to any credential. A wrong Bakery token on an open backend gets anonymous service — cache hits work, misses 404 and fall back — so "login succeeded" does NOT confirm the token is valid. To verify a token, use it against a `read_auth_required` backend, or check the key's `last_used_at` in the console.

---

## Docker Engine (`dockerd`) — `/etc/docker/daemon.json`

```json
{
  "registry-mirrors": ["https://bakery.corp/cache/{org}/{proj}/docker"]
}
```

**Supported as of M5, with a warning.** `registry-mirrors` only ever mirrors Docker Hub — there is no `?ns=`, no multi-registry routing, and no `default_upstream` question, because Hub is the only upstream this client can name. It uses the SAME `/cache/{org}/{proj}/docker` mount and implicit-`/v2` shape as containerd (Docker Engine v28.5.2's `ValidateMirror` accepts a path prefix — contrary to older advice that mirror URLs must be domain roots, which is true only of `registry:2`'s own `remoteurl`).

**⚠️ CREDENTIAL-TRANSIT WARNING.** Docker Engine forwards the operator's **own real Docker Hub login** to whatever `registry-mirrors` names, unscoped, on every single pull — there is no per-mirror credential slot, so there is no way to give it a Bakery token instead. Two consequences:

1. **Only point this at Bakery if you accept that your Docker Hub credentials transit through it on every pull.** Bakery never logs a forwarded credential.
2. **This only works against an OCI backend with authenticated reads turned OFF.** A forwarded Hub credential is never `bkry_`-shaped, so it is always treated as anonymous — a backend that requires an authenticated read will 401 every request from a plain Docker Engine, and the engine will silently fall back to the real registry (0% hit rate, no error anywhere).

Support-and-warn was a deliberate product decision, not an oversight: leaving the most-common Docker client entirely undocumented does not stop users from pointing it at Bakery anyway.

---

## Not supported (and why)

| Client | Why |
|---|---|
| **Yocto < Scarthgap 5.0** | No WebSocket transport, no hashserv auth, no GC. |
| **Binary package feeds (ipk/deb/rpm)** | Out of scope — that's a repository server with a mutable index, not a cache. |
| **Registry push, `/v2/_catalog`, tags list, delete, referrers** | Bakery's OCI backend is pull-through only. A client that tries one of these gets an honest 404/405 and falls back to the real registry, exactly like a mirror miss. |
| **Docker Schema1 manifests** | Rejected by containerd ≥ 2.0 already; no fallback rewrite exists on Bakery's side either. |
| **Non-sha256 digests (e.g. `sha512:...`)** | go-containerregistry — the library Bakery's upstream client is built on — cannot fetch them either. A clean 404 sends the client to a registry that can. |
