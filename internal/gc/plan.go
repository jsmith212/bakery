package gc

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// The cache_objects namespaces, spelled once. They are the discriminator half of
// the primary key, not a display string: "" really is sstate's and downloads'
// namespace, which is why the scan cursor compares the PAIR (namespace, key) and
// never key alone.
const (
	nsDefault   = "" // sstate and downloads
	nsAC        = "ac"
	nsACGRPC    = "ac-grpc"
	nsSccache   = "sccache"
	nsCAS       = "cas"
	nsTags      = "tags"
	nsManifests = "manifests"
	nsBlobs     = "blobs"
)

// rule is a stage's own retention predicate, evaluated in Go over a scanned row.
//
// The two UNIVERSAL predicates -- created_at < run.started_at and
// pg_visible_in_snapshot -- are NOT here and cannot be: they are applied by
// ScanObjectsForGC in SQL, so a row that reaches a rule has already survived the
// write barrier. What is left is the third predicate, the one that needs Go
// (cross-table unihash lookups, a tag/manifest anti-join, a quota histogram).
type rule func(row repository.ScanObjectsForGCRow) bool

// stage is one namespace pass over one backend, in the order spec §3's table
// gives. Order is the whole point: the named must outlive the namer, and quota
// eviction exhausts a stage's candidates before touching its successor.
type stage struct {
	namespace string

	// window is the stage's effective W, already laddered. zero means the stage has
	// no age rule at all and runs only for accounting and quota.
	window time.Duration

	// reason is what a deletion from this stage is attributed to in
	// bakery_gc_objects_deleted_total.
	reason metrics.GCReason

	// kindLabel is the blob.Ref metrics sub-kind for this namespace. It never reaches
	// a series the sweep writes (DeleteBatch takes no measurements), but a Ref with a
	// blank kind would be a lie waiting for the first caller that does.
	kindLabel string
}

// backendPlan is one backend's whole sweep: its effective windows, its quota, and
// the paired hashserv backend the sstate root derivation probes.
type backendPlan struct {
	id      int64
	kind    repository.BackendKind
	backend metrics.Backend
	org     string
	project string
	enabled bool

	// window is the backend's effective BASE window after the disabled clamp. false
	// means "retain forever" -- every age rule is off and the backend is swept only
	// under quota pressure.
	window    time.Duration
	hasWindow bool

	// quota is quota_bytes; zero means no cap.
	quota int64

	// hashserv is the paired hashserv backend in the same project (sstate only).
	// Zero means the project has none, which is an ordinary deployment
	// (BB_HASHSERVE=auto, an rsync'd mirror) and NOT an error -- see sstate.go.
	hashserv int64

	stages []stage
}

// managed reports whether the retention scan has any reason to touch this backend
// (spec §8, finding 7a): a backend with no window and no quota is inert
// configuration, and inert configuration costs nothing. Under the opinionated
// defaults this is `downloads` and any backend an operator has deliberately opted
// out of.
func (p backendPlan) managed() bool { return p.hasWindow || p.quota > 0 }

// buildPlans turns the raw backend list into the sweep's plan, applying the window
// ladder (spec §4) and the disabled clamp (spec §3, finding 10).
//
// THE LADDER IS COMPUTED IN CODE, NEVER CONFIGURED. There is one retention_window
// per backend row and every per-namespace window is derived from it, so no knob can
// express an ordering violation -- the same posture as greatest(oidc_role,
// local_role) being computed by the database. An operator who sets a bazel backend
// to 30 days cannot produce W_cas < W_ac, because W_cas is not a number they can
// write.
func buildPlans(rows []repository.ListBackendsForGCRow) []backendPlan {
	// The paired hashserv backend is resolved ONCE per project per run rather than
	// per sstate key: it is the same lookup for every key in the backend.
	hashservByProject := map[pgtype.UUID]int64{}

	for _, r := range rows {
		if r.Kind == repository.BackendKindHashserv {
			hashservByProject[r.ProjectID] = r.ID
		}
	}

	windows := make(map[int64]time.Duration, len(rows))
	hasWindow := make(map[int64]bool, len(rows))

	for _, r := range rows {
		w, ok := effectiveWindow(r)
		windows[r.ID], hasWindow[r.ID] = w, ok
	}

	plans := make([]backendPlan, 0, len(rows))

	for _, r := range rows {
		p := backendPlan{
			id:        r.ID,
			kind:      r.Kind,
			backend:   backendOf(r.Kind),
			org:       r.OrgSlug,
			project:   r.ProjectSlug,
			enabled:   r.Enabled,
			window:    windows[r.ID],
			hasWindow: hasWindow[r.ID],
			quota:     0,
			hashserv:  hashservByProject[r.ProjectID],
			stages:    nil,
		}

		if r.QuotaBytes.Valid && r.QuotaBytes.Int64 > 0 {
			p.quota = r.QuotaBytes.Int64
		}

		// THE sstate RUNG, and it is the only cross-backend one: an sstate object's
		// filename embeds the unihash, so an object whose unihash row is still alive is
		// still reachable and must not be swept for being old. W_sstate is therefore
		// max(own, paired hashserv) -- and "retain forever" is the maximum, so a NULL
		// on either side turns the age rule off entirely rather than defaulting to the
		// other side's number.
		if p.kind == repository.BackendKindSstate && p.hashserv != 0 {
			hw, hok := windows[p.hashserv], hasWindow[p.hashserv]

			switch {
			case !hok:
				p.window, p.hasWindow = 0, false
			case p.hasWindow && hw > p.window:
				p.window = hw
			}
		}

		p.stages = stagesFor(p)
		plans = append(plans, p)
	}

	return plans
}

// effectiveWindow reads a backend's configured window and applies the disabled
// clamp.
//
// A DISABLED BACKEND IS SWEPT HARDER, not skipped (spec §3, finding 10):
// least(configured, 30d). A NULL window on a disabled backend therefore resolves to
// 30 days, mirroring Postgres' own least(), which ignores NULLs -- "retain forever"
// is the largest window there is and least() picks the ceiling.
//
// downloads is the ONE exception, and it is a product property of the kind rather
// than a default (spec §1.2): downloads is an ARCHIVE, not a cache. A premirror
// tarball whose upstream died is unrecoverable, unlike every other namespace's
// eviction, so a NULL window there means never -- including when someone disables
// the backend. Disabling a backend must not be a way to delete an archive.
func effectiveWindow(r repository.ListBackendsForGCRow) (time.Duration, bool) {
	w, ok := durationOf(r.RetentionWindow)

	if r.Enabled {
		return w, ok
	}

	if !ok {
		if r.Kind == repository.BackendKindDownloads {
			return 0, false
		}

		return disabledBackendCeiling, true
	}

	return min(w, disabledBackendCeiling), true
}

// stagesFor is spec §3's stage table, per kind, IN ORDER.
//
// hashserv has no stages here: it owns no cache_objects rows at all, and its two
// stages are self-contained SQL sweeps (sweepHashserv).
func stagesFor(p backendPlan) []stage {
	switch p.kind {
	case repository.BackendKindSstate:
		// Stage 3. The reason is `unreachable` rather than `retention` because the rule
		// is CONJUNCTIVE -- dead iff the window elapsed AND no surviving unihash names
		// it -- and unreachability is the half that distinguishes it from every other
		// stage.
		return []stage{{
			namespace: nsDefault, window: p.window,
			reason: metrics.GCReasonUnreachable, kindLabel: "object",
		}}

	case repository.BackendKindDownloads:
		// Stage 4, and it is SKIPPED entirely under the shipped defaults: downloads'
		// window is NULL and stays NULL (spec §1.2).
		return []stage{{
			namespace: nsDefault, window: p.window,
			reason: metrics.GCReasonRetention, kindLabel: "file",
		}}

	case repository.BackendKindBazel:
		// Stages 5 then 6. The ac family FIRST and cas SECOND is not cosmetic: it is
		// what makes quota eviction shed ActionResults (cheap to lose -- a miss) before
		// CAS blobs (expensive to lose -- Bazel's LostInputsEvent rewind), and it is why
		// W_cas is twice W_ac.
		return []stage{
			{namespace: nsAC, window: p.window, reason: metrics.GCReasonRetention, kindLabel: "ac"},
			{namespace: nsACGRPC, window: p.window, reason: metrics.GCReasonRetention, kindLabel: "ac"},
			{namespace: nsSccache, window: p.window, reason: metrics.GCReasonRetention, kindLabel: "ac"},
			{
				namespace: nsCAS, window: p.window * casWindowFactor,
				reason: metrics.GCReasonRetention, kindLabel: "cas",
			},
		}

	case repository.BackendKindOci:
		// Stages 7, 8, 9: tags before manifests before OCI blobs, which is CLAUDE.md's
		// "sweep tags BEFORE manifests BEFORE the blobs table" for the namespace half
		// (the blobs TABLE is Layer B and runs later still).
		return []stage{
			{namespace: nsTags, window: p.window, reason: metrics.GCReasonRetention, kindLabel: "tag"},
			{
				namespace: nsManifests, window: p.window * casWindowFactor,
				reason: metrics.GCReasonRetention, kindLabel: "manifest",
			},
			{
				namespace: nsBlobs, window: p.window * casWindowFactor,
				reason: metrics.GCReasonRetention, kindLabel: "blob",
			},
		}

	case repository.BackendKindHashserv:
		return nil

	default:
		return nil
	}
}
