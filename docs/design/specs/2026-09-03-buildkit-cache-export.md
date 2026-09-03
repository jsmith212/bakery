# BuildKit cache export: a writable OCI namespace (`registry` backend kind)

Status: approved 2026-09-03 (owner). Requester brief: "Bakery BuildKit Cache
Export" (arcturus build tooling, 2026-09-03).

Bakery's OCI backend is pull-through only. BuildKit's
`--cache-to type=registry` is a registry *push* — blobs, one config blob, a
manifest under a tag — so today the export dies on its first
`POST .../blobs/uploads/` (which does not even reach `errPushPath`: the OCI
routes register `GET` only, so write verbs fall through to the bare `/v2/`
NotFoundHandler). This spec adds the smallest writable surface that makes the
export work, as a new backend kind.

**Product framing (owner decision): cache-first.** The wire surface is a real
registry push and cannot be restricted by content (we never parse what we
store), but v1 documents, snippets, gates and supports ONLY BuildKit cache
export. Pushing ordinary images will work and is explicitly unsupported:
no listing UI, no deletion API, no `docker push` snippet.

## §1 Client behavior this spec is built against (verified from source)

Pinned: BuildKit v0.33.0 (`63956db`), containerd v2.3.4 (`18c051a`) — the
containerd version BuildKit's go.mod vendors. Corrections to the requester
brief are marked ⚠.

- ⚠ **Auth needs no new machinery.** BuildKit installs its OWN authorizer on
  every RegistryHost (`util/resolver/pool.go:204` — containerd's
  `authorizer.go` is dead code on this path). It computes
  `repository:<name>:pull,push` scopes from the operation itself
  (`ContextWithRepositoryScope`, pusher.go:81) and merges the challenge's
  `scope=` only additively (`GetTokenScopes`). Bakery's existing scope-less
  `Bearer realm=...,service="bakery"` challenge and echo-the-key token
  endpoint work for push UNCHANGED. In the token response, `access_token`
  wins over `token` when both are set (fetch.go:225-227); Bakery sets both.
  Anonymous token fetches are GET-only; credentialed fetches try GET, then
  fall back to OAuth2 POST on 405/404/401.
- ⚠ **A failed export hard-fails the build by default.** `ignore-error=true`
  on `--cache-to` is the only thing that makes it soft
  (`solver/llbsolver/export.go:139`, `control/control.go:496-500`). The
  snippet MUST emit it: a Bakery outage must not fail arcturus builds.
  Import failure is unconditionally soft (Debug log + errored vertex, cold
  build; `bridge.go:329-335` discards the error).
- **Blob push:** HEAD blob — 200 = exists (skip), 404 = proceed, anything
  else is a hard client error. POST `blobs/uploads/` — 200/202/204 all mean
  "upload started, parse `Location`" (relative Location is resolved against
  the answering host); 201 means a cross-repo mount succeeded.
  `Docker-Upload-UUID` is never read. Upload is always ONE monolithic PUT to
  `Location` + `?digest=` — containerd has no chunked (PATCH) upload at all
  (pusher.go:310 TODO). The final PUT must answer 200/201/204 — **202 is
  rejected** (pusher.go:329). `Docker-Content-Digest` on the PUT response is
  verified iff present.
- **Manifest push:** same `push()` code path. HEAD `manifests/<tag>` first;
  a 200 whose `Docker-Content-Digest` EXACTLY matches the descriptor digest
  skips the PUT. PUT accepts 200/201/204.
- **Cross-repo mount:** sent only when the client has a
  `containerd.io/distribution.source.<host>` label for the blob; answering
  202 (ignore the mount, normal upload) is always safe; only 201
  short-circuits.
- **Errors:** containerd parses the OCI `{"errors":[...]}` envelope when the
  body carries one (errcode.go:290-303) — emit it on push failures.
- **Import:** HEAD then GET `manifests/<tag>` with containerd's default
  Accept set (both docker and OCI manifest+index types), config blob GET,
  layer blob GETs lazy with `Range` honored. 404 on the tag is the clean
  "cache not found" miss. Import resolves through the same buildkitd.toml
  registry config as pulls.
- **The two manifest shapes** (store verbatim, NEVER parse or validate —
  the existing OCI invariant extends unchanged): default = an OCI image
  *index* whose `manifests[]` entries are blob descriptors (trips validating
  registries); `image-manifest=true` = an OCI image *manifest* whose config
  media type is `application/vnd.buildkit.cacheconfig.v0` (trips
  config-allowlisting registries). Bakery accepts both by construction,
  because it parses neither.

## §2 Routing and the new kind

**Ref shape:** `<host>/{org}/{project}/buildcache/<repo>:<tag>`, e.g.
`bakery.corp/acme/proj/buildcache/impulse:main`. Clients prepend `/v2`, so
requests land on:

```
/v2/{org}/{project}/buildcache/{rest...}                (BuildKit, podman)
/cache/{org}/{project}/docker/v2/buildcache/{rest...}   (route-family twin)
```

Registered as LITERAL-segment patterns beside the existing mirror
`{rest...}` patterns; ServeMux precedence (literal beats wildcard) routes
them with no `splitRef` changes and no registration panic. Methods:

- `GET` (matches HEAD): serve from cache only.
- `POST`: only `.../blobs/uploads/` (with or without `?mount=&from=`).
- `PUT`: blob upload completion and manifest-by-tag.
- Everything else (PATCH, DELETE): ServeMux's own 405 — correct, containerd
  never sends them.

**New backend kind `registry`** (enum value added by migration; the
`(project_id, kind)` unique means one writable namespace per project). An
absent or disabled `registry` row 404s the whole `buildcache` subtree —
never a mount that cannot serve, per the standing invariant. The kind's
`config` jsonb is empty in v1 (no upstreams — there IS no upstream).

**Accepted, documented shadow:** on a project WITH a `registry` backend, a
mirror pull of an upstream repo literally named `buildcache/...` resolves to
the writable namespace instead of the mirror (a real repo whose first name
component is `buildcache` is vanishingly rare; the `token` reserved-slug
precedent is the same class of collision, resolved the same way — by
declaration). The repo tail inside the writable namespace is otherwise
unrestricted, exactly like the mirror's.

**Reserved slugs:** unchanged. `buildcache` sits inside `{rest...}`, not in
the org/project segments, so the denylist is not involved.

## §3 Storage model

Everything reuses `blob.Service` + `cache_objects` under the NEW backend's
own `backend_id` — namespaces are backend-scoped, so the OCI trio is reused
verbatim with no schema change beyond the enum value:

| namespace   | key                | value                        | overwrite |
|-------------|--------------------|------------------------------|-----------|
| `blobs`     | digest hex         | blob bytes                   | no (immutable, dedup) |
| `manifests` | digest hex         | manifest bytes verbatim      | no (immutable) |
| `tags`      | `<repo>:<tag>`     | manifest bytes (dedup'd)     | yes (mutable tag) |

- The manifest digest is SELF-COMPUTED over received bytes
  (`storage.KeyOf` + `blob.VerifyDigest`), never trusted from the client —
  same rule as the mirror, same reason.
- Blob PUTs stream `r.Body` straight into `blob.Put` with
  `VerifyDigest(<digest from ?digest=>)` — hash-as-you-copy, no buffering,
  no size cap (same posture as sstate; multi-hundred-MB layers are normal).
  Digest mismatch → 400 + OCI envelope (`DIGEST_INVALID`).
- The tag key has no upstream-host prefix (the mirror's tag keys are
  `<host>/<name>:<tag>`; the writable namespace has no host). Tag PUT is
  `Overwrite: true` — every export overwrites the tag; the previous
  manifest row becomes untagged and ages out (§5).
- Empty request bodies on PUT are rejected 400 (an empty AC-style value has
  no meaning here; the empty blob `e3b0c442…` with `?digest=` matching is
  legal and stores normally).

## §4 Auth

- **Reads** follow the row's `ReadAuthRequired`, exactly like the mirror:
  open backend serves anonymously; closed backend challenges (the Bearer
  challenge on ping and 401s — machinery unchanged).
- **Every write verb requires a write-scoped key** — the standing cache
  invariant, no representable unauthenticated-write state. No credential →
  401 + the Bearer challenge; authenticated but not write-scoped →
  403 + OCI envelope (`DENIED`). Enforced per-request via
  `Principal.CanWriteProject` (already on the interface; the client leg
  finally uses it).
- A miss on this namespace NEVER contacts any upstream — structurally:
  the registry backend is constructed without an Upstream at all, not with a
  checked flag. The anonymous-open-relay class of bug cannot recur here
  because there is nothing to relay to.
- Foreign (non-`bkry_`) credentials: same shape gate as the mirror —
  discarded before any DB probe; on an open backend treated as anonymous
  for reads. For writes a foreign credential is "no credential" → 401.

## §5 GC and quota

- `stagesFor(registry)` = the OCI ladder verbatim: sweep `tags` (W) →
  `manifests` (2W) → `blobs` (2W), same anti-join sparing manifests a live
  tag still names, same `p.hasWindow` zero-window guard. The named must
  outlive the namer; no new ordering rules.
- Blob liveness is `accessed_at` (imports touch what they read). A blob a
  live tag still names CAN age out if never imported — the failure mode is
  BuildKit's unconditionally-soft import miss (cold build), never a broken
  build. Recorded follow-up, not v1: manifest→blob reachability touching.
- **Default retention 30 days** (the `backends.sql` CASE gains a
  `'registry'` arm), same as `oci`. **Quota IS allowed** for this kind
  (unlike `oci`/`hashserv`): `mode=max` caches run multi-GB by design and a
  per-project ceiling is the operator's only defense. Quota eviction reuses
  the same stage order.
- `downloads`-style archive exemption does NOT apply: this kind is churn by
  design.

## §6 Wire surface (server obligations)

| Request | Response |
|---|---|
| `HEAD/GET blobs/<digest>` | 200 + `Docker-Content-Digest`, `Content-Length` (GET honors `Range`) / 404. |
| `POST blobs/uploads/` (± `mount`) | 202 + `Location: <path>/blobs/uploads/u` (relative; stateless — the PUT carries everything) . Mount requests also 202 (fall back to upload; always safe). |
| `PUT <Location>?digest=<d>` | Stream→verify→store. 201 + `Docker-Content-Digest`. Mismatch → 400 `DIGEST_INVALID`. Existing blob → 201 (idempotent). |
| `HEAD manifests/<tag>` | 200 + exact `Docker-Content-Digest` + `Content-Length` + stored `Content-Type` / 404. This header is what lets clients skip redundant pushes. |
| `GET manifests/<tag or digest>` | Stored bytes verbatim, stored `Content-Type`, `Docker-Content-Digest`. 404 miss, no upstream, ever. |
| `PUT manifests/<tag>` | Store verbatim (§3), point tag. 201 + `Location` + `Docker-Content-Digest`. |
| any push-path failure | Proper status + OCI error envelope. |

Metrics: the new kind labels `backend="registry"`; requests ride the
existing `r.Pattern` middleware (new patterns = new label values, still a
closed set). Blob/object put+hit counters come free via `blob.Service`.

## §7 Console and snippet

- `registry` joins every kind surface: enum migration + sqlc, metrics label,
  `backendKindOf`, `backendQuotaPatch` (allowed), `stagesFor`, web
  `BackendKind` union, `KIND_META`/`KIND_ORDER`, `KNOWN_KINDS`,
  `backendEndpoints`, snippet tool mapping, docs. (The full touchpoint list
  is in the design notes; it is mechanical and closed.)
- New snippet tile **"BuildKit cache"** (tool id `buildcache`, kind
  `registry`): emits `docker buildx create --driver docker-container`, the
  `docker login` line (token in both fields, as everywhere), and paired
  `--cache-to`/`--cache-from` lines with
  `mode=max,image-manifest=true,oci-mediatypes=true,ignore-error=true` and
  the project's real `buildcache` ref. Gotcha line: export hard-fails the
  build without `ignore-error=true`; the tile's warning also notes the http
  (`[registry."…"] http = true`) requirement for non-TLS deployments.

## §8 Conformance gate

`just registry-conformance` + a CI job in the house style (a skip FAILS):

1. Real `docker buildx` container driver (`--driver-opt network=host`, a
   buildkitd.toml marking the Bakery host `http = true`) against a live
   `bakery serve`: cold build with `--cache-to`, `buildx prune -af`, warm
   build with `--cache-from` asserting "importing cache manifest" and ≥1
   CACHED step. Both manifest shapes (`image-manifest=true` and the index
   default).
2. Read-only key: export must fail 401/403 (and the build still completes,
   because the snippet's `ignore-error=true` is under test too); import must
   still work.
3. `skopeo inspect --raw` returns the stored manifest bytes unchanged.
4. Anti-regression: zero upstream requests during the entire gate (there is
   no upstream to hit; assert the fake-upstream counter stays 0 on the
   mirror backend of the same project).

## §9 Out of scope (recorded)

PATCH chunked upload; cross-repo mount 201 fast-path; DELETE/untag; repo and
tag listing (UI or API); generic image hosting and `docker push` support;
manifest→blob reachability marking (follow-up); per-repo quotas.

## §10 New invariants to record in CLAUDE.md on landing

1. The writable `buildcache` namespace never contacts an upstream —
   structurally (no Upstream constructed), and a miss is a clean 404.
2. Registry-kind manifest digests are self-computed; content is stored and
   served verbatim; both BuildKit cache manifest shapes must round-trip
   unparsed.
3. The final blob PUT must answer 201 (never 202 — containerd rejects it),
   and HEAD `manifests/<tag>` must answer the exact stored
   `Docker-Content-Digest` (it is what makes repeat exports cheap).
4. The BuildKit snippet always carries `ignore-error=true` — without it a
   Bakery outage fails every consumer build.
5. GC: the registry kind rides the OCI ladder (tags→manifests→blobs) under
   its own backend_id; quota is allowed for this kind.
