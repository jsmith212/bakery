# M6 — GC, retention, quotas

**Status:** approved for implementation, 2026-08-14.
**Provenance:** a 5-seat research wave adjudicated into a design memo, then adversarially
critiqued (20 findings, all folded here); product decisions confirmed by the owner the same
day. The memo and critique live in the session workflow journal (`wf_9bfd49f3-e21`); this
spec is the implementable distillation and **supersedes both where they disagree**.

---

## 1. Product decisions (confirmed 2026-08-14)

1. **Retention ships ON — opinionated defaults.** The upgrade migration seeds real
   `retention_window` values onto existing backends and `CreateBackend` seeds them onto new
   ones (org default overrides, §7). The owner explicitly accepted that the first sweeps
   after upgrade delete old objects. Safety rails: the per-run `grace_period` (24h) between
   metadata delete and byte reclaim, dry-run, and `--gc-disable-retention`.
2. **`downloads` is an ARCHIVE, not a cache.** Its default window is NULL and stays NULL
   even under opinionated defaults; the console presents its quota as advisory-only. A
   premirror tarball whose upstream died is unrecoverable; every other namespace's eviction
   is not. (`TestDownloadsRetainsForeverByDefault`.)
3. **Org-level quota/retention are SEED DEFAULTS for new backends, never enforced
   ceilings.** No cross-project aggregate cap in M6. **OCI gets no quota** — a pull-through
   proxy is bounded by its retention window, matching the mocked console.
4. **Scheduling is a plain interval** (`--gc-interval`), not a maintenance window. The
   `gcWindow` string in the settings mock is descoped until operators ask.
5. Engineering defaults taken without asking: quota warning badge at **90%**; default
   windows in §4.

## 2. Two layers (unchanged from the memo)

- **Layer A — metadata:** namespace-specific sweeps of `cache_objects`,
  `hashserv_unihashes`, `hashserv_outhashes`. Deleting a `cache_objects` row fires the
  refcount trigger (decrement + `unreferenced_since`).
- **Layer B — bytes:** the already-built `MarkBlobsPendingDelete` → `ReapDigest` machinery
  (`internal/db/query/gc.sql`, `blob.Service.ReapDigest`). Global, namespace-blind, runs
  last. CLAUDE.md's "tags → manifests → the blobs table" resolves to: OCI `blobs`
  *namespace* is a Layer-A stage after `manifests`; the `blobs` *table* is Layer B.

Nothing in the existing machinery is re-implemented. `DeleteObjectsChunk` is **forbidden to
GC** (no LRU invalidation — teardown only; it gains a doc comment saying so).

## 3. The sweep, stage by stage

One `gc_runs` row per sweep; `started_at` + `snapshot` frozen once, shared by every stage.
**Every Layer-A stage carries all three predicates:**

```sql
AND created_at < run.started_at
AND pg_visible_in_snapshot(live_xid::text::xid8, run.snapshot::pg_snapshot)
AND <the stage's own retention rule>
```

**The liveness column is ONE rule, verbatim in every stage and in the quota histogram:
`coalesce(accessed_at, created_at) < now() - W_stage`.** (Critique finding 1: a bare
`accessed_at < …` makes every pre-upgrade row — all NULL — immortal, and makes quota
eviction non-terminating. `coalesce` is not the CAS trap: `created_at` is used only when
there is no read record at all, which is genuinely cold.)

| # | Stage | Scope (kind, namespace) | Rule beyond the universal three |
|---|---|---|---|
| 0 | Crash re-drive | global | every `state='pending_delete'` blob → `ReapDigest`, oldest first |
| 1 | hashserv unihashes | `hashserv` | `coalesce(accessed_at, created_at) < now()-W`. THE GC root. |
| 2 | hashserv outhashes | `hashserv` | **space reclamation, not correctness** (finding 9 — the equivalence queries already JOIN unihashes, so an orphan is filtered, not dangling): orphaned rows (`NOT EXISTS` unihash for `(backend_id, method, taskhash)`) past the window are deleted; **additionally**, `outhash_siginfo` is set NULL on any outhash row (orphaned or not) past `W_siginfo = 4·W` (finding 19 — siginfo is ~128 KiB TOAST, read only by RPCs not on any build path). |
| 3 | sstate | `sstate`, ns `''` | window elapsed (coalesce rule) **AND** (unihash underivable **OR** no surviving unihash row on the paired hashserv backend). Derivation in §5. |
| 4 | downloads | `downloads`, ns `''` | coalesce rule; **W is NULL by default → stage skipped** (product decision 2) |
| 5 | ac, ac-grpc, sccache | `bazel`, ns `ac`/`ac-grpc`/`sccache` | `greatest(created_at, coalesce(accessed_at, created_at)) < now()-W_ac` (Overwrite=true refreshes `created_at` on rewrite) |
| 6 | cas | `bazel`, ns `cas` | coalesce rule with `W_cas` (ladder §4); **plus the reachability touch, §6.3** |
| 7 | OCI tags | `oci`, ns `tags` | `updated_at < now()-W_tags` (SWR revalidation already maintains `updated_at`; `accessed_at` not used here) |
| 8 | OCI manifests | `oci`, ns `manifests` | `NOT EXISTS` (tags row **in this backend** with same `digest` — note `cache_objects_digest_idx` is on `digest` alone, so backend scoping is a post-filter; fine at OCI scale) **AND** `coalesce(accessed_at, created_at) < now()-W_manifests` (finding 18: the column is stated, and it is not `created_at` alone — that would be the CAS trap for deduped hot base images) |
| 9 | OCI blobs | `oci`, ns `blobs` | coalesce rule with `W_ociblobs` |
| 10 | Layer B | global | `MarkBlobsPendingDelete` loop → `ReapDigest` loop, existing queries untouched |

**Disabled backends are swept too, and harder** (finding 10): `enabled = false` is a
*stronger* retention signal, not an exemption — effective window is
`least(configured, 30d)`. A disabled backend serving no traffic never touches
`accessed_at`, and skipping it would let its rows pin deduped digests forever. If a
"disabled but preserved" state is ever wanted, it must be a new explicit column.

**Quota-pressure eviction** (§8) runs inside the same staged pass, ignores the retention
window (a cap is a cap), but **exhausts a stage's candidates before touching its
successor** — never pure global LRU (`TestQuotaEvictionExhaustsACBeforeCAS`).

## 4. Windows: the ladder and the shipped defaults

One `retention_window` per `cache_backends` row. Per-namespace windows are **derived in
code — no knob can express an ordering violation** (same posture as
`greatest(oidc_role, local_role)`):

```
W_ac = W_acgrpc = W_sccache = W       W_cas       = 2·W
W_tags = W                            W_manifests = W_ociblobs = 2·W
W_sstate = max(W_sstate_own, W_of_paired_hashserv_backend)
W_siginfo = 4·W_hashserv
```

Shipped defaults (product decision 1 — seeded onto existing rows by the migration and onto
new rows by `CreateBackend`, org `default_retention_window` overriding when set):

| Kind | Default `retention_window` | Effective per-namespace |
|---|---|---|
| sstate | 90d | 90d (ladder vs paired hashserv) |
| hashserv | 90d | unihashes 90d; orphan outhashes 90d; siginfo NULLed at 360d |
| downloads | **NULL — archive** | never age-swept |
| bazel | 30d | ac/ac-grpc/sccache 30d, cas 60d |
| oci | 30d | tags 30d, manifests/blobs 60d |

`NULL` always means "retain forever" and remains settable per backend.

**Upgrade shock is real and priced** (owner accepted): the first sweep after upgrade
deletes everything colder than the window by `created_at` (all `accessed_at` are NULL on
day one). The spec's rails: the 24h blob grace period, `bakery gc run --dry-run` in the
upgrade notes, and `--gc-disable-retention` as the brake.

## 5. The sstate unihash root (derived; `gc_root` stays unbuilt)

1. Basename (strip `universal/hh/hh/` sharding); strip `.siginfo`, `.sig`, **and `.done`**
   (finding 12 — yocto.md names three sidecars, the memo handled two).
2. Match `RE_SSTATE_PKGSPEC`; capture `bb_unihash` (`[^_]*` — safe because the unihash is
   hex and cannot contain the terminating `_`).
3. Validate `^[0-9a-f]{1,64}$` (never `{64}` — Scarthgap emits 40-hex).
4. Probe `hashserv_unihashes` on the **paired** backend (`project_id → kind='hashserv'`,
   resolved once per backend per run), riding `hashserv_unihashes_unihash (backend_id, unihash)`.
5. Unparseable keys are legal (`do_populate_lic` swspec) and are treated as *unreachable*:
   they die on age alone. Rule is conjunctive — dead iff window elapsed AND unreachable.

**The fallback must be observable** (finding 12): most real deployments (rsync'd mirrors,
`BB_HASHSERVE=auto`, no hashserv backend) have ZERO unihash rows, collapsing the policy to
age-only — silently. Emit `bakery_gc_sstate_unihash_coverage{org,project}` (fraction of
scanned keys whose unihash resolved) and **refuse to sweep an sstate backend whose coverage
is 0 while its paired hashserv backend holds > 0 rows** — that combination means the
derivation broke, not that the objects are garbage.

## 6. `accessed_at`

Nullable `timestamptz` on `cache_objects` and `hashserv_unihashes`. **In no index, ever —
not as a key, not in a predicate** (HOT is forfeited by any indexed column the update
touches). Retention scans use a keyset cursor over `cache_objects_pkey` (leading
`backend_id` — a range scan of one backend's slice).

### 6.1 The toucher is the LRU itself (finding 6 — no second map)

The memo's separate 64-shard pending map is **rejected**: it doubles hot-path lock
acquisitions and forces a string allocation on the zero-alloc path. Instead:

- `lruEntry` gains a `markedAt` field, **stamped under the shard mutex the LRU hit already
  holds** — zero extra locks, zero allocations, no new data structure.
- The flusher goroutine walks the 64 shards each tick (F = `--gc-touch-interval`, 1m),
  collects entries with `markedAt` set and older than the SQL staleness guard
  (T = `--gc-touch-staleness`, 1h), clears the flag, and issues batched
  `TouchObjectsAccessed` UPDATEs (guarded `accessed_at IS NULL OR accessed_at < now()-T`,
  the `touchKeysSQL` shape).
- Pending is **bounded by LRU capacity** — `--gc-touch-max-pending` does not exist
  (finding 17 dissolved). An evicted-before-flush entry loses its touch; bounded staleness
  ≤ F against windows of days.
- **Gates:** `TestExists_LRUHitIssuesZeroQueries` and the batch variant pass **unmodified**;
  `BenchmarkExists_LRUHot` gates on **ns/op AND allocs/op**; `TestToucherFlushIsCoalesced`
  (N reads in T ⇒ one UPDATE).
- **Final flush on shutdown** under `context.WithoutCancel` + bounded timeout — the
  `StartKeyToucher` precedent's missing flush is a recorded bug, not a pattern.
- OCI `tags` excluded (reads via `StatUncached`, freshness already on `updated_at`).

`hashserv_unihashes` reads don't traverse the LRU; the hashserv store gets its own small
keyToucher-pattern map (single mutex is fine — hashserv QPS is nowhere near the HEAD storm,
and every get already pays a DB query), same F/T, same final flush.

### 6.2 The delete/touch race (an invariant, not an optimization)

`FindMissingBlobs` answering "present" is a **reservation** served from the LRU with zero DB
contact; a pending (unflushed) touch is invisible to the sweep's SELECT. Three mechanisms
compose, all *sound only because GC refuses to run multi-instance* (§9.4 — this process's
pending state is complete):

1. **Pre-sweep force-flush** (the primary mechanism, post-review): the engine synchronously
   flushes the hashserv toucher immediately before stage 1 and the blob toucher before the
   first `cache_objects` stage, so every mark older than the sweep becomes a real
   `accessed_at` that the `coalesce` predicate then spares. The residual window is a mark
   arriving *during* a stage's own execution — bounded by stage duration (seconds–minutes),
   versus the up-to-`T` hold it replaces.
2. **The per-chunk veto**: before deleting, the sweep intersects each candidate chunk
   against the live pending state (LRU `markedAt` probe + the §6.3 aux map) and drops any
   key present — this catches marks made mid-sweep on the blob side.
3. **The delete itself carries both write-barrier halves** (`DeleteObjectsByKeys` joins the
   run row): a row overwritten or re-created between scan and delete (an `/ac` overwrite
   refreshes `created_at`/`live_xid`) no longer matches. The delete transaction also takes
   the affected blob digests' row locks explicitly, in digest order
   (`LockBlobDigests ... ORDER BY digest FOR NO KEY UPDATE`), so the refcount trigger's
   lock order is deterministic by construction rather than by planner accident.

### 6.3 The ac-grpc reachability TOUCH (critique finding 2 — replaces "ladder is enough")

A reachability **sweep** from `ac-grpc` stays rejected (mark-sweep with a live mutator: CAS
writes land before `UpdateActionResult` commits). A reachability **touch** has none of that
risk — it only makes rows younger; the worst case of any race is a missed touch, never a
wrong delete. On `GetActionResult` (a read of an already-committed, already-unmarshaled
`ActionResult`), feed every referenced output digest into a small sharded aux pending map
(GetActionResult is one RPC per action — not the HEAD storm; an insert there is fine),
flushed by the same flusher. This closes the `--remote_download_minimal` divergence
*exactly*: an AC hit now touches its outputs whether or not Bazel fetches them.

The touch also descends one level: for each `OutputDirectory`, the (locally present, hot)
`Tree` blob is read and every `FileNode` digest in `root`+`children` is marked — under
BwoB, Bazel `injectRemoteFile`s tree *contents* from the Tree's metadata with zero CAS
contact, so without this the tree blob stays hot while its contents age out. Best-effort
(any error or an oversized tree ⇒ skip; a missed touch is the safe direction).

The ladder (`W_cas = 2·W_ac`) stays as defense in depth — but the touch is **load-bearing,
not optional**, per source verification (2026-08-14, Bazel 6.4/6.5/7.0/7.4/8.0 +
moon master): `lookupCache` issues only `GetActionResult`; under
`--remote_download_minimal` the skip-download decision (`RemoteOutputChecker.
shouldDownloadOutput`) is purely local and skipped outputs are `injectRemoteFile`d from
`ActionResult` metadata with **zero CAS contact**. The only Bazel mechanism that ever
probes AC-hit outputs is `--experimental_remote_cache_lease_extension` (Bazel 7+, default
**false**, current-build Skyframe graph only), whose own comment assumes the server treats
`FindMissingBlobs` as a lease-extending touch — exactly our `accessed_at` semantics.
Default-config Bazel instead trusts the server's TTL contract
(`--experimental_remote_cache_ttl`, default 3h — far under our windows) and discovers an
evicted blob only lazily, as the `LostInputsEvent` rewind. Without §6.3's touch, "hot AC,
cold CAS" is therefore the *normal* BwoB steady state, not an edge case.
Opaque-`/ac` clients (ccache, sccache, moon-http, bazel-http) need no equivalent: none has
a download-minimal mode — moon's `hydrate_manifest` unconditionally reads every output blob
on every hit — they fetch what they hit, and fetches touch.

Gating tests: `TestCASOutlivesItsActionCacheEntry`,
`TestWindowLadderIgnoresAnInvertedConfig`, `TestACHitTouchesItsOutputs` (new — AC hit under
zero output reads keeps outputs alive past `W_cas`).

### 6.4 Fillfactor and the first-touch transition (finding 8)

`SET (fillfactor = 85)` on both tables is catalog-only and **does not move existing
tuples**: the corpus sits at ~95–100% fill, so the FIRST touch of every pre-existing row is
non-HOT (new tuple + entries in both indexes). That one-time index-bloat/WAL spike is
managed, not ignored: the toucher **ramps** — `T` starts at 24h until
`gc_state.touch_ramp_until` (a one-row table the migration stamps at `now() + 7 days`;
chosen over an accessed-at-NULL-fraction proxy, which never converges on a mostly-cold
corpus and was last-backend-wins) — and the migration raises this table's autovacuum
aggressiveness (`autovacuum_vacuum_scale_factor = 0.02`). `pg_repack` is documented as the
optional operator fast-path. Operator validation note: confirm HOT absorption on a loaded
instance (`pg_stat_user_tables.n_tup_hot_upd / n_tup_upd`) before tightening autovacuum
further.

## 7. Migration `000012_gc_retention_quotas`

As in the memo §6 (accessed_at columns, fillfactor, `cache_backends.retention_window` +
`quota_bytes` real columns with CHECKs, org seed defaults, `cache_backend_usage` table,
`gc_runs` `trigger`/`dry_run`/`hashserv_rows_deleted`), with these deltas:

1. **Opinionated defaults:** `UPDATE cache_backends SET retention_window = <per-kind
   default from §4> WHERE retention_window IS NULL AND kind <> 'downloads'`.
2. **Dry runs don't hold the active slot** (finding 14): replace
   `gc_runs_single_active_idx` with `... WHERE status = 'running' AND NOT dry_run` (cheap:
   zero production rows exist). Two dry runs may overlap; neither writes.
3. **`FinishGCRun` gains `AND status = 'running'`** (finding 4): a stale finisher can never
   move a run out of a terminal state, so the boot reaper and the shutdown finisher are
   independent instead of mutually corrupting.
4. **Quota is refused on hashserv backends** (finding 15): `CHECK (kind <> 'hashserv' OR
   quota_bytes IS NULL)` — hashserv has no `cache_objects` rows; the quota would be
   structurally unenforceable and read 0 forever.
5. All `cache_objects`/`hashserv_unihashes` ALTERs run with `lock_timeout = 5s` + retry
   (finding 20). The boot-lock argument (migrate-before-serve, single instance) is stated
   inline in the migration comment — and does NOT hold under `--allow-multi-instance`,
   which is one more reason §9.4 refuses GC there.

New sqlc queries: as memo §6 (`ListBackendsForGC`, `ScanObjectsForGC`,
`DeleteObjectsByKeys`, `TouchObjectsAccessed`, `TouchUnihashesAccessed`, `SweepUnihashes`,
`SweepOrphanedOuthashes`, `NullOrphanSiginfo` (new, finding 9/19), `UnihashesExistBatch`,
`SweepUnreferencedManifests`, `UpsertBackendUsage`, `MarkOrphanedGCRunsFailed`, dry-run
mirrors) — **plus `InstancePhysicalBytes`** (`SUM(blobs.size_bytes) WHERE state='live'`,
finding 15).

## 8. Quotas

- **Evict-to-quota + advisory badge. Hard-reject is a non-goal** (client PUT-failure
  latches unverified; enforcement point recorded: `blob.Service.put` after `w.Digest()`).
- **Attribution: logical bytes, full charge to every namer, scoped per backend row.**
  Order-independent, locally computable — the same property the refcount trigger has.
- **Accounting rides the sweep's scan** (no trigger counter, no hot row): one cursor pass
  accumulates count/bytes; when over quota, a histogram of `coalesce(accessed_at,
  created_at)` (finding 1) yields the eviction cutoff, driving to 90% of quota.
- **Quota is a logical cap and says so** (finding 15): eviction frees logical bytes; disk
  falls only after grace + reap, and under cross-project dedup possibly not at all. The
  console copy and docs state it; `bakery_storage_physical_bytes` (instance-level gauge,
  from `InstancePhysicalBytes`) is the number an operator alerts on for disk.
- **Usage measurement is decoupled from GC being enabled** (findings 7b, 13): a lightweight
  usage pass on `--gc-usage-interval` (default 6h) runs even with retention disabled,
  skipping backends measured within the interval when a full sweep already updated them.
  The storage gauge vecs are `Reset()` then re-`Set()` each pass (vanished backends leave
  the scrape) and `bakery_gc_usage_measured_timestamp_seconds` makes staleness detectable.
- **Inert config costs nothing** (finding 7a): a backend with NULL window and NULL quota is
  skipped by the retention scan entirely (under opinionated defaults this is `downloads`
  and operator-opted-out backends). Sweep pacing defaults: `--gc-interval` **6h** (not 1h —
  at batch 1000/pause 100ms a 10⁷-row slice is ~17min of pause alone), `--gc-batch-size`
  1000, `--gc-batch-pause` 100ms.

## 9. Runtime

1. **In-process loop in `bakery serve`** + API trigger. `bakery gc …` CLI verbs are HTTP
   clients (the `sstate push` shape), never DB-direct.
2. **`blob.Service.DeleteBatch(ctx, refs)`** — the only deletion path GC uses.
   - The batch DELETE is **digest-ordered** (finding 3): the refcount trigger's UPDATE
     branch takes paired blob locks in digest order precisely to kill an ABBA; a key-ordered
     batch delete would reintroduce it a thousand digests wide. `DeleteObjectsByKeys` orders
     its row set by `digest`, and the caller retries bounded on `40P01` (a concurrent `/ac`
     overwrite still takes two locks on its own schedule). Regression test: two concurrent
     overlapping batch deletes, no deadlock.
   - **LRU invalidation is `delBatch`, shard-grouped** (finding 16): keys grouped by shard,
     each shard's mutex taken once, `gen` bumped **once per shard per chunk** — not once per
     key, which would suppress concurrent cold-HEAD fills process-wide for the chunk's
     duration. `lru.del`-not-negative-fill divergence documented in the method comment.
3. **Boot:** `MarkOrphanedGCRunsFailed` runs **iff this process actually holds the boot
   advisory lock** (finding 4 — gate on the lock being held, not on the flag): under
   `--allow-multi-instance` a booting instance must not mark another instance's live sweep
   failed. Then `RedrivePendingDelete` as a background goroutine beside the existing two.
4. **Multi-instance:** GC loop refuses to start under `--allow-multi-instance` (loud boot
   warning; API trigger 409). Three reasons, all load-bearing: process-local LRU
   invalidation, the boot reaper's soundness, and §6.2's pending-set completeness.
5. **Shutdown:** finish the chunk, final toucher flush, `FinishGCRun(failed, "shutdown")`
   under `WithoutCancel` + timeout; the (now-guarded) boot reaper covers the lost-write
   case.
6. **`--gc-disable-retention` halts Layer B's MARK too** (finding 11): the incident it
   serves is "we deleted things we wanted", when the bytes still sit in the 24h grace
   window — leaving the mark running converts a recoverable window into permanent loss at
   maximum speed. Stage 0's re-drive of already-`pending_delete` blobs continues (those are
   past recovery; stalling them reclaims nothing). `--gc-grace-period` is the recovery
   lever: frozen per-run, so raising it takes effect next run.
7. **Dry run:** genuinely read-only mirror queries; writes a `gc_runs` row with
   `dry_run = true`; does not occupy the active slot (§7.2).
8. **Config:** `--gc-enabled` (default true), `--gc-interval` 6h, `--gc-usage-interval` 6h,
   `--gc-grace-period` 24h, `--gc-batch-size` 1000, `--gc-batch-pause` 100ms,
   `--gc-disable-retention` false, `--gc-touch-interval` 1m, `--gc-touch-staleness` 1h
   (ramped, §6.4). No `--gc-touch-max-pending` (§6.1).
9. **Metrics** (`internal/metrics/gc.go`, oci.go shape): `bakery_gc_runs_total{status,trigger}`,
   `bakery_gc_run_duration_seconds`, `bakery_gc_last_run_timestamp_seconds`,
   `bakery_gc_objects_deleted_total{backend,namespace,reason}` (reason ∈
   retention|quota|unreachable), `bakery_gc_blobs_marked_total`, `bakery_gc_blobs_deleted_total`,
   `bakery_gc_bytes_reclaimed_total`, `bakery_gc_pending_delete_backlog`,
   `bakery_gc_touch_flush_rows_total`, `bakery_gc_sstate_unihash_coverage{org,project}`,
   `bakery_gc_usage_measured_timestamp_seconds`, `bakery_storage_physical_bytes`, and the
   first writers to `bakery_storage_objects`/`bakery_storage_bytes` +
   `bakery_gc_quota_ratio{org,project,backend}`.
10. **API/CLI:** `GET /api/v1/gc/runs[, /{id}]`, `POST /api/v1/gc/run` (202/409), all
    `AccessSiteAdmin` (session-only — API-key principals structurally excluded);
    `bakery gc run [--dry-run] [--wait]`, `bakery gc list`.

## 10. Gating tests (the ones that earn their keep)

Per-stage write-barrier replicas (`TestSweepUnihashesSparesAConcurrentBuild`,
`TestSweepObjectsSparesAConcurrentBuild` — each with the vacuity guard asserting the
timestamp predicate alone WOULD have selected the row) ·
`TestCASOutlivesItsActionCacheEntry` · `TestACHitTouchesItsOutputs` ·
`TestWindowLadderIgnoresAnInvertedConfig` · unmodified
`TestExists_LRUHitIssuesZeroQueries` (+ batch) · `BenchmarkExists_LRUHot` on ns/op AND
allocs/op · `TestToucherFlushIsCoalesced` · `TestPendingTouchVetoesTheSweep` (§6.2) ·
`TestDeleteBatchInvalidatesTheLRU` · concurrent-overlapping-DeleteBatch no-deadlock ·
`TestDownloadsRetainsForeverByDefault` · `TestDisabledBackendIsStillSwept` ·
`TestBootMarksOrphanedRunsFailed` (and its multi-instance no-op twin) ·
`TestShutdownMidSweepFinishesTheRun` · `TestFinishGCRunCannotResurrectATerminalRun` ·
`TestDryRunDoesNotHoldTheActiveSlot` · `TestQuotaEvictionExhaustsACBeforeCAS` ·
`TestUsageIsLogicalNotPhysicalUnderDedup` · `TestMultiArchChildManifestSurvivesItsIndexWindow`
· `TestSweepRespectsBatchSizeAndPause` · `TestSstateZeroCoverageRefusesToSweep` ·
`TestNullAccessedAtRowsAreSweepable` (finding 1's regression).

## 11. Non-goals (unchanged from the memo §9 unless noted)

Hard-reject quotas · parsing `/ac` values (permanent contract) · ac-grpc→CAS reachability
**sweep** (the *touch* in §6.3 is in scope and is not this) · OCI manifest parsing ·
`gc_root` column · upstream hashserv GC RPCs · per-namespace configurable windows ·
scoped `gc_runs` · any `accessed_at` index · trigger-maintained usage counters · GC under
`--allow-multi-instance` · object pinning · maintenance-window scheduler · S3 in the reap
path · **all SPA wiring, including the GC-runs screen and the storage-gauge console views**
— the API and metrics land in M6; the screens land with the SPA→API wiring wave, whose
milestone this explicitly is (the console is still fully mock-data everywhere; wiring one
screen now would be an inconsistency, not a feature).
