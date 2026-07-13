# Client configuration — verified reference

Every one of these was verified by reading the client's source, not its docs. Several have gotchas that will cost a day if you don't know them. **This document is the source of truth for the UI's config snippet generator.**

Throughout: `{org}` / `{proj}` are Bakery slugs, `{key}` is a project-scoped API key (`id:secret`).

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
export BB_HASHSERVE_USERNAME="{key_id}"
export BB_HASHSERVE_PASSWORD="{key_secret}"
```

…or in `~/.netrc`. **The netrc gotcha:** oe-core calls `netrc.authenticators(BB_HASHSERVE)` — an **exact string match on the full URL**, not the hostname:

```
machine wss://bakery.corp/cache/{org}/{proj}/hashserv login {key_id} password {key_secret}
```

`machine bakery.corp` does **not** work. This is the single most common way to get a silently-unauthenticated build.

For the sstate/downloads HTTP Basic credentials, `~/.netrc` keyed by hostname works normally:
```
machine bakery.corp login {key_id} password {key_secret}
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
    compression: 'zstd'
```

```bash
export BAKERY_TOKEN="{key_id}:{key_secret}"
```

**Gotchas:**
- `auth.token` is an **env var name**. If the named variable is empty, moon **silently disables the remote cache** with no error. Users will report "caching doesn't work."
- gRPC **cannot carry a URL path** (tonic discards it). The project MUST come from `instanceName`. Slashes are legal and unvalidated — moon passes it verbatim.
- `api: 'http'` also works and gets path routing for free, but moon's HTTP mode has no `FindMissingBlobs`, so it **re-uploads every blob on every build**. Prefer gRPC.

---

## ccache — `~/.config/ccache/ccache.conf`

```ini
remote_storage = http://{key_id}:{key_secret}@bakery.corp/cache/{org}/{proj} @layout=bazel
```

or:
```bash
export CCACHE_REMOTE_STORAGE='http://bakery.corp/cache/{org}/{proj}|layout=bazel|bearer-token={key_id}:{key_secret}'
```

**`@layout=bazel` is required.** It makes ccache write to `/ac/<hash>`, which is the same endpoint Bakery serves for moon and sccache. Without it, ccache uses `subdirs` layout (`/<ab>/<cdef…>`) which Bakery does not serve on this mount.

ccache keys are 40 hex chars; the bazel layout pads them to 64 by appending the first 24 chars of the key to itself. Bakery must not assume a `/ac/` key is a real SHA-256.

---

## sccache

```bash
export SCCACHE_WEBDAV_ENDPOINT="https://bakery.corp/cache/{org}/{proj}"
export SCCACHE_WEBDAV_TOKEN="{key_id}:{key_secret}"
```

Same `/ac/` endpoint. "WebDAV" is a misnomer — it's plain GET/PUT.

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
