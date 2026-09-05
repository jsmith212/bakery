package gc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// orgID resolves the fixture's org, which newFixture creates but does not keep --
// only the second-project test below needs it.
func (f *fixture) orgID() pgtype.UUID {
	f.t.Helper()

	var id pgtype.UUID
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT org_id FROM projects WHERE id = $1`, f.projectID).Scan(&id); err != nil {
		f.t.Fatalf("orgID: %v", err)
	}

	return id
}

// projectUsageRow reads one backend's cache_backend_usage row back.
func (f *fixture) projectUsageRow(backendID int64) (objects, bytes int64, measured time.Time, ok bool) {
	f.t.Helper()

	err := f.pool.QueryRow(f.t.Context(),
		`SELECT objects_count, logical_bytes, measured_at FROM cache_backend_usage WHERE backend_id = $1`,
		backendID).Scan(&objects, &bytes, &measured)
	if err != nil {
		return 0, 0, time.Time{}, false
	}

	return objects, bytes, measured, true
}

// TestMeasureProjectWritesTheUsageRow is the headline: one project's figures are
// correct and current immediately, without a sweep and without a gc_runs row.
//
// The gc_runs assertion is not incidental. The obvious implementation borrows the
// sweep's ScanObjectsForGC, which needs a run id for its frozen snapshot -- and would
// therefore mint a gc_runs row per dashboard load. A measurement deletes nothing, so
// it needs no write barrier and should leave no audit trail behind.
func TestMeasureProjectWritesTheUsageRow(t *testing.T) {
	f := newFixture(t, testConfig())

	sstate := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})

	// Distinct content per key so nothing dedups: the byte total must be the sum of
	// the objects this backend NAMES, which is the logical accounting the quota and
	// the console both use.
	contents := []string{"a", "bb", "ccc", "dddd"}
	want := int64(0)

	for _, c := range contents {
		f.put(sstate, nsDefault, "sstate:"+c, c)
		want += int64(len(c))
	}

	before := countGCRuns(t, f)

	if err := f.eng.MeasureProject(t.Context(), f.projectID); err != nil {
		t.Fatalf("MeasureProject() error = %v", err)
	}

	objects, bytes, measured, ok := f.projectUsageRow(sstate)
	if !ok {
		t.Fatal("no cache_backend_usage row was written")
	}

	if objects != int64(len(contents)) {
		t.Errorf("objects_count = %d, want %d", objects, len(contents))
	}

	if bytes != want {
		t.Errorf("logical_bytes = %d, want %d", bytes, want)
	}

	if time.Since(measured) > time.Minute {
		t.Errorf("measured_at = %v, want ~now", measured)
	}

	if after := countGCRuns(t, f); after != before {
		t.Errorf("gc_runs grew by %d: a measurement must not mint a run row", after-before)
	}
}

// TestMeasureProjectTouchesOnlyItsOwnProject. The whole point of the per-project
// measurement is that a dashboard load costs one project's worth of work; a version
// that measured everything would pass every other assertion in this file.
func TestMeasureProjectTouchesOnlyItsOwnProject(t *testing.T) {
	f := newFixture(t, testConfig())

	mine := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})
	f.put(mine, nsDefault, "sstate:a", "a")

	// A second project in the same org, with its own backend and its own objects.
	other, err := f.store.CreateProject(t.Context(), repository.CreateProjectParams{
		OrgID: f.orgID(), Slug: "other", Name: "Other",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	otherBackend, err := f.store.CreateBackend(t.Context(), repository.CreateBackendParams{
		ProjectID: other.ID, Kind: repository.BackendKindSstate,
		Enabled: true, ReadAuthRequired: true, Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}

	if err := f.eng.MeasureProject(t.Context(), f.projectID); err != nil {
		t.Fatalf("MeasureProject() error = %v", err)
	}

	if _, _, _, ok := f.projectUsageRow(mine); !ok {
		t.Error("the measured project's backend has no usage row")
	}

	if _, _, _, ok := f.projectUsageRow(otherBackend.ID); ok {
		t.Error("a sibling project's backend was measured: the pass is not project-scoped")
	}
}

// TestMeasureProjectExcludesHashserv. hashserv owns no cache_objects rows, and the
// sweep's own plan gives it no stages -- so it structurally never gets a
// cache_backend_usage row, and several console screens rely on that: they read a
// missing row as "not applicable" rather than as a live, permanent zero.
func TestMeasureProjectExcludesHashserv(t *testing.T) {
	f := newFixture(t, testConfig())

	hashserv := f.backend(repository.BackendKindHashserv, backendOpts{window: 90 * 24 * time.Hour})
	sstate := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})

	f.put(sstate, nsDefault, "sstate:a", "a")

	if err := f.eng.MeasureProject(t.Context(), f.projectID); err != nil {
		t.Fatalf("MeasureProject() error = %v", err)
	}

	if _, _, _, ok := f.projectUsageRow(hashserv); ok {
		t.Error("hashserv got a usage row: 'never measured' becomes a permanent live zero")
	}

	if _, _, _, ok := f.projectUsageRow(sstate); !ok {
		t.Error("the sstate backend was not measured")
	}
}

// TestMeasureProjectMeasuresAnEmptyBackendAsZero. "Measured, and genuinely empty" and
// "never measured" are different facts and the console renders them differently (an
// em dash versus 0 B). A LEFT JOIN is what makes the first one reachable at all -- an
// inner join would leave a backend with no objects permanently unmeasured, and the
// dashboard would say "not yet measured" forever for a backend that is simply empty.
func TestMeasureProjectMeasuresAnEmptyBackendAsZero(t *testing.T) {
	f := newFixture(t, testConfig())

	empty := f.backend(repository.BackendKindBazel, backendOpts{window: 30 * 24 * time.Hour})

	if err := f.eng.MeasureProject(t.Context(), f.projectID); err != nil {
		t.Fatalf("MeasureProject() error = %v", err)
	}

	objects, bytes, _, ok := f.projectUsageRow(empty)
	if !ok {
		t.Fatal("an empty backend got no usage row: it stays 'never measured' forever")
	}

	if objects != 0 || bytes != 0 {
		t.Errorf("empty backend measured %d objects / %d bytes, want 0/0", objects, bytes)
	}
}

// TestMeasureProjectSingleflightCollapsesConcurrentCallers.
//
// This is the property that makes measuring on a READ affordable. One person opening
// a project fires several parallel loads, and several people open the same dashboard
// at once -- every one of them sees the same "stale" answer from the freshness gate,
// so without collapsing them every one issues the same aggregate over the same rows.
//
// The assertion is on the QUERY COUNT, through a counting wrapper around the real
// store, because that is the only thing that distinguishes a singleflight from a
// mutex: both serialise, only one of them does the work once.
func TestMeasureProjectSingleflightCollapsesConcurrentCallers(t *testing.T) {
	f := newFixture(t, testConfig())

	backend := f.backend(repository.BackendKindSstate, backendOpts{window: 90 * 24 * time.Hour})
	f.put(backend, nsDefault, "sstate:a", "a")

	counter := &countingMeasureQueries{Queries: f.eng.db, gate: make(chan struct{})}
	f.eng.db = counter

	const callers = 8

	var wg sync.WaitGroup

	errs := make([]error, callers)

	for i := range callers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			errs[i] = f.eng.MeasureProject(t.Context(), f.projectID)
		}()
	}

	// Let every caller reach DoChan before the leader's query returns, which is the
	// race the singleflight exists to win. Without the gate the goroutines could run
	// strictly one after another and the test would prove nothing.
	waitFor(t, func() bool { return counter.waiting() >= 1 })
	time.Sleep(50 * time.Millisecond)
	close(counter.gate)

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: MeasureProject() error = %v", i, err)
		}
	}

	if n := counter.count(); n != 1 {
		t.Errorf("MeasureProjectUsage ran %d times for %d concurrent callers, want 1", n, callers)
	}
}

// countingMeasureQueries counts MeasureProjectUsage calls and holds the first one
// open until its gate closes, so every concurrent caller is guaranteed to be waiting
// on the same flight.
type countingMeasureQueries struct {
	Queries

	gate chan struct{}

	mu      sync.Mutex
	calls   int
	blocked int
}

func (c *countingMeasureQueries) MeasureProjectUsage(
	ctx context.Context, projectID pgtype.UUID,
) ([]repository.MeasureProjectUsageRow, error) {
	c.mu.Lock()
	c.calls++
	c.blocked++
	c.mu.Unlock()

	<-c.gate

	c.mu.Lock()
	c.blocked--
	c.mu.Unlock()

	return c.Queries.MeasureProjectUsage(ctx, projectID)
}

func (c *countingMeasureQueries) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.calls
}

func (c *countingMeasureQueries) waiting() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.blocked
}

// waitFor polls until cond holds or the test times out. A poll rather than a channel
// because the condition it waits on is a counter inside a fake, not an event.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(2 * time.Millisecond)
	}

	t.Fatal("timed out waiting for the leader to reach the query")
}

// countGCRuns is the assertion that a measurement leaves no audit trail.
func countGCRuns(t *testing.T, f *fixture) int64 {
	t.Helper()

	var n int64
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM gc_runs`).Scan(&n); err != nil {
		t.Fatalf("count gc_runs: %v", err)
	}

	return n
}

// TestMeasureProjectBacksOffAfterAFailedAttempt is the backoff, and the bug it closes
// is a self-inflicted denial of service on the biggest project in the installation.
//
// The freshness gate that decides whether a page load measures reads measured_at, and
// measured_at is written only when a measurement SUCCEEDS. A project whose aggregate
// exceeds measureTimeout therefore stays stale FOREVER: every dashboard load sees a
// stale row, starts another ten-second aggregate, times out, writes nothing, and the
// next load does it again. The row never moves and the database never stops. The same
// hole was open on the explicit refresh, whose own gate read the same column.
//
// So the attempt is what is remembered, and it is remembered whatever the outcome.
func TestMeasureProjectBacksOffAfterAFailedAttempt(t *testing.T) {
	eng, q, _ := fakeEngine(t, testConfig())

	q.measureErr = context.DeadlineExceeded
	project := uuidOf(7)

	if err := eng.MeasureProject(t.Context(), project); err == nil {
		t.Fatal("MeasureProject() error = nil, want the failed attempt to surface")
	}

	// The second caller is refused, and refusing is a NO-OP, not an error: the handler
	// serves the row on file, whose measured_at says how old it is.
	if err := eng.MeasureProject(t.Context(), project); err != nil {
		t.Errorf("MeasureProject() inside the interval error = %v, want nil", err)
	}

	if got := q.count("MeasureProjectUsage"); got != 1 {
		t.Errorf("MeasureProjectUsage ran %d times, want 1: a failed attempt must back off", got)
	}

	// A DIFFERENT project is not held back by this one's attempt.
	if err := eng.MeasureProject(t.Context(), uuidOf(8)); err == nil {
		t.Error("MeasureProject() for another project was refused; the backoff is per project")
	}

	if got := q.count("MeasureProjectUsage"); got != 2 {
		t.Errorf("MeasureProjectUsage ran %d times, want 2 across two projects", got)
	}
}

// TestMeasureProjectDoesNotHoldTheWindowOnAnEmptyProject is the exception the backoff
// needs, and without it the rate limit reintroduces the complaint the whole feature
// exists to answer.
//
// A project that configures no measurable backend aggregates to NO ROWS: a trivial
// index lookup on cache_backends, which cannot be the expensive query the backoff
// guards against, and which writes nothing -- so there is no stale row for the next
// caller to be served instead. Holding the window there means a backend created
// seconds after somebody opened the project overview reads "not yet measured" until it
// expires, with Refresh doing nothing.
func TestMeasureProjectDoesNotHoldTheWindowOnAnEmptyProject(t *testing.T) {
	eng, q, _ := fakeEngine(t, testConfig())

	project := uuidOf(9)

	for range 3 {
		if err := eng.MeasureProject(t.Context(), project); err != nil {
			t.Fatalf("MeasureProject() error = %v", err)
		}
	}

	if got := q.count("MeasureProjectUsage"); got != 3 {
		t.Errorf("MeasureProjectUsage ran %d times, want 3: a measurement that wrote "+
			"nothing must not hold the window", got)
	}
}
