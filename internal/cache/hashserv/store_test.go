package hashserv

import (
	"context"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// The equivalence algorithm is the whole point of hash equivalence, and it is a story about
// what is ALREADY IN THE DATABASE. A fake store cannot pose the questions that matter here --
// "did ON CONFLICT DO NOTHING really no-op?", "did ORDER BY created_at really pick the oldest
// row?" -- so these run against a real migrated Postgres.
func TestMain(m *testing.M) { dbtest.Main(m) }

// The fixture values are upstream's own, lifted from bitbake's lib/hashserv/tests.py, so a
// divergence from the reference server shows up here rather than in CI.
const (
	testMethod = "oe.sstatesig.OEOuthashBasic"

	taskhash1 = "3bde230c743fc45ab61a065d7a1815fbfa01c4740e4c895af2eb8dc0f684a4ab"
	unihash1  = "3bde230c743fc45ab61a065d7a1815fbfa01c4740e4c895af2eb8dc0f684a4ab"
	outhash1  = "afd11c366050bcd75ad763e898e4430e2a60659b26f83fbb22201a60672019fa"

	taskhash2 = "6259ae8263bd94d454c086f501c37e64c4e83cae806902ca95b4ab513546b273"
	unihash2  = "6259ae8263bd94d454c086f501c37e64c4e83cae806902ca95b4ab513546b273"

	outhash3 = "d2187ee3a8966db10b34fe0e863482288d9a6185cb8ef58a6c1c6ace87a2f24c"
)

// newStore builds a store bound to a real backend row. It returns two stores over two
// DIFFERENT projects, because the single most important property of this schema is that they
// cannot see each other: upstream's hashserv is single-tenant and has no test for this, so if
// we get it wrong nothing upstream will tell us.
func newStores(t *testing.T) (a, b store, ctx context.Context) {
	t.Helper()

	pool := dbtest.New(t)
	s := db.NewStore(pool)
	ctx = t.Context()

	mk := func(orgSlug, projSlug string) store {
		t.Helper()

		org, err := s.CreateOrganization(ctx, repository.CreateOrganizationParams{
			Slug: orgSlug, Name: orgSlug,
		})
		if err != nil {
			t.Fatalf("CreateOrganization: %v", err)
		}

		project, err := s.CreateProject(ctx, repository.CreateProjectParams{
			OrgID: org.ID, Slug: projSlug, Name: projSlug,
		})
		if err != nil {
			t.Fatalf("CreateProject: %v", err)
		}

		backend, err := s.CreateBackend(ctx, repository.CreateBackendParams{
			ProjectID:        project.ID,
			Kind:             repository.BackendKindHashserv,
			Enabled:          true,
			ReadAuthRequired: true,
			Config:           []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("CreateBackend: %v", err)
		}

		return store{q: s, backendID: backend.ID}
	}

	return mk("acme", "widget"), mk("globex", "gadget"), ctx
}

func report(t *testing.T, s store, ctx context.Context, taskhash, outhash, unihash string) unihashResponse {
	t.Helper()

	resp, _, err := s.report(ctx, reportRequest{
		Method:   testMethod,
		Taskhash: taskhash,
		Outhash:  outhash,
		Unihash:  unihash,
	}, nil)
	if err != nil {
		t.Fatalf("report(%s): %v", taskhash[:8], err)
	}

	return resp
}

func TestReportMintsThenAdoptsEquivalent(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	// First sighting: nothing to be equivalent to, so the client's unihash is minted.
	if got := report(t, s, ctx, taskhash1, outhash1, unihash1); got.Unihash != unihash1 {
		t.Fatalf("first report = %s, want the client's own unihash %s", got.Unihash, unihash1)
	}

	// THE EQUIVALENCE. A different taskhash produced the SAME output, so it must inherit the
	// first task's unihash and IGNORE the one it proposed. This is the entire product: the
	// downstream rebuild stops propagating here.
	got, equivalent, err := s.report(ctx, reportRequest{
		Method: testMethod, Taskhash: taskhash2, Outhash: outhash1, Unihash: unihash2,
	}, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if got.Unihash != unihash1 {
		t.Fatalf("equivalent report = %s, want task 1's unihash %s", got.Unihash, unihash1)
	}

	if !equivalent {
		t.Error("equivalent = false; the caller needs this to be true to meter the one thing that matters")
	}
}

// TestDivergingReportRace is upstream's test of the same name, and it is the test that
// catches a server which returns the unihash it just "minted" instead of re-reading what is
// actually stored. Nothing about it is concurrent -- the name is upstream's.
func TestDivergingReportRace(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	report(t, s, ctx, taskhash1, outhash1, unihash1)

	if got := report(t, s, ctx, taskhash2, outhash1, unihash2); got.Unihash != unihash1 {
		t.Fatalf("report 2 = %s, want %s", got.Unihash, unihash1)
	}

	// Task 2 reports AGAIN under a DIFFERENT outhash -- a non-deterministic task. The outhash
	// row is new, and nothing is equivalent to it, so the algorithm "mints" the client's
	// unihash2. But taskhash2 is already mapped to unihash1 and the mapping is write-once, so
	// the insert no-ops. Only the re-read returns the truth. A server that returned the minted
	// value would answer unihash2 and hand this task an sstate object name that does not exist.
	if got := report(t, s, ctx, taskhash2, outhash3, unihash2); got.Unihash != unihash1 {
		t.Fatalf("report 3 = %s, want %s -- the re-read is what makes this right", got.Unihash, unihash1)
	}
}

// TestDivergingReportReverseRace reports the same three facts in the opposite order.
func TestDivergingReportReverseRace(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	report(t, s, ctx, taskhash1, outhash1, unihash1)

	// Task 2 under a novel outhash first: nothing equivalent, so it keeps its own unihash.
	if got := report(t, s, ctx, taskhash2, outhash3, unihash2); got.Unihash != unihash2 {
		t.Fatalf("report of a novel outhash = %s, want the client's own %s", got.Unihash, unihash2)
	}

	// Now task 2 reports outhash1, which task 1 already owns. The outhash row is new and an
	// equivalent EXISTS -- but taskhash2's unihash is already written, and write-once means it
	// does not move. The answer is still unihash2.
	if got := report(t, s, ctx, taskhash2, outhash1, unihash2); got.Unihash != unihash2 {
		t.Fatalf("report 3 = %s, want %s -- write-once must not be overwritten by a late equivalence",
			got.Unihash, unihash2)
	}
}

// TestUnihashIsWriteOnce is the hostile case: a client re-reports a taskhash it already
// owns, claiming a different unihash. If that were honoured, every downstream task that has
// already baked the old unihash into its own taskhash -- and into an sstate object filename
// -- would be stranded.
func TestUnihashIsWriteOnce(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	report(t, s, ctx, taskhash1, outhash1, unihash1)

	hostile := strings.Repeat("f", 64)
	if got := report(t, s, ctx, taskhash1, outhash1, hostile); got.Unihash != unihash1 {
		t.Fatalf("re-report = %s, want the original %s: (method, taskhash) -> unihash is write-once",
			got.Unihash, unihash1)
	}

	stored, ok, err := s.getUnihash(ctx, testMethod, taskhash1)
	if err != nil || !ok {
		t.Fatalf("getUnihash: %v ok=%v", err, ok)
	}

	if stored != unihash1 {
		t.Fatalf("stored unihash = %s, want %s", stored, unihash1)
	}
}

// TestEquivalenceAdoptsTheOldest pins ORDER BY created_at ASC. Picking the newest instead
// would still "work" -- every test above would pass -- but unihashes would drift every time a
// new task reported the same output, and drifting unihashes invalidate sstate objects for no
// reason. It is stability, not correctness, and only this test defends it.
func TestEquivalenceAdoptsTheOldest(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	// Three distinct taskhashes, all producing outhash1. The first one to land owns the
	// unihash that all the others must adopt.
	first := strings.Repeat("1", 64)
	second := strings.Repeat("2", 64)
	third := strings.Repeat("3", 64)

	report(t, s, ctx, first, outhash1, first)
	report(t, s, ctx, second, outhash1, second)

	if got := report(t, s, ctx, third, outhash1, third); got.Unihash != first {
		t.Fatalf("third report adopted %s, want the OLDEST %s", got.Unihash, first)
	}
}

// TestReportReadOnlyNeverWrites is the anonymous / read-scoped path. It must answer, and it
// must not write -- an unauthenticated write is a cache-poisoning vector.
func TestReportReadOnlyNeverWrites(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	req := reportRequest{Method: testMethod, Taskhash: taskhash1, Outhash: outhash1, Unihash: unihash1}

	// Nothing known: it echoes the client's own unihash back, so the build proceeds.
	resp, err := s.reportReadOnly(ctx, req)
	if err != nil {
		t.Fatalf("reportReadOnly: %v", err)
	}

	if resp.Unihash != unihash1 {
		t.Fatalf("read-only report = %s, want the echoed %s", resp.Unihash, unihash1)
	}

	if _, ok, err := s.getUnihash(ctx, testMethod, taskhash1); err != nil || ok {
		t.Fatalf("read-only report WROTE a unihash row (ok=%v); it must never write", ok)
	}

	// Once a real report exists, the read-only path returns the STORED unihash, not the echo.
	report(t, s, ctx, taskhash1, outhash1, unihash1)

	other := strings.Repeat("a", 64)
	resp, err = s.reportReadOnly(ctx, reportRequest{
		Method: testMethod, Taskhash: taskhash1, Outhash: outhash1, Unihash: other,
	})
	if err != nil {
		t.Fatalf("reportReadOnly: %v", err)
	}

	if resp.Unihash != unihash1 {
		t.Fatalf("read-only report = %s, want the stored %s", resp.Unihash, unihash1)
	}
}

// TestReportReadOnlyFindsEquivalenceAcrossTaskhashes is a regression test for a bug that was
// live in this package: the outhash lookup filtered on taskhash as well as (method, outhash).
//
// It looked tighter and it broke read-only equivalence completely. Keyed on the caller's own
// taskhash, the lookup can only ever find the caller's own row -- so an anonymous client on an
// open mirror would be told "no equivalent" every single time, and would keep its own unihash
// forever. The mirror would appear to work, serve every request, return valid-looking answers,
// and deliver no equivalence at all. Upstream keys on (method, outhash) alone, for exactly
// this reason.
func TestReportReadOnlyFindsEquivalenceAcrossTaskhashes(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	// Task 1 has reported this output, with a write-scoped credential.
	report(t, s, ctx, taskhash1, outhash1, unihash1)

	// Task 2 -- a DIFFERENT taskhash -- produced the same output, but is reporting read-only.
	// It must be told task 1's unihash, not its own.
	resp, err := s.reportReadOnly(ctx, reportRequest{
		Method: testMethod, Taskhash: taskhash2, Outhash: outhash1, Unihash: unihash2,
	})
	if err != nil {
		t.Fatalf("reportReadOnly: %v", err)
	}

	if resp.Unihash != unihash1 {
		t.Fatalf("read-only report = %s, want task 1's %s: the lookup must key on (method, outhash) "+
			"alone, or an open mirror silently delivers no equivalence", resp.Unihash, unihash1)
	}
}

// TestBackendsAreIsolated is the test upstream cannot have, and the one whose absence would
// be worst: hashserv is single-tenant, Bakery is not. A taskhash reported by one customer
// must be invisible to every other, or we would be handing out one customer's unihashes --
// and therefore pointing builds at another customer's sstate objects.
func TestBackendsAreIsolated(t *testing.T) {
	t.Parallel()

	acme, globex, ctx := newStores(t)

	report(t, acme, ctx, taskhash1, outhash1, unihash1)

	if _, ok, err := globex.getUnihash(ctx, testMethod, taskhash1); err != nil || ok {
		t.Fatalf("globex can see acme's unihash (ok=%v); backends must be isolated", ok)
	}

	if ok, err := globex.unihashExists(ctx, unihash1); err != nil || ok {
		t.Fatalf("globex can see acme's unihash via exists (ok=%v)", ok)
	}

	// And the same outhash reported by the other tenant must NOT adopt acme's unihash -- the
	// equivalence query has to be scoped too, not just the lookups.
	got, equivalent, err := globex.report(ctx, reportRequest{
		Method: testMethod, Taskhash: taskhash2, Outhash: outhash1, Unihash: unihash2,
	}, nil)
	if err != nil {
		t.Fatalf("report: %v", err)
	}

	if equivalent || got.Unihash != unihash2 {
		t.Fatalf("globex adopted %s across the tenant boundary (equivalent=%v); want its own %s",
			got.Unihash, equivalent, unihash2)
	}
}

// TestPersistUpstreamBindsUpstreamTaskhashNotTheCallers is the regression test for a
// cache-poisoning bug the adversarial review caught.
//
// get-outhash is keyed on (method, outhash); the taskhash the caller sends is ignored by the
// lookup. So the taskhash in the UPSTREAM RESPONSE is the one that legitimately owns the
// returned unihash. Writing the CALLER's taskhash against that unihash instead would let a
// client holding only @read (or none, on an open mirror) forge a permanent, write-once
// (method, taskhash) -> unihash mapping out of two values it fully controls -- and since the
// unihash is embedded in the sstate filename, aim a victim's build at the wrong object.
func TestPersistUpstreamBindsUpstreamTaskhashNotTheCallers(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	const attackerTaskhash = "1111111111111111111111111111111111111111"
	const upstreamTaskhash = "2222222222222222222222222222222222222222"

	// The row as it comes back from upstream: its OWN taskhash, self-consistent with its unihash.
	up := outhashResponse{
		Method:   testMethod,
		Taskhash: upstreamTaskhash,
		Outhash:  outhash1,
		Unihash:  unihash1,
	}

	if err := s.persistUpstream(ctx, up); err != nil {
		t.Fatalf("persistUpstream: %v", err)
	}

	// The upstream taskhash is bound to the upstream unihash.
	if got, ok, _ := s.getUnihash(ctx, testMethod, upstreamTaskhash); !ok || got != unihash1 {
		t.Fatalf("upstream taskhash -> %q ok=%v, want %s", got, ok, unihash1)
	}

	// The attacker's taskhash was NEVER written. This is the whole point.
	if _, ok, _ := s.getUnihash(ctx, testMethod, attackerTaskhash); ok {
		t.Fatal("the caller's taskhash was bound to an upstream unihash: cache-poisoning primitive")
	}

	// And the outhash row was persisted, so a later local get-outhash for this output is a hit.
	if _, ok, _ := s.getOuthashRaw(ctx, testMethod, outhash1); !ok {
		t.Error("persistUpstream did not write the outhash row; the local cache never warms")
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		where map[string]string
		// wantUnihashGone / wantOuthashGone encode upstream's per-table asymmetry, which is
		// load-bearing: the outhash table has no unihash column and vice versa, so a filter on
		// one column simply does not reach the other table.
		wantUnihashGone bool
		wantOuthashGone bool
		wantCount       int64
	}{
		{
			name:            "by taskhash hits both tables",
			where:           map[string]string{"taskhash": taskhash1},
			wantUnihashGone: true, wantOuthashGone: true, wantCount: 2,
		},
		{
			name:            "by method hits both tables",
			where:           map[string]string{"method": testMethod},
			wantUnihashGone: true, wantOuthashGone: true, wantCount: 2,
		},
		{
			name:            "by unihash hits ONLY the unihash table",
			where:           map[string]string{"unihash": unihash1},
			wantUnihashGone: true, wantOuthashGone: false, wantCount: 1,
		},
		{
			name:            "by outhash hits ONLY the outhash table",
			where:           map[string]string{"outhash": outhash1},
			wantUnihashGone: false, wantOuthashGone: true, wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, _, ctx := newStores(t)
			report(t, s, ctx, taskhash1, outhash1, unihash1)

			count, err := s.remove(ctx, tt.where)
			if err != nil {
				t.Fatalf("remove(%v): %v", tt.where, err)
			}

			if count != tt.wantCount {
				t.Errorf("remove count = %d, want %d", count, tt.wantCount)
			}

			_, ok, err := s.getUnihash(ctx, testMethod, taskhash1)
			if err != nil {
				t.Fatalf("getUnihash: %v", err)
			}

			if ok == tt.wantUnihashGone {
				t.Errorf("unihash row present = %v, want gone = %v", ok, tt.wantUnihashGone)
			}

			// Probe the RAW outhash row, not the joined one: a remove-by-unihash deletes the
			// unihash row and the join would then report the surviving outhash row as absent,
			// which would hide exactly the asymmetry this table is asserting.
			_, ok, err = s.getOuthashRaw(ctx, testMethod, outhash1)
			if err != nil {
				t.Fatalf("getOuthashRaw: %v", err)
			}

			if ok == tt.wantOuthashGone {
				t.Errorf("outhash row present = %v, want gone = %v", ok, tt.wantOuthashGone)
			}
		})
	}
}

// TestRemoveRefusesUnknownColumns: `remove` deletes GC ROOTS. An unrecognized filter must be
// a loud error, never a delete that quietly matches nothing -- or, far worse, everything.
func TestRemoveRefusesUnknownColumns(t *testing.T) {
	t.Parallel()

	s, _, ctx := newStores(t)

	for _, where := range []map[string]string{
		{"owner": "someone"},
		{"PN": "zlib"},
		{"1": "1"},
		{},
		{"taskhash": taskhash1, "method": testMethod},
	} {
		if _, err := s.remove(ctx, where); err == nil {
			t.Errorf("remove(%v) succeeded; an unrecognized or ambiguous filter must be refused", where)
		} else if !isInvokeError(err) {
			t.Errorf("remove(%v) error = %v, want an invoke-error the client can see", where, err)
		}
	}
}

// TestRemoveIsScopedToItsBackend: a key can only ever purge the project it was minted
// against. If `remove` leaked across the backend_id boundary, one customer's CI could delete
// another's hash equivalence.
func TestRemoveIsScopedToItsBackend(t *testing.T) {
	t.Parallel()

	acme, globex, ctx := newStores(t)

	report(t, acme, ctx, taskhash1, outhash1, unihash1)
	report(t, globex, ctx, taskhash1, outhash1, unihash1)

	if _, err := globex.remove(ctx, map[string]string{"method": testMethod}); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, ok, err := acme.getUnihash(ctx, testMethod, taskhash1); err != nil || !ok {
		t.Fatalf("globex's remove deleted acme's rows (ok=%v)", ok)
	}
}
