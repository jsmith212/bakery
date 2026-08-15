# SPA → API wiring wave

**Status:** approved for implementation, 2026-08-15.
**Provenance:** 5-seat research wave → adjudicated memo → 21-finding adversarial critique
(journal `wf_ad4dce6f-535`); the critique's verdict ("close; gap narrow and enumerable") is
folded here in full — where this spec and the memo disagree, the critique won. Product
decisions confirmed by the owner the same day. This spec supersedes both.

## 1. Product decisions (confirmed 2026-08-15)

1. **Key minting stays refused without a project role.** The server keeps capping key
   scope at the caller's own project role. The UI renders the mint control disabled with
   teaching copy; `scope_exceeds_role` gets a first-class client path. (An org admin
   self-grants a role in two clicks, visibly, with provenance.)
2. **GC gets org-visible sweep summaries** (deviation from the memo): per-backend deletion
   attribution recorded by the engine per run, surfaced to org viewers. Backend item B7.
   The full `/gc` operations screen stays site-admin-only.
3. **Time-series is planned-later.** Charts are deleted this wave with honest empty states
   pointing at `--metrics-addr` + Prometheus; `StatTile`/`Sparkline`/`TimeSeriesChart`
   stay in the library for a future rollup milestone.
4. **Personal access tokens: placeholder, not deletion.** The `/user` section renders a
   "Planned" card: no such credential exists yet; use `bakery login` (CLI) and
   project-scoped keys. A user-scoped credential is a future milestone.
5. Defaults taken without asking: grant-roles-to-existing-users only (no invite emails —
   and NO new endpoints for it, see §3.2); single-issuer OIDC login; no org-level
   `read_auth_required` default; org slugs stay immutable.

## 2. Ground truth (corrected by the critique — build against THIS)

- Error dispatch is **by `error.code`, never by HTTP status** (`errors.go:17-19`).
  `scope_exceeds_role` is **403** with `field:"scope"` — not 422. The closed
  `ApiErrorCode` union includes `claim_derived_role` and `not_implemented`.
- A login-gated account **never gets a session**: `/me` can only 401, never 403.
  The reachable failure is `HandleCallback` writing raw JSON 403 at
  `/api/v1/auth/callback` — fixed server-side (B8).
- `PUT/DELETE …/members/{user}` and `/site-admins/{user}` **already accept an email**
  in `{user}` (`resolveUser`, `members.go:528-563`). No grant-by-email endpoints.
- Vitest **is** CI-gated today via `just coverage` in the `build` job. Adding `web-test`
  to `just check` is a speed/locality improvement, not a gap fix.
- `GET /gc/runs` pagination **clamps** limit at the ceiling, never rejects
  (`gc.go:155-173`); every new list endpoint copies that behavior.
- CSRF: mutations must send `Content-Type: application/json` **iff a body is present**;
  bodyless POST/DELETE must send neither body nor header (`requireJSON`,
  `authz.go:417-440`). This is the #1 implementation trap; it has dedicated unit tests.
- `internal/auth`'s own `writeJSON` gains `Cache-Control: no-store` (one line, B8) —
  today `/auth/*` responses carry none.
- Session: `bakery_session`, HttpOnly, SameSite=Lax, 12h/2h, `Secure` auto-off under
  `DEV_LOGIN_ENABLED` (what makes Playwright-over-http work). Session fixation and
  dev-login 404-when-off were attacked and found sound.

## 3. Backend track (lands first; Go tests before any screen consumes it)

**B1 — snippet generator overhaul** (`internal/api/snippets.go`):
- `api.Config` gains `ExternalURL` (thread the existing `--external-url`) and
  `GRPCExternalEndpoint` (new `--grpc-external-endpoint` / `GRPC_EXTERNAL_ENDPOINT`),
  plus `GRPCAddr`. Origin precedence: config > `X-Forwarded-*` > request host.
  Scheme **fails closed to https** unless provably loopback (the OCI `token.go:141-148`
  posture) — a snippet carries a live credential.
- gRPC endpoint: override verbatim when set; else derive host-from-origin +
  **port-from-`GRPCAddr`** (never the HTTP port) with a `sync.Once` warning; `GRPCAddr`
  empty + no override → **409** on `bazel`/`moon` ("no gRPC listener; set --grpc-addr or
  --grpc-external-endpoint"). Loopback derivation is NOT refused (local dev, e2e).
- **The generator reads the project's `cache_backends` and emits blocks only for
  configured+enabled kinds** (critique 5 — today it touches the store zero times).
  A requested tool whose backend kinds are absent gets `Warnings[]` naming what was
  omitted and why; yocto with sstate-but-no-hashserv emits the sstate block, NO
  `BB_HASHSERVE`, and a loud warning (emitting it would point at a 404 and silently
  degrade `bb.siggen` to `unihash=taskhash`).
- Yocto with BOTH backends emits the full five-block `client-config.md` form:
  `BB_SIGNATURE_HANDLER="OEEquivHash"`, `BB_HASHSERVE="wss://{host}/cache/{o}/{p}/hashserv"`,
  and a **separately named** `hashservNetrcLine` (full-URL keying; doc comment notes the
  env-var credential path is deliberately not emitted and would owe
  `BB_ENV_PASSTHROUGH_ADDITIONS`).
- **Preview mode**: request gains `preview: bool`. Preview returns the full config with a
  `«create an API key»` placeholder and **mints nothing**. The screen previews on tile
  select; only an explicit "Generate with a new key" gesture POSTs without `preview`
  (which mints exactly one key). Default scope in the mint path caps at the caller's
  ceiling (read for `reader`) instead of hard-defaulting to write.
- Drift fixes stay as memoed: no `id:secret` anywhere, moon compression note, real
  `bakery sstate push` argv, sccache gotcha text.
- Tests: `TestSnippetYoctoHashservBlockOnlyWhenConfigured` (positive + negative + both
  netrc lines distinct), `TestGRPCEndpointUsesGRPCAddrPortNotHTTPPort` (fails against
  current code), `TestGRPCExternalEndpointOverride`, `TestExternalURLBeatsForwardedHeaders`,
  `TestSnippetSchemeFailsClosed`, `TestSnippetRefusesBazelMoonWhenGRPCDisabled`,
  `TestSnippetPreviewMintsNothing` (assert no api_keys row), config binding tests.

**B2 — usage endpoints** (first readers of `cache_backend_usage`):
`GET /orgs/{org}/usage` (OrgView) and `GET …/{project}/usage` (ProjectRead), the memo's
shapes, one JOIN each, `measured_at` always on the wire (staleness is honest).

**B3 — object browser**: `GET …/backends/{kind}/objects?namespace=&prefix=&after_key=&limit=`
(ProjectRead), keyset over the `cache_objects` PK, prefix as PK range scan, limit
**clamped** (default 50, ceiling 200), `next_cursor` per the gc.go convention. Returns
`accessed_at` (rendered approximately — the toucher ramp makes it up to 24h coarse).

**B4 — org defaults on the wire**: `default_retention_window`/`default_quota_bytes` added
to `Org` + `UpdateOrgRequest` (3-state on update).

**B6 — `GET /instance`** (SiteAdmin): boot-time config echo (version, addrs, external
URLs, oidc issuer, dev-login, gc knobs). Never touches Prometheus.

**B7 — GC org visibility** (product decision 2):
- New table `gc_run_backends (run_id REFERENCES gc_runs, backend_id REFERENCES
  cache_backends ON DELETE CASCADE, objects_deleted bigint, bytes_freed bigint,
  PRIMARY KEY (run_id, backend_id))`, written by the engine once per swept backend per
  real (non-usage, non-dry) run — zeros included, so "swept, nothing eligible" is
  distinguishable from "not swept".
- `GET /orgs/{org}/gc/activity?limit=` (OrgView, clamped): recent runs' per-backend rows
  joined to this org's projects only: `{items:[{run_id, started_at, finished_at, status,
  backends:[{project_slug, kind, objects_deleted, bytes_freed}]}], next_cursor}`.
- Surfaced as a "Retention activity" card on the org projects page. The instance-wide
  `/gc` screen remains site-admin-only.

**B8 — auth polish**: `HandleCallback` denial becomes `302 /login?denied=login_gate`
(the raw-JSON 403 at the callback URL is the reachable failure; gating test
`TestCallbackDeniedRedirectsToLogin` in Go — a Playwright gated-user test is infeasible,
dev-login bypasses reconciliation). `AuthConfig` gains `allow_self_serve_orgs` (critique
10 — gates the create-org affordance). `internal/auth.writeJSON` adds
`Cache-Control: no-store`.

## 4. Client architecture

As memoed §5 with these critique folds:
- `lib/api/`: `types.ts` mirrors **JSON tags, not Go field names** — `Member` uses
  `org_role`/`oidc_role`/`oidc_group`/`local_role`/`org_role_source`; `SiteAdmin` uses
  `site_role`/`site_role_oidc`/`site_oidc_group`/`site_role_local`/`site_role_source`;
  a shared `<Provenance>` component takes a normalized shape via two thin adapters.
- Error table keyed on **code**: `scope_exceeds_role` (403, `field:"scope"`) drives the
  scope selector + teaching copy; `claim_derived_role` renders the server's own message
  in-place (and the Remove control is **disabled when `local_role` is absent**, making
  the 409 a backstop); `validation_failed`/`reserved_slug`/`invalid_slug` attach to the
  field; 401 → central `onUnauthorized`; 403 `forbidden` → in-place state; 409 → toast
  with server message; 404 → EmptyState on load, toast on mutation; 5xx/non-JSON →
  generic toast.
- 3-state update encoding: `undefined` omit / `null` clear / value set; `''` is forbidden
  and rejected client-side (the server's `DisallowUnknownFields` + RawMessage semantics
  make anything else a 400). **Request fixtures** exist beside response fixtures: every
  mutating endpoint has a canonical body round-trip test and an extra-field-rejected test.
- Storage (return-path stash, last-used tenancy) sits behind an injectable
  `Storage`-shaped port (theme.ts idiom) so Vitest stays in `node` env — no jsdom.
  Return path uses the `sessionStorage` stash **only** (it is the only mechanism that
  survives the OIDC round-trip); no `?return=` param. Stashed values are validated
  `^/(?!/)` before `goto`.
- Role gating: one exported `canAdminOrg(me, org)` / `canOwnOrg(...)` helper family, every
  check `me.is_site_admin || …` (a site admin has empty `me.orgs` and `role`-less `Org`
  rows — without the helper they'd be the least-privileged user in the console). `/`
  resolution: stash → localStorage → `me.orgs[0]` → (site admin: first org from
  `GET /orgs`) → `/orgs`.
- `backendStatus()` is **total over a missing usage row**: hashserv never has one
  (structurally — the GC has no stages for it), so hashserv's badge derives from
  `enabled` alone (`●`/`▲`) with no usage dependency; other kinds with no row render `○`
  with an explicit "not yet measured" caption (up to `--gc-usage-interval` old), never
  bare `idle`. `logical_bytes==0` **with** a `measured_at` is honest `idle`. No
  `as BadgeStatus` casts anywhere; `backendStatus()` is the only producer (nominal-typed
  prop rather than an ESLint rule — no JS lint toolchain exists and none is added).
- Tenancy in the path: `/o/[org]/p/[project]/…` exactly as memoed, legacy flat routes get
  one-release redirects. Vite dev proxy + `just dev`.

## 5. Screens

As memo §6 (waves and per-screen plans) with these changes: no B5a/B5b screens work —
member/site-admin grant flows use `PUT` with an URL-encoded email in `{user}` and render
`resolveUser`'s own 404 message; `/login` renders `?denied=login_gate` as the terminal
state (no 403-on-/me state exists); snippets screen previews per tile and mints only on
the explicit gesture, rendering `token_prefix` in the result so the key is findable on
`/keys`; org projects page gains the B7 Retention-activity card; `/user` renders the
personal-tokens **"Planned"** card (product decision 4); `/gc` stays site-admin with the
`include_usage` toggle defaulting off.

## 6. Test gates

- **Vitest** (runs in `build` via `just coverage`; also added to `just check` for fast
  failure): client header matrix (Content-Type iff body — the 415 trap), envelope→
  `ApiError`, non-JSON degradation, 401-hook-once, 204→undefined; exhaustive
  `ApiErrorCode` switch incl. `claim_derived_role`/`not_implemented`; response AND
  request fixtures per endpoint; `backendStatus` incl. the null-usage-row and hashserv
  cases; tenancy resolution via the storage port; provenance adapters (fixtures include
  a claim-derived member — the `org_role_source` tag is the highest-drift-risk spot).
- **Go**: B1–B8 dbtests incl. the 404-vs-403 ladder per new route, keyset stability,
  clamping; the B1 suite above; `TestHeadlessDoesNotRouteTheSPA` extended with a
  `/o/…/p/…` deep link (SPA in console mode, 404 headless).
- **Playwright** (`web/e2e/`, Chromium only, from scratch): config with `webServer.cwd`
  at the **repo root** (`build/bakery serve`, `DEV_LOGIN_ENABLED=1`, readyz health URL),
  `forbidOnly: true`, `retries: 0`. One spec, the DESIGN.md flow: dev-login → create org
  → create project → create **sstate AND hashserv** backends → mint key (token revealed
  once, `bkry_`-prefixed) → snippets: preview yocto, assert `BB_HASHSERVE` + both netrc
  lines present; plus the negative leg (sstate-only project → no `BB_HASHSERVE`, warning
  rendered). Skip detection: `PLAYWRIGHT_JSON_OUTPUT_NAME` + `jq -e '.stats.skipped==0'`
  — no stdout grep. `just web-e2e` (a skip FAILS); CI job `web-e2e` copies a conformance
  job's shape (postgres service, log upload `if: always()`, added to `image.needs`).

## 7. Non-goals

The memo's §10 stands (no time-series endpoints, no `/metrics` exposure, no audit log,
no per-object delete, no CORS/second-origin/bearer-token SPA auth, no TS codegen or
OpenAPI, no cross-browser/visual-regression testing, no org slug mutation, no
multi-provider OIDC, no invite emails) with two amendments: personal access tokens are
**planned-later** (placeholder ships), and GC org-visible summaries are **in scope**
(B7). ESLint for web is explicitly not added this wave.
