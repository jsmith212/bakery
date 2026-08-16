# Feedback wave 1 — user tokens, robots, sticky nav, k8s, avatars, S3

**Status:** approved for implementation, 2026-08-16.
**Provenance:** 5-seat research wave → adjudicated memo → 11-finding adversarial critique
(journal `wf_1562b43a-62a`; working copies `/tmp/W1-memo.md`, `/tmp/W1-critique.md`).
**The critique's findings F1–F11 are all folded here and win wherever the memo disagrees.**
Baseline is HEAD (post-M6, post-SPA-wiring) — the memo's `7c3d609` baseline claims are void (F7).
S3 stays in the wave (owner allowed deferral; the design needs no storage-contract rework).

## 1. Owner decisions + defaults taken

Confirmed asks: per-user tokens for credential-free CLI pushes; org robot tokens for CI
("no touchy on anything else"); sticky nav; k8s probes/metrics; OIDC avatars + the copy cut
to "Managed by \<identity provider\>"; S3 if reasonable. Defaults I am taking (flagged for
the owner's post-landing feedback round, not blocking): personal tokens are visible/revocable
by their owner only this wave (no org-admin roster); the ~30–60s deploy outage from
`Recreate` + the boot lock is the accepted singleton cost and the docs say so; avatars
hotlink with monogram fallback (air-gapped installs degrade gracefully).

## 2. THE AUTH SEAM (lands first; gates everything credential-shaped)

1. **Allowlist inversion (F3), BEFORE any new Method exists:** the seven capability
   guards currently keyed `p.method == MethodAPIKey` (IsSiteAdmin, CanViewOrg,
   CanAdminOrg, CanOwnOrg, CanAdminProject, CanReadProject's API-key arm,
   CanWriteProject's API-key arm) are rewritten onto **two unexported allowlists**,
   not one, and which guard takes which is the substance of the change:

   - `principal.isInteractive()` (`MethodSession|MethodBearer|MethodDev`) — "a human
     is at a console". It gates **IsSiteAdmin, CanAdminOrg, CanOwnOrg,
     CanAdminProject** and the org-admin short-circuit inside CanWriteProject.
   - `principal.actsAsUser()` (the three above **plus `MethodUserToken`**) — "this
     credential IS its owner, and reads their live role maps". It gates **CanViewOrg,
     CanReadProject and CanWriteProject's role-map arms**.

   Written with `isInteractive` everywhere, a personal access token could not read
   its owner's orgs at all — which contradicts §3 (`bakery org list` working with a
   PAT is the whole point) and §5's precedence chain. Both are closed switches with
   a trailing `return false` and no `default`, so a new Method defaults CLOSED under
   either. Gate: `TestEveryMethodIsExplicitlyClassified` (a table over every Method
   constant declared **anywhere in package `auth`**, read out of the source by an AST
   scan; adding one unclassified fails). `internal/api` mirrors both allowlists
   (`interactiveMethod`, `actsAsUserMethod`) for its test double, and drives its own
   door gates from the same AST scan so a Method declared in `internal/auth` fails a
   test in `internal/api` with no edit there.
2. **Prefix family:** `bkry_` (api_keys, unchanged) / `bkru_` (user_tokens) / `bkro_`
   (org_tokens). `looksLikeAPIKey` stays bkry_-only. New `auth.LooksLikeBakeryToken`
   (family gate) + `auth.tokenKind` (dispatch switch).
3. **Dispatch at ALL THREE seams (F2):** `AuthenticateCache` (Basic field selection picks
   whichever field is family-shaped, then dispatches by kind), `AuthenticateToken`
   (prefix switch → one validator each; `observeErr` labelled by the token's kind, not
   hardcoded MethodAPIKey — F11), and `oci.isBakeryToken` → `LooksLikeBakeryToken`
   (a forwarded Docker Hub PAT is still discarded pre-probe, verbatim invariant).
   Gate: `TestEveryPlaneAcceptsEveryTokenKind` — {bkry_,bkru_,bkro_} ×
   {Basic-pass, Basic-user, Bearer, hashserv in-band auth, OCI Bearer} — plus
   `TestUserTokenNeverReachesProjectKeyProbe`, `TestForeignCredentialStillDiscarded`.
4. **No token mints a token:** CreateAPIKey/CreateUserToken/CreateOrgToken all refuse
   every non-interactive method.
5. **Two route levels move, and both are behaviour changes for existing `bkry_`
   keys.** Recorded here because they are not derivable from the items above, and
   noted in DESIGN.md's route table:
   - `GET /orgs` goes from `AccessAuthenticated` to **`AccessUserScoped`**. The old
     level admitted an API key, which then received `200 + []` (the list is filtered
     by CanViewOrg, false for a key). It is now a **403**. Without the move, "a robot
     may reach `GET /api/v1/me` and nothing else" is true of the door's table and
     false of the shipped routes.
   - `POST .../keys`, `DELETE .../keys/{key}` and `POST .../snippet` go from
     `AccessProjectRead` to **`AccessProjectCredential`** — same capability floor
     (CanReadProject: a reader mints a read key and a read snippet), a door no
     credential may come through. `GET .../keys` stays at `AccessProjectRead`.
     The two mints were already refused by `requireMintAuthority`; **the revoke was
     refused nowhere**, so a leaked PAT could enumerate its owner's key ids and
     delete every one of them — no escalation, every CI job in the org broken, logged
     as the owner. It is the rule `/user/tokens` already states, applied one file
     over. Gate: `TestUserTokenCannotManageAPIKeys`, driving the real route table.

## 3. USER TOKENS (`bkru_`)

Memo §2 stands (table `user_tokens` in migration `000015` with the sha256 covering
index, soft revoke, `max_scope read|write`, 90d default expiry with never allowed,
one-time reveal, `/api/v1/user/tokens` CRUD session-only, /user UI in the project-Keys
idiom, CLI §5) **with the critique's amendments**:

- **Live roles on BOTH planes via epoch-keyed caching (F4, option b — structural, not
  call-site-maintained):** migration adds `users.authz_epoch bigint NOT NULL DEFAULT 0`
  and triggers on `org_memberships` (I/U/D), `project_memberships` (I/U/D), and
  `users.site_role_*` updates that bump the owner's epoch. The token probe is ONE round
  trip: `user_tokens` sha256 index scan JOIN `users` (PK) returning `authz_epoch`.
  The principal cache (sharded, negative-capable, blob-LRU style) is keyed
  `(user_id, authz_epoch)` — a stale entry is *unreachable* the moment authority
  changes, no Evict calls to forget, sound even under `--allow-multi-instance`.
  TTL 30s stays as GC of dead entries only. Gates: `TestRoleChangeInvalidatesViaEpoch`
  (grant/revoke org role, project role, site role — next request sees new authority,
  no eviction code involved), `TestUserTokenRevocationIsInstant`, plan test on the
  probe (index scan + PK join, one round trip), sharded-cache parallel benchmark.
- **The epoch triggers impose a LOCK ORDER, and every authority-mutating transaction
  obeys it.** The trigger body is `UPDATE users SET authz_epoch = ...`, taken AFTER
  the membership row; the login reconciler takes `users` FIRST and has no choice
  (it needs a user id before it can name a membership). So the reconciler is the
  fixed order and every membership writer in `internal/api` agrees with it:
  `lockUserFirst` (`SELECT … FOR NO KEY UPDATE`) opens each of those transactions,
  the previously-autocommit single statements are wrapped in one, and the two
  cascading deletes (org, project) lock every affected member's row in id order.
  `Reconcile` additionally carries `deletebatch.go`'s bounded 40P01 retry as belt.
  The membership triggers are SPLIT — unguarded on INSERT/DELETE, `WHEN (… IS
  DISTINCT FROM …)` on UPDATE — and `ReconcileOrgMembershipUpsert` carries the same
  guard on its `DO UPDATE`, so an ordinary login (which re-upserts every mapped org
  on every request that reconciles) bumps nothing and evicts nothing. Gates:
  `TestMembershipGrantTakesTheUsersLockFirst` (deterministic: hold `users(U)`, fire
  the PUT, prove the membership row is still lockable by a third transaction) and
  `TestConcurrentLoginAndMembershipGrantDoNotDeadlock`.
- **Never admin, never site admin** (falls out of F3's allowlist): CanWriteProject for a
  PAT requires an explicit project writer/admin role AND `max_scope=write` — the
  org-admin short-circuit does not apply to non-interactive methods. Documented in the
  reveal copy (memo §2.6 text).
- Reveal copy adds the staleness truth: authority narrows live (epoch), revocation is
  instant (probe-checked).

## 4. ROBOTS (`bkro_`)

Memo §3 stands (separate `robots` + `org_tokens` tables in `000016` — NOT users rows;
reconciler safety is structural because no login-path SQL names these tables; principal
carries only {robotID, orgID, scope}; CanRead/CanWrite = org match (+scope); everything
else false; cache plane + `GET /api/v1/me` only; org-admin-managed CRUD under
`/api/v1/orgs/{org}/robots*`; ROBOTS card on the members page below the human roster)
**with the critique's amendments (F5, F11)**:

- **`org_tokens.expires_at NOT NULL`**, API+console max 365d, rotation flow documented.
  A robot is an org-owned identity, not a delegation — it deliberately survives its
  creator (explicit ruling, was silent) — so expiry is the countervailing control.
- **`created_by_email text NOT NULL` snapshot** beside the `created_by` SET NULL FK;
  the ROBOTS card shows created_by/created_at/expires_at/last_used_at (it is the audit
  surface).
- `org_tokens_revoked_after_created` CHECK added (parity with user_tokens).
- **`org_tokens.org_id` is the entire authorization decision** — the hot probe returns
  it and `principal.robotGrants` compares it to the routed org and reads nothing else
  — so it gets a **composite FK**: `robots` carries `UNIQUE (id, org_id)` and
  `org_tokens` carries `FOREIGN KEY (robot_id, org_id) REFERENCES robots (id, org_id)
  ON DELETE CASCADE` in place of the plain `robot_id` FK. Two independent FKs let any
  future writer that supplies `org_id` as a parameter mint a live token granting
  org-wide write on org B backed by a robot in org A — not dangling, simply
  cross-tenant, and nothing would notice. Same leg `api_keys`' composite FK onto
  `project_memberships` provides.
- Gates: memo §3.2's five + `TestOrgTokenOutlivesItsCreator` (documenting) +
  `TestDeletingRobotRevokesEveryToken` + cascade-on-org-delete.

## 5. CLI

Memo §2.7 wholesale: widen `credentials.json` (embedded Token compat; `default_server`;
per-server `user_token` + `keys` map keyed "org" / "org/project"), `bakery login --token`
(headless, validates via /me; bound to **`BAKERY_USER_TOKEN`**, and NOT to `BAKERY_TOKEN`,
which the moon snippet already defines as a project API key — the help names the env
var and `-` (stdin) before the argv form, which leaks into `ps aux` and shell history), `bakery token {create,list,revoke}`,
`bakery org robot {create,list,revoke,delete}`, `--store` defaulting on with reveal copy
naming the file, precedence flag > env > project key > org token > user token > bearer >
ErrNeedsLogin, server resolution --server > BAKERY_SERVER > default_server > localhost.
`Client.authorize` unchanged (empty Key falls back to bearer — verified).

## 6. STICKY NAV (F9 folded — the memo's "residual" is in scope)

- `storage.ts`: `bakery-project` stores `"{org}/{project}"`; `lastProject(org)` returns
  the slug only when the stored org matches; both `setLastProject` call sites updated.
- `(console)/+layout.svelte`: `orgSlug = page.params.org ?? rememberedOrg` (guarded
  against `data.orgs`); `projectSlug = page.params.project ?? (page.params.org ? null :
  lastProject(orgSlug))` — with the namespaced key this cannot leak a project across orgs.
- ConsoleNav: the project dropdown must not open enabled-but-empty on global pages —
  when `projects` is absent (global layout), the switcher renders the remembered slugs
  as links to the remembered scope, not an empty menu.
- Vitest: `/user` remembers acme/fw; `/o/acme/members` renders project none;
  stale org → none; `bakery-project` from org A never renders under org B.
  Playwright walk per memo §4.

## 7. K8S (F6, F8 folded)

- Probes exactly as memo §5.1 (startup=healthz budgeting whole boot; readiness=readyz;
  liveness=healthz — never readyz).
- **`Recreate` + `replicas: 1` mandatory.** `--boot-lock-wait` (default 0s) helps node
  drains and slow-terminating pods ONLY — it **cannot** rescue RollingUpdate (circular
  wait against a healthy holder k8s is waiting on; F6) and the flag's error text at
  budget exhaustion names RollingUpdate as the likely cause. Manifest policy gate in
  `just check`: kubeconform + a script asserting `strategy.type: Recreate` and the
  NetworkPolicy's presence — shipping manifests that violate either fails CI. The
  kubeconform half asserts a **positive valid count and a skip budget of one**, not
  merely `Invalid: 0, Errors: 0`: with `-ignore-missing-schemas` a schema the catalog
  cannot serve is *Skipped*, so a version bump that skips all eight manifests would
  otherwise exit 0 having validated nothing. The schemas are **vendored** under
  `docs/deploy/schemas/` and `-schema-location` points at them, because `check` is
  the recipe that gates every other CI job and it must not need
  `raw.githubusercontent.com` to run.
- **OIDC discovery bounded**: `context.WithTimeout(ctx, 30s)` inside `NewProvider`.
- **NetworkPolicy is the control, Services are discovery (F8):**
  `docs/deploy/k8s/networkpolicy.yaml` — default-deny ingress; 8080 from the ingress
  namespace; 9090 ONLY from monitoring; 9092 only if remote gRPC clients exist; and
  **8080 from an `ipBlock` naming the node CIDR**, because health probes come from
  the KUBELET on the node rather than from a namespace and no selector can admit
  them. Without it, on every CNI that enforces host→pod traffic (Calico, Weave,
  Antrea, kube-router) all three probes are dropped, the startupProbe burns its
  60×5s budget, and the pod CrashLoopBackOffs with a healthy process and an empty
  log — indistinguishable from the boot-lock wedge the same doc explains. `k8s.md`
  names the CNIs; `k8s-check` asserts the probe port is admitted from something
  other than a namespaceSelector.
  `METRICS_ADDR=0.0.0.0:9090` + separate `bakery-metrics` Service remains required and
  invariant-clean (listener separation, not bind interface).
- Manifest set per memo §5.6 + networkpolicy.yaml; `terminationGracePeriodSeconds: 60`;
  `STORAGE_DIR=/data` + PVC; SHA-pinned images — **CI gains `sha-{sha}` (+ release tag)
  image tags first**; `EXPOSE 8080 9090 9092`; `docs/deploy/k8s.md` prose incl. the
  accepted deploy outage, the `boot lock lost` alert line, and BOOT_LOCK_WAIT ×
  startupProbe sizing.

## 8. AVATARS + COPY (F10 folded)

Memo §6 stands (idTokenClaims.Picture → Identity.AvatarURL → UpsertUser narg →
reconciler additively; `Me.avatar_url`; both render sites with `referrerpolicy=
"no-referrer"`, `object-cover`, onerror→monogram; hotlink not proxy; no CSP exists,
none added; copy diffs exactly as written — delete the avatar apology, both hints
become "Managed by {idpLabel}" with idpLabel = issuer host or the generic fallback)
with two amendments:
- **Migration CHECK** `avatar_url IS NULL OR avatar_url LIKE 'https://%'` — the schema
  is the guarantee, the verify()-side https filter is the friendly path (F10). It lands
  in `000014_user_avatar`, ahead of the two credential migrations: the three are
  contiguous as `000014_user_avatar` / `000015_user_tokens` (with the `authz_epoch`
  column and its triggers, which belong with the cache they invalidate) /
  `000016_robots`.
- **`AvatarURL` does NOT join the sealed Principal interface** — `handleMe` reads it
  from the user row it already loads; the authz type stays about authorization (F10).
- Gates: memo §8.2 avatar row + fixture-key updates (tscontract gates the Me change).

## 9. S3 (F1 folded — the central correction)

Memo §7 stands (staging-key mandatory; pipe-streamed manager.Uploader; size-proportional
copy in `Sync`; config surface incl. STORAGE_DRIVER enum + endpoint/path-style for minio;
standard AWS credential chain only; HeadBucket boot probe; DriverS3 metrics label;
conformance-suite extraction prerequisite + minio harness cloned from dbtest/docker.go +
`just storage-conformance` + CI job; proxied reads, no presigned URLs; no hand-rolled
Range — ServeObject's plain-200 fallback is the shipped behavior; lifecycle rule on
staging/ + AbortIncompleteMultipartUpload documented) **except Commit semantics (F1)**:

- `Sync`: upload + server-side copy staging → `objects/{hex}` **and RETAIN the staging
  key**.
- **`Commit` (inside the digest advisory lock): one `HeadObject` on the final key;
  if absent (a concurrent reap won the race between Sync and the lock), re-copy from
  staging.** Constant-time in the common case, size-proportional only in the rare race —
  the starvation rule ("no size-proportional network work under the lock on the common
  path") holds, and the PUT-vs-reap mutual exclusion the lock provides is restored:
  a live metadata row can never name a reaped object. Staging is deleted on successful
  Commit and on Abort; Abort never touches the final key.
- The `storage.go` Writer doc gains the amended contract line (bytes durable at the
  content address strictly before any metadata row names them; Local publishes at
  Commit, S3 publishes at Sync **and re-asserts at Commit under the lock**).
- Gates: shared conformance suite green on both drivers; `TestS3_CommitRepublishesAfterAReap`
  (the F1 interleaving as a test: Sync → reap the digest → Commit → object present,
  GET succeeds); `TestS3_CommitIssuesOnlyConstantTimeCallsWhenObjectPresent` (replaces the
  memo's zero-request gate, which would have enforced the bug) and
  `TestS3_CommitHeadIsBoundedByTheInLockRetryCeiling` (constant time PER ATTEMPT is half
  a bound: the head runs on the tight-retry client, `s3InLockMaxAttempts`, which now names
  BOTH in-lock calls — ReapDigest's DELETE and Commit's HeadObject); orphan-direction test;
  skip-fails recipe.

## 10. Implementation order

Stage 1 (parallel-safe bundle): sticky nav + avatars/copy + k8s (code + CI tags +
manifests + policy gate) + storage conformance-suite extraction.
Stage 2: the auth seam (§2) — gates 3/4/5.
Stage 3: user tokens + robots backends (migrations 000015/000016 + epoch triggers,
validators, APIs).
Stage 4: consoles (/user tokens UI, ROBOTS card) + CLI (§5).
Stage 5: S3 driver + minio gate + CI job.
Stage 6: full gate. Then 3-lens review, fixes, verify, merge.

## 11. Non-goals

Memo §10 verbatim (no multi-replica, no admin capability for any token, no robots-as-
users, no in-place rotation, no avatar proxy/upload, no CSP, one credentials file, no
presigned reads, no hand-rolled Range) plus: no org-admin visibility of personal tokens
this wave (owner feedback item); no batched S3 deletes in GC (recorded follow-up); the
members-page em-dash copy outside /user stays untouched (different meaning).
