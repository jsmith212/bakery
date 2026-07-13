# M1.5 — Hybrid role model: implementation plan

> Executed as a multi-agent workflow (ultracode), like M1. Steps are the contract each agent works to.

**Goal:** Split the single OIDC group lookup that currently answers three questions into four independent
planes, so org membership can be granted in-app without an LDAP round-trip, and a freshly-created org is
usable by its creator.

**Spec:** `docs/design/specs/2026-07-12-hybrid-role-model.md`. Read it first — it carries the *why*, and the
`why` is where the traps are.

**Architecture:** One row per `(user, org)` with independent `oidc_role` / `local_role` columns and a
database-generated effective `role = greatest(oidc_role, local_role)`. The reconciler writes only the
`oidc_*` half and therefore *cannot* clobber a local grant. Same treatment for `users.site_role`.

## Global constraints

Every task inherits these. They are not negotiable and they are not restated per task.

- Go stdlib-first. `net/http` ServeMux. `log/slog`. **pgx/v5 + pgxpool — never `database/sql`.**
- sqlc → `internal/db/repository/` (generated, gitignored). golang-migrate, embedded, applied at boot.
- Tests: table-driven, colocated, same-package, **stdlib only**. No testify, no gomock. Hand-written fakes in
  `mocks_test.go`.
- Green = `just generate && go build ./... && go vet ./... && gofmt -l . && go tool golangci-lint run &&
  just test-db` (which **fails on a SKIP**) `&& go test -race ./...`.
- Conventional commits, one per task.
- **Do not weaken an existing invariant to make a task easier.** If a task appears to require it, stop and
  say so — that is a design bug, not an implementation detail.

---

## Task 1 — Register the enum types on the pool (prerequisite)

**Why first:** `RevokeAPIKeysForMembership` has never worked, and every later task that touches an enum array
hits the same wall. This is a whole *class* of bug, not one query.

**Files:** modify `internal/db/db.go` (`NewPool`), `internal/db/db_test.go`; modify
`internal/api/members.go` (restore the single query).

- [ ] **Write the failing test.** In `internal/db`, against a real ephemeral Postgres, call a query that
      takes an `api_key_scope[]` parameter. Assert it succeeds. It currently fails at encode time with
      `cannot find encode plan` for the unknown OID.
- [ ] **Run it. Confirm it fails with that exact error** — not some other error. If it fails for a different
      reason, you are testing the wrong thing.
- [ ] **Implement.** In `NewPool`, set `poolCfg.AfterConnect` to load and register **every** custom enum:
      `site_role`, `org_role`, `project_role`, `api_key_scope`, `backend_kind`, `blob_state`,
      `gc_run_status`. Use `conn.LoadType(ctx, name)` then `conn.TypeMap().RegisterType(t)`, and register the
      array type (`_name`) too — the array OID is the one that was missing. Fail the connection if a type is
      absent: a silently-unregistered type reintroduces the bug.
- [ ] **Run the test. It passes.**
- [ ] **Restore `internal/api/members.go`** to call `RevokeAPIKeysForMembership` directly instead of the
      list-and-loop workaround. **Keep the workaround's test.** It is the one that proves a demoted writer
      loses their write key, and it must still pass against the real query.
- [ ] **Delete the long explanatory comment** in `members.go` about why the query is unused — it is now a lie.
- [ ] Green + commit: `fix(db): register the enum types on the pool so enum arrays encode`

---

## Task 2 — Migration 000008: hybrid org membership and site role

**Files:** create `internal/db/migrations/000008_hybrid_roles.up.sql` and `.down.sql`; test in
`internal/db/db_test.go`.

**Interfaces produced:** `org_memberships.{oidc_role, oidc_group, local_role, granted_by, granted_at}`,
generated `role`; `users.{site_role_oidc, site_role_local, site_granted_by, site_granted_at}`, generated
`site_role`.

**The constraint that dictates everything:** `project_memberships` has
`FOREIGN KEY (user_id, org_id) REFERENCES org_memberships (user_id, org_id) ON DELETE CASCADE`. That cascade
is what revokes every API key when a user leaves an org. It needs `(user_id, org_id)` UNIQUE. **The PK does
not change. The sources are columns, never rows.** A migration that splits them into rows silently reopens
the revocation hole and no test you are likely to write will catch it.

- [ ] **Write the failing tests first**, against a real ephemeral Postgres:
      - effective `role` == `greatest(oidc_role, local_role)` for every pair, including NULLs
      - a row with **both** roles NULL is rejected by a CHECK
      - `granted_at` is present iff `local_role` is present
      - **the cascade still fires**: delete an `org_memberships` row → the user's `project_memberships` in
        that org and their `api_keys` under them are gone. This is the regression test for the FK.
- [ ] **Run them. They fail** (columns do not exist).
- [ ] **Write the migration.** Postgres cannot convert an existing column to `GENERATED`, so: add the source
      columns, backfill `oidc_role := role` (every existing membership is claim-derived today, which is
      exactly true), drop `role`, add it back as
      `role org_role GENERATED ALWAYS AS (greatest(oidc_role, local_role)) STORED`, then re-add the
      `project_memberships` FK if dropping the column disturbed it. Add:
      `CHECK (oidc_role IS NOT NULL OR local_role IS NOT NULL)` and
      `CHECK ((local_role IS NULL) = (granted_at IS NULL))`.
      `greatest()` is the max-wins rule and it works because the enums are declared in privilege order
      (`member < admin < owner`, `user < admin`) — **verify that ordering holds before relying on it.**
      Same treatment on `users.site_role`.
- [ ] **Run the tests. They pass.**
- [ ] **Apply up → down → up** against a real Postgres and confirm the down actually reverses.
- [ ] Green + commit: `feat(db): make org membership and site role hybrid (oidc + local)`

---

## Task 3 — Group map: login gate, optional orgs, unreadable ≠ empty

**Files:** modify `internal/config/groupmap.go`, `internal/config/groupmap_test.go`.

**Interfaces produced:** `GroupMap.LoginGroups []string`; `ErrGroupsUnreadable`; `Resolve` no longer errors
on an empty org set.

**Three behavior changes, each of which breaks an existing test on purpose:**

1. `validate()` currently rejects `len(g.Orgs) == 0` ("no user could ever log in"). **That is no longer
   true** — a deployment can now run entirely on local grants. Remove that rule.
2. Add top-level `login_groups`. **Empty/unset admits any successful OIDC auth.**
3. `Resolve` must stop conflating *"the IdP says zero groups"* with *"we could not read the groups"*. The
   first is now an ordinary, admissible state meaning "local memberships only". The second must refuse.

- [ ] **Write the failing tests:** empty `orgs` parses; `login_groups` gates admission; a user in no login
      group is refused; empty `login_groups` admits anyone; `groups: []` resolves to zero orgs **without
      error**; an *unreadable* claim returns `ErrGroupsUnreadable`.
- [ ] Run them. They fail.
- [ ] Implement. Keep `DisallowUnknownFields` — a typo in an authorization file must not parse.
- [ ] Run. They pass. **Existing tests that asserted "no orgs ⇒ error" must be updated, not deleted** — invert
      them to assert the new behavior, so the change is recorded rather than erased.
- [ ] Green + commit: `feat(config): add the login gate and let the group map be org-free`

---

## Task 4 — Detect the claim overage at the OIDC boundary

**Files:** modify wherever `auth.Identity` is built from the ID token (`internal/auth/oidc.go` or
`identity.go` — find it); test alongside.

**This is the trap the whole design fails-closed around.** Azure AD replaces a large `groups` claim with
`_claim_names` / `_claim_sources` pointing at Graph. The `groups` claim is then simply **absent** — which is
indistinguishable from "this user is in no groups" unless you look for the overage explicitly.

**Interfaces produced:** `Identity.GroupsPresent bool` (or `Groups *[]string` — pick one and be consistent;
a plain `[]string` cannot express the difference and is the bug).

- [ ] **Write the failing test:** an ID token with `_claim_names: {"groups": "src1"}` and no `groups` claim
      must produce `GroupsPresent == false`. A token with `groups: []` must produce
      `GroupsPresent == true, Groups == []`. These two must not be confusable.
- [ ] Run. Fails.
- [ ] Implement.
- [ ] Run. Passes.
- [ ] Green + commit: `feat(auth): tell an unreadable groups claim apart from an empty one`

---

## Task 5 — Rewrite the reconciler

**Files:** modify `internal/auth/reconcile.go`, `internal/db/query/*.sql`, `internal/auth/reconcile_test.go`.

**Interfaces consumed:** Task 2's columns, Task 3's `ErrGroupsUnreadable` + `LoginGroups`, Task 4's
`GroupsPresent`.

**The algorithm:**

1. `GroupsPresent == false` → **refuse the login and reconcile NOTHING.** Return `ErrLoginNotAllowed`. Not a
   single write.
2. Login gate: `login_groups` non-empty and no intersection → refuse.
3. Upsert user, writing **`site_role_oidc` only**.
4. For each claim-mapped org: upsert **`oidc_role` + `oidc_group` only**.
5. For orgs the claims no longer justify: **`SET oidc_role = NULL, oidc_group = NULL`**, then
   **`DELETE` the row iff `local_role IS NULL`.** Two statements, or one with a `RETURNING`-guarded delete —
   but never a blind delete.
6. **Delete the `len(keep) == 0` guard.** It was the fail-closed backstop when zero orgs meant "refuse"; now
   zero orgs is legitimate (local-only user) and that guard would refuse them. Its job has moved to step 1 —
   which is the *correct* place, because it fires on "we don't know" rather than on "you have none."

- [ ] **Write the failing tests. These are the ones that matter most in the whole milestone:**
      - a local grant **survives** a login where the LDAP group is gone (assert `local_role` intact, row
        present, project roles and API keys still alive)
      - the row is deleted **only** when both sources are gone — and when it is, the cascade revokes the
        user's project roles and API keys in that org
      - `groups: []` is **admitted** and reconciles the OIDC half away
      - an **unreadable** claim refuses the login and **mutates nothing** — assert the row counts and the
        actual role values are unchanged, not merely that an error came back. A test that only checks the
        error would pass against code that wiped the table first.
      - the reconciler never writes `local_role`, `project_memberships`, or `granted_by`
- [ ] Run. They fail.
- [ ] Implement the sqlc queries and the reconciler.
- [ ] Run. They pass.
- [ ] **Mutation-check the two load-bearing ones:** make the delete unconditional (drop the
      `local_role IS NULL` guard) and confirm the "local grant survives" test goes RED. Make the unreadable
      claim fall through to reconciliation and confirm the "mutates nothing" test goes RED. **Restore the
      code.** Report what you saw.
- [ ] Green + commit: `feat(auth): reconcile only the OIDC half and fail closed on an unreadable claim`

---

## Task 6 — Local org grants through the API

**Files:** modify `internal/api/members.go`, `internal/api/orgs.go` (responses), `internal/db/query/*.sql`;
tests alongside.

- [ ] **Write the failing tests:**
      - `PUT /api/v1/orgs/{org}/members/{user}` writes `local_role`, `granted_by`, `granted_at` — and is no
        longer refused as claim-derived
      - `DELETE` clears `local_role`; if `oidc_role` is set the row **survives** and **the response says so**
        (the admin must not be told the user is gone when they are not)
      - `DELETE` on a purely claim-derived membership is a **no-op the response is honest about** — LDAP owns
        that membership and the API cannot remove it
      - membership responses expose provenance: `role, oidc_role, oidc_group, local_role, granted_by,
        granted_at`
      - the authz matrix still holds: only an org admin may grant
- [ ] Run. Fail. Implement. Run. Pass.
- [ ] Green + commit: `feat(api): grant and revoke org membership in-app`

---

## Task 7 — Org creation grants the creator a local owner role

**Files:** modify `internal/api/orgs.go`, `internal/config` (add `--allow-self-serve-orgs`, default **on**,
env-bindable); tests alongside.

**This is the fix for the dead-end that started all of this.**

- [ ] **Write the failing test — the whole flow, end to end:** a user with no org memberships at all creates
      an org, then adds a project, then mints an API key. Today this fails at the third step with
      `scope_exceeds_role`. It must pass.
- [ ] Also: creator gets `local_role = 'owner'` with provenance; with `--allow-self-serve-orgs=false` a
      non-site-admin is refused; the org creation and the owner grant are **one transaction** (a crash between
      them would leave exactly the orphaned, unusable org this milestone exists to abolish).
- [ ] Run. Fail. Implement. Run. Pass.
- [ ] Green + commit: `feat(api): give an org's creator a local owner role`

---

## Task 8 — Hybrid site admin, its flag, and the break-glass

**Files:** modify `internal/api` (site-admin grant/revoke + listing), `internal/config`
(`--allow-local-site-admins`, default on), `internal/cli` (+ `internal/config` command tree).

**Bootstrap problem:** with `login_groups` empty and no `site_admin_groups`, a fresh deployment has no site
admin and no way to make one. So there must be an out-of-band path that no HTTP request can reach.

- [ ] **Write the failing tests:**
      - a site admin can grant another user a **local** site-admin role; provenance recorded
      - `--allow-local-site-admins=false` refuses that path entirely
      - the site-admin listing reports **source** for each: `ldap: platform-admins` vs
        `local: granted by <user> on <date>`. A local grant that outlives an LDAP revocation must be
        **visible**, or it is a backdoor.
      - an API-key principal can **never** grant a site role (a delegation must not become a master key)
- [ ] `bakery user site-admin <email>` CLI subcommand: writes straight to the DB, requires `DB_URL`, has **no
      HTTP or API path** — mirroring how `DEV_LOGIN_ENABLED` is env-only. Reaching it requires infrastructure
      access, not a session.
- [ ] Run. Fail. Implement. Run. Pass.
- [ ] Green + commit: `feat(auth): allow local site-admin grants, with provenance and a CLI break-glass`

---

## Task 9 — Docs, config template, invariants

**Files:** `CLAUDE.md`, `docs/design/DESIGN.md`, `stack.env.tmpl`, `docker-compose.yaml`, `README.md`.

- [ ] Add the six invariants from the spec's "Invariants" section to CLAUDE.md **in the same voice as its
      neighbours** — each states the trap, not just the rule.
- [ ] **Correct the invariants M1.5 falsifies.** CLAUDE.md currently says login "fails closed" on *no mapped
      orgs*, and the 000002 migration comment says a `source` column is deliberately absent. Both are now
      wrong. **Update them; do not leave them to rot.** A stale invariant is worse than no invariant — the
      next session will trust it.
- [ ] Mark M1.5 done in DESIGN.md. Document `login_groups`, `--allow-self-serve-orgs`,
      `--allow-local-site-admins` in `stack.env.tmpl` and compose.
- [ ] Green + commit: `docs: record the hybrid role model and correct the invariants it falsifies`

---

## Final gate

- [ ] Full green, including `just test-db` reporting **no skipped tests**, and `go test -race ./...`.
- [ ] Fresh clone builds. Docker image builds.
- [ ] **End-to-end, for real:** boot with dev-login → create org → create project → **mint a key** (the flow
      that was broken) → confirm it works. Then: a user in an LDAP group *and* holding a local grant keeps
      their membership when the group disappears. Paste the real transcript.
- [ ] Report honestly: what is red, what skipped, what you could not verify.
