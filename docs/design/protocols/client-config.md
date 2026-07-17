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

One file **per upstream registry namespace**, all pointing at the **same** Bakery endpoint.

```toml
# /etc/containerd/certs.d/docker.io/hosts.toml
server = "https://registry-1.docker.io"
[host."https://bakery.corp/cache/{org}/{proj}/docker"]
  capabilities = ["pull", "resolve"]
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

---

## BuildKit

```toml
[registry."docker.io"]
  mirrors = ["bakery.corp/cache/{org}/{proj}/docker"]
```

**⚠️ BuildKit puts the path prefix AFTER `/v2`** (`path.Join("/v2", mirrorPath)`) — the opposite of containerd. So the request is:

```
GET /v2/cache/{org}/{proj}/docker/library/alpine/manifests/latest?ns=docker.io
```

Bakery serves both shapes.

---

## Not supported (and why)

| Client | Why |
|---|---|
| **Plain `docker pull` (dockerd)** | `registry-mirrors` only ever mirrors Docker Hub, and the mirror URL must be a bare domain root — no path, so no project. Multi-registry is impossible without a MITM CA proxy. |
| **podman / CRI-O** | `registries.conf` supports path prefixes, but containers/image **never sends `?ns=`**, so Bakery cannot learn the upstream from the request. Would require encoding the registry in the path and a per-project `default_upstream`. Deferred. |
| **Yocto < Scarthgap 5.0** | No WebSocket transport, no hashserv auth, no GC. |
| **Binary package feeds (ipk/deb/rpm)** | Out of scope — that's a repository server with a mutable index, not a cache. |
