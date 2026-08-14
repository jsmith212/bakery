package gc

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

func uuidOf(n byte) pgtype.UUID {
	var u pgtype.UUID

	u.Bytes[15] = n
	u.Valid = true

	return u
}

func backendRow(id int64, kind repository.BackendKind, project pgtype.UUID, window time.Duration, enabled bool) repository.ListBackendsForGCRow {
	w := pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: false}
	if window > 0 {
		w = interval(window)
	}

	return repository.ListBackendsForGCRow{
		ID: id, Kind: kind, Enabled: enabled, RetentionWindow: w,
		QuotaBytes:  pgtype.Int8{Int64: 0, Valid: false},
		ProjectID:   project,
		ProjectSlug: "widget",
		OrgID:       uuidOf(255),
		OrgSlug:     "acme",
	}
}

// stageWindow finds a plan's window for one namespace.
func stageWindow(t *testing.T, p backendPlan, namespace string) time.Duration {
	t.Helper()

	for _, st := range p.stages {
		if st.namespace == namespace {
			return st.window
		}
	}

	t.Fatalf("backend %d has no %q stage", p.id, namespace)

	return 0
}

// THE LADDER IS DERIVED, SO NO CONFIGURATION CAN INVERT IT (spec §4).
//
// An sstate object's filename embeds the unihash, so an object whose unihash row is
// still alive is still reachable -- W_sstate must therefore be at least the paired
// hashserv's window whichever way an operator sets the two. Within a backend the
// question does not even arise: W_cas and W_manifests are 2 x W and there is no knob
// that writes them, which is the same posture as greatest(oidc_role, local_role)
// being computed by the database rather than by application code.
func TestWindowLadderIgnoresAnInvertedConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sstateWindow time.Duration
		hashsWindow  time.Duration
		paired       bool
		want         time.Duration
		wantSet      bool
	}{
		{
			name:         "sstate shorter than its hashserv is raised",
			sstateWindow: day(7), hashsWindow: day(90), paired: true,
			want: day(90), wantSet: true,
		},
		{
			name:         "sstate longer than its hashserv is kept",
			sstateWindow: day(90), hashsWindow: day(7), paired: true,
			want: day(90), wantSet: true,
		},
		{
			// "Retain forever" is the LARGEST window there is, so a NULL on either side
			// turns the sstate age rule off rather than falling back to the other side's
			// number: a live unihash may still name an object of any age.
			name:         "a forever hashserv makes sstate forever",
			sstateWindow: day(30), hashsWindow: 0, paired: true,
			want: 0, wantSet: false,
		},
		{
			name:         "no paired hashserv leaves sstate on its own window",
			sstateWindow: day(30), hashsWindow: 0, paired: false,
			want: day(30), wantSet: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			project := uuidOf(1)
			rows := []repository.ListBackendsForGCRow{
				backendRow(1, repository.BackendKindSstate, project, tc.sstateWindow, true),
			}

			if tc.paired {
				rows = append(rows,
					backendRow(2, repository.BackendKindHashserv, project, tc.hashsWindow, true))
			}

			plans := buildPlans(rows)

			if plans[0].hasWindow != tc.wantSet || plans[0].window != tc.want {
				t.Fatalf("W_sstate = %v (set %v), want %v (set %v)",
					plans[0].window, plans[0].hasWindow, tc.want, tc.wantSet)
			}
		})
	}
}

// The within-backend rungs: the NAMED outlives the NAMER, always, by construction.
func TestDerivedNamespaceWindows(t *testing.T) {
	t.Parallel()

	project := uuidOf(2)
	plans := buildPlans([]repository.ListBackendsForGCRow{
		backendRow(1, repository.BackendKindBazel, project, day(30), true),
		backendRow(2, repository.BackendKindOci, project, day(30), true),
	})

	bazel, oci := plans[0], plans[1]

	for _, ns := range []string{nsAC, nsACGRPC, nsSccache} {
		if got := stageWindow(t, bazel, ns); got != day(30) {
			t.Errorf("W_%s = %v, want 30d", ns, got)
		}
	}

	// W_cas = 2 x W_ac. It is defence in depth behind the §6.3 reachability touch:
	// under --remote_download_minimal an AC hit never reads its outputs, so without
	// both the touch AND this rung "hot AC, cold CAS" is the normal steady state and
	// the sweep would delete exactly the blobs a live ActionResult names.
	if got := stageWindow(t, bazel, nsCAS); got != day(60) {
		t.Errorf("W_cas = %v, want 60d (2 x W_ac)", got)
	}

	if got := stageWindow(t, oci, nsTags); got != day(30) {
		t.Errorf("W_tags = %v, want 30d", got)
	}

	for _, ns := range []string{nsManifests, nsBlobs} {
		if got := stageWindow(t, oci, ns); got != day(60) {
			t.Errorf("W_%s = %v, want 60d (2 x W_tags)", ns, got)
		}
	}

	// The stage ORDER is the other half of the ladder, and quota eviction depends on
	// it as much as retention does.
	wantOrder := []string{nsAC, nsACGRPC, nsSccache, nsCAS}
	for i, st := range bazel.stages {
		if st.namespace != wantOrder[i] {
			t.Fatalf("bazel stage %d is %q, want %q", i, st.namespace, wantOrder[i])
		}
	}

	wantOrder = []string{nsTags, nsManifests, nsBlobs}
	for i, st := range oci.stages {
		if st.namespace != wantOrder[i] {
			t.Fatalf("oci stage %d is %q, want %q", i, st.namespace, wantOrder[i])
		}
	}
}

// A DISABLED BACKEND IS SWEPT HARDER, NOT SKIPPED (spec §3, finding 10) -- with the
// one exception the product decision forces: downloads is an archive, and disabling
// its backend must not become a way to delete one.
func TestEffectiveWindowUnderTheDisabledClamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    repository.BackendKind
		window  time.Duration
		enabled bool
		want    time.Duration
		wantSet bool
	}{
		{
			name: "enabled keeps its configured window", kind: repository.BackendKindSstate,
			window: day(90), enabled: true, want: day(90), wantSet: true,
		},
		{
			name: "disabled is clamped to 30d", kind: repository.BackendKindSstate,
			window: day(90), enabled: false, want: day(30), wantSet: true,
		},
		{
			name: "disabled keeps a shorter window", kind: repository.BackendKindBazel,
			window: day(7), enabled: false, want: day(7), wantSet: true,
		},
		{
			// least(forever, 30d) is 30d, mirroring Postgres' own least(), which ignores
			// NULLs: a disabled backend with no window is the case finding 10 is about --
			// it serves no traffic, so nothing ever touches accessed_at, and its rows would
			// pin deduped digests forever.
			name: "disabled with no window resolves to 30d", kind: repository.BackendKindOci,
			window: 0, enabled: false, want: day(30), wantSet: true,
		},
		{
			name:   "a disabled downloads archive is still retained forever",
			kind:   repository.BackendKindDownloads,
			window: 0, enabled: false, want: 0, wantSet: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := effectiveWindow(backendRow(1, tc.kind, uuidOf(3), tc.window, tc.enabled))
			if ok != tc.wantSet || got != tc.want {
				t.Errorf("effectiveWindow() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantSet)
			}
		})
	}
}
