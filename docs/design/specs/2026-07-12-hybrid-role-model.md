# Hybrid role model — OIDC-synced groups + in-app grants

**Status:** approved, not implemented. Lands as **M1.5**, before the SPA→API wiring wave.
**Supersedes:** the claim-only authorization model recorded in DESIGN.md "M1 as landed".

## The problem

M1 derives site roles *and* org memberships entirely from OIDC group claims, reconciled on every login.
`org_memberships` deliberately has no `source` column, because a manually granted role would be silently
deleted by the user's next login — so the schema made that state unrepresentable.

That is internally coherent, and it has two consequences we do not want:

1. **A group per org, in LDAP, forever.** Adding a user to an org means editing the group-map file or the
   directory. In a cluster where LDAP is not exposed through an ingress, that is a cluster-admin round-trip
   to perform what should be a self-service act.
2. **A freshly-created org is unusable by its creator.** Creating an org grants no membership in it (there is
   no group mapping yet) → the creator cannot be added as a project member (the `{user}` path segment is
   resolved against the org roster, so it 404s) → `CreateAPIKey` refuses with `scope_exceeds_role`, because
   it requires a real project membership and site admin deliberately does not bypass it. The console's
   headline flow — create org → create project → mint key → copy the config snippet — dead-ends.

The root cause is not the OIDC coupling. It is that **one question was answering three**: "may this human use
Bakery?", "may they run the platform?", and "which orgs are they in?" were all resolved from the same group
lookup, so the only way to answer the first was to answer the third.

## The model — four planes

| Plane | Question | Source of truth |
|---|---|---|
| **Login gate** | May this human use Bakery at all? | `login_groups` (claims). **Empty/unset ⇒ any successful OIDC auth is admitted.** |
| **Site role** | May they run the platform? | **Hybrid** — `site_admin_groups` (claims) **+** local grants. |
| **Org membership** | Which orgs are they in, and how? | **Hybrid** — group map (claims) **+** local grants. Effective role = `greatest(oidc, local)`. |
| **Project role** | What may they do inside a project? | **In-app only.** Unchanged from M1. |

Project roles are deliberately *not* group-mappable. That is precisely the group explosion this design
exists to escape, and the hybrid org plane already gives centrally-managed orgs their on-ramp.

The login gate being independent of org mapping is what unblocks everything else: a user may now be admitted
with **zero** claim-derived orgs and hold only local memberships.

## Schema

### The constraint that dictates the physical shape

`project_memberships` carries a composite FK:

```
FOREIGN KEY (user_id, org_id) REFERENCES org_memberships (user_id, org_id) ON DELETE CASCADE
```

This is load-bearing. It is why deleting one org membership cascades away every project role in that org and
(via 000004) every API key held under those roles — **one DELETE revokes every key the user holds in that
org**. That cascade is what makes a self-contained, join-free API-key grant safe to trust on the sstate HEAD
storm.

It requires `org_memberships` to be UNIQUE on `(user_id, org_id)`.

**Therefore the two sources CANNOT be two rows.** A `PRIMARY KEY (user_id, org_id, source)` would destroy
that uniqueness, destroy the FK, and silently reopen the revocation hole. One row per `(user, org)`; the two
sources are two columns.

### `org_memberships`

```sql
ALTER TABLE org_memberships
    ADD COLUMN oidc_role  org_role,                        -- NULL: no claim justifies this membership
    ADD COLUMN oidc_group text,                            -- which group justified it (audit)
    ADD COLUMN local_role org_role,                        -- NULL: no local grant
    ADD COLUMN granted_by uuid REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN granted_at timestamptz;

-- `role` becomes the EFFECTIVE role, computed by the database. The enum is declared
-- in privilege order ('member' < 'admin' < 'owner'), so greatest() is exactly the
-- max-wins rule and there is no application code that can get it wrong.
ALTER TABLE org_memberships
    ALTER COLUMN role DROP DEFAULT,
    ALTER COLUMN role TYPE org_role,
    ALTER COLUMN role SET GENERATED ALWAYS AS (greatest(oidc_role, local_role)) STORED;

-- A row justified by neither source is garbage; it must not exist, because its
-- existence would keep the project-membership cascade from firing.
ALTER TABLE org_memberships
    ADD CONSTRAINT org_memberships_has_a_source
        CHECK (oidc_role IS NOT NULL OR local_role IS NOT NULL);

-- Provenance is only meaningful for a local grant.
ALTER TABLE org_memberships
    ADD CONSTRAINT org_memberships_local_provenance
        CHECK ((local_role IS NULL) = (granted_at IS NULL));
```

(Postgres cannot convert an existing column to GENERATED in place; the migration adds a new generated column
and drops/renames. Written out properly in the implementation.)

**The reconciler writes `oidc_role`/`oidc_group` and nothing else. It cannot clobber a local grant, by
construction — not by convention.**

### `users.site_role`

Same treatment: `site_role_oidc`, `site_role_local`, `site_granted_by`, `site_granted_at`, and `site_role`
becomes `greatest(...)` generated (`'user' < 'admin'`). A local site-admin grant is disableable by config
(`--allow-local-site-admins`, default on).

## Reconciliation — and the trap under it

On every login, compute the claim-derived set and sync **only the `oidc_*` columns**:

- Claims justify a membership → upsert `oidc_role`, `oidc_group`.
- Claims no longer justify one → set `oidc_role = NULL`; **delete the row iff `local_role IS NULL` too**
  (which fires the project/key cascade, as today).
- `local_role` is never read and never written by the reconciler.

### Fail-closed moves, it does not go away

The M1 rule — *resolved to no orgs ⇒ refuse* — **cannot survive**: a locally-granted user legitimately
resolves to zero orgs, and refusing them is the bug.

It is replaced by a sharper rule:

> **An unreadable groups claim ⇒ refuse the login and reconcile nothing.**

"The IdP says you are in zero groups" and "we could not read your groups" are *categorically different*, and
only the first is safe to act on:

- **Genuinely empty** (`groups: []`) — a fine, ordinary state. It now means "you have only local
  memberships." Admit, and reconcile the OIDC half to empty.
- **Unreadable** — the claim is absent, or Azure AD's `_claim_names`/`_claim_sources` overage has replaced it
  with a pointer to Graph. We do **not know** this user's groups. Reconciling would read as "zero groups",
  NULL every `oidc_role`, delete every row with no local grant, and cascade away their project roles and API
  keys. **Refuse the login. Touch nothing.**

This must hold **whether or not the login gate is enabled** — the gate and the reconciler consume the same
claim, and disabling the gate must not disable the trap detection.

## Effective role and API keys

API-key scope is capped by the **project** role, and the reconciler never touches project roles — it only
touches org membership. So what protects keys here is not new revocation logic; it is the **FK cascade**:

```
org_memberships DELETE → project_memberships CASCADE → api_keys CASCADE
```

Two requirements follow, and they are the whole of it:

1. **The one-row design must preserve that cascade.** It does — `(user_id, org_id)` stays unique, so the
   composite FK survives. This is the entire reason the sources are columns and not rows.
2. **The row is deleted exactly when BOTH sources are gone.** `oidc_role` going NULL while `local_role` is
   set must NOT delete the row: the user still holds a deliberate local grant, so their membership, their
   project roles, and their keys all survive — **by design, not by accident.**

The existing project-role-downgrade-revokes-keys transaction in `internal/api/members.go` is unchanged:
project roles remain single-source and in-app, so there is no "effective" project role to recompute.

The one genuinely new behavior is on the API: `DELETE /orgs/{org}/members/{user}` clears only `local_role`.
If the user is *also* in a mapped LDAP group, the row survives and they remain a member. The response must
say so, or an admin will remove someone, see a success, and reasonably believe they are gone when they are
not.

### Prerequisite bug fix (blocks this work)

`RevokeAPIKeysForMembership` (sqlc, `internal/db`) **has never worked**. Its parameter is
`[]repository.ApiKeyScope` bound to `sqlc.arg(scopes)::api_key_scope[]`, and pgx cannot build an encode plan
for a slice of a custom enum unless the type is registered on the connection. It had no caller, so nothing
proved it; `internal/api/members.go` currently works around it in-transaction.

**Fix:** register the custom enum types on the pool in `pgxpool.Config.AfterConnect` (`conn.LoadType` +
`TypeMap().RegisterType`) — for **all** enums, not just `api_key_scope`. This is a whole class of bug: any
future sqlc query taking an enum *array* fails identically, at encode time, on its first caller. Then restore
`members.go` to the intended single query, and add a test that exercises an enum-array parameter so the class
cannot silently rot again.

## Org creation

Any user who passes the login gate may create an org, gated by `--allow-self-serve-orgs` (default **on**;
off restricts creation to site admins).

**The creator receives a `local` owner grant on the new org.** That is the fix for the dead-end: create org →
own it → add members → mint keys, with no LDAP round-trip and no cluster admin.

## Config

```json
{
  "note": "Empty login_groups admits any successful OIDC auth.",
  "login_groups": ["bakery-users"],
  "site_admin_groups": ["platform-admins"],
  "orgs": [
    { "slug": "acme", "groups": { "acme-engineering": "member", "acme-leads": "admin" } }
  ]
}
```

Flags: `--allow-self-serve-orgs` (default on), `--allow-local-site-admins` (default on). Both env-bindable.

## API and UI surface

- `POST /api/v1/orgs` — creator gets a local owner grant. Gated by the self-serve flag.
- `PUT /api/v1/orgs/{org}/members/{user}` — **now writes `local_role`.** Was refused as claim-derived; is now
  the primary way to add someone to an org. Requires org admin.
- `DELETE /api/v1/orgs/{org}/members/{user}` — clears `local_role`; deletes the row iff `oidc_role IS NULL`.
  Must state in the response when a claim-derived membership survives the removal, or the admin will think
  the user is gone when they are not.
- Membership responses expose **provenance**: `{ role, oidc_role, oidc_group, local_role, granted_by,
  granted_at }`. The console renders source, e.g. `ldap: acme-leads` vs `local: granted by jsmith`.
- Site-admin screen lists every site admin **with their source**. A local grant that survives an LDAP
  revocation must be *visible*, not invisible.

## Invariants (to be added to CLAUDE.md on landing)

- **`org_memberships` is one row per `(user, org)`. Never two.** The composite FK from
  `project_memberships` — and therefore the cascade that revokes every API key when a user leaves an org —
  depends on `(user_id, org_id)` being unique. Splitting the sources into rows silently reopens that hole.
- **The reconciler writes only `oidc_*` columns.** A local grant is not in its keep-set and not in its delete
  predicate. This is structural, not conventional.
- **Effective role is `greatest(oidc_role, local_role)`, computed by the database.** No application code
  recomputes it.
- **An unreadable groups claim refuses the login and reconciles nothing** — regardless of whether the login
  gate is enabled. Empty ≠ unreadable. Conflating them cascade-deletes a real user's entire access.
- **An org membership row is deleted exactly when BOTH sources are gone.** Deleting it while a local grant
  survives revokes keys the user is still entitled to; failing to delete it when neither source justifies it
  suppresses the cascade and leaves keys alive that should be dead.
- **A row with neither `oidc_role` nor `local_role` must not exist.** Its existence would suppress the
  project-membership cascade.

## Testing

- Reconciler: local grants survive a login that drops the OIDC group; the row is deleted only when both
  sources are gone; a genuinely-empty `groups: []` is admitted and reconciles the OIDC half away; an
  **absent** claim and an **overage** (`_claim_names`) claim each refuse the login and mutate nothing (assert
  the row count is unchanged, not merely that an error was returned).
- Effective role: `greatest()` for every (oidc, local) pair including NULLs.
- **Key revocation fires on the reconciler path**: a writer whose LDAP group is downgraded loses their write
  key on next login, in the same transaction. Break the transaction and watch the test go red.
- Cascade preserved: dropping the last org grant still revokes project roles and API keys (this is the
  regression test for the FK the two-row design would have destroyed).
- Org creation grants the creator a working owner role: create → add member → mint key, end to end.
- Self-serve and local-site-admin flags off ⇒ the paths are refused.
- Enum-array parameter encodes (the `RevokeAPIKeysForMembership` regression).

## Out of scope

Group→project mapping (the explosion we are escaping). SCIM. A third identity source (the column-per-source
shape does not scale past two; a third would be a migration to a grants table, and that is a fine trade for
YAGNI today). Wiring the SPA to any of this — that is the follow-on wave.
