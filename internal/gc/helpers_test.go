package gc

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/storage"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

// orgCounter keeps each fixture's org slug unique within one test database.
var orgCounter atomic.Int64

// fixture is a real Postgres, a real blob.Service over a real temp-dir byte store,
// and a real Engine. The predicates under test live in SQL and in the refcount
// trigger, so there is nothing useful a fake could prove about them.
type fixture struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *db.Store
	blobs *blob.Service
	mx    *metrics.Metrics
	eng   *Engine
	touch *stubUnihashFlusher

	orgSlug     string
	projectSlug string
	projectID   pgtype.UUID
}

// testConfig is the shipped configuration with the pacing turned off: a test that
// waits out 100ms per chunk to prove a predicate is a slow test proving nothing.
// TestSweepRespectsBatchSizeAndPause sets its own.
func testConfig() Config {
	return Config{
		Enabled: true, AllowMultiInstance: false,
		Interval: time.Hour, UsageInterval: time.Hour, GracePeriod: 0,
		BatchSize: 100, BatchPause: 0, DisableRetention: false, TouchStaleness: time.Hour,
	}
}

func newFixture(t *testing.T, cfg Config) *fixture {
	t.Helper()

	pool := dbtest.New(t)
	store := db.NewStore(pool)
	ctx := t.Context()

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	mx := metrics.New()

	blobs, err := blob.New(blob.Config{
		Reader: store, Tx: store, Storage: storage.NewInstrumented(local, mx, metrics.DriverLocal),
		Metrics: mx, CacheSize: 4096, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("blob.New() error = %v", err)
	}

	// The unihash flusher is a stub over the REAL TouchUnihashesAccessed statement
	// rather than a live *hashserv.Backend: reaching hashserv's own toucher requires
	// a WebSocket session and its RPC layer, and what this package's tests are the
	// subject of is the ENGINE -- that it forces a flush BEFORE stage 1 rather than
	// after it, or not at all. hashserv's own suite proves FlushNow writes inside the
	// staleness window (TestBackendFlushNowForcesWriteInsideStalenessWindow); this
	// stub issues exactly the statement that flush ends in.
	touch := &stubUnihashFlusher{store: store, marks: nil}

	eng, err := New(t.Context(), Deps{
		DB: store, Blobs: blobs, Metrics: mx, Log: slog.New(slog.DiscardHandler),
		Access: blobs, Unihash: touch,
	}, cfg)
	if err != nil {
		t.Fatalf("gc.New() error = %v", err)
	}

	slug := fmt.Sprintf("acme-%d", orgCounter.Add(1))

	org, err := store.CreateOrganization(ctx, repository.CreateOrganizationParams{Slug: slug, Name: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrganization() error = %v", err)
	}

	project, err := store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget", Name: "Widget",
	})
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}

	return &fixture{
		t: t, pool: pool, store: store, blobs: blobs, mx: mx, eng: eng, touch: touch,
		orgSlug: slug, projectSlug: "widget", projectID: project.ID,
	}
}

// stubUnihashFlusher stands in for *hashserv.Backend's unihash toucher: mark()
// records a (backend, method, taskhash) the way a get-stream hit does, and
// FlushNow issues the one UPDATE the real flusher issues.
type stubUnihashFlusher struct {
	store *db.Store
	marks []repository.TouchUnihashesAccessedParams
}

func (s *stubUnihashFlusher) mark(backendID int64, method, taskhash string) {
	s.marks = append(s.marks, repository.TouchUnihashesAccessedParams{
		BackendID: backendID, Methods: []string{method}, Taskhashes: []string{taskhash},
		Staleness: pgtype.Interval{Microseconds: 0, Days: 0, Months: 0, Valid: true},
	})
}

func (s *stubUnihashFlusher) FlushNow(ctx context.Context) (int64, error) {
	var rows int64

	for _, m := range s.marks {
		n, err := s.store.TouchUnihashesAccessed(ctx, m)
		if err != nil {
			return rows, err
		}

		rows += n
	}

	s.marks = nil

	return rows, nil
}

// backendOpts is what a test wants a cache_backends row to say. A zero window means
// NULL -- "retain forever" -- which is the shipped state of a downloads backend.
type backendOpts struct {
	window   time.Duration
	quota    int64
	disabled bool
}

func (f *fixture) backend(kind repository.BackendKind, opts backendOpts) int64 {
	f.t.Helper()

	row, err := f.store.CreateBackend(f.t.Context(), repository.CreateBackendParams{
		ProjectID:        f.projectID,
		Kind:             kind,
		Enabled:          !opts.disabled,
		ReadAuthRequired: true,
		Config:           []byte(`{}`),
	})
	if err != nil {
		f.t.Fatalf("CreateBackend(%s) error = %v", kind, err)
	}

	// retention_window and quota_bytes are set here rather than through CreateBackend
	// because seeding them at creation is the API's job, not the engine's, and this
	// package's tests must be able to express a window CreateBackend would never seed.
	var window, quota any

	if opts.window > 0 {
		window = fmt.Sprintf("%d microseconds", opts.window.Microseconds())
	}

	if opts.quota > 0 {
		quota = opts.quota
	}

	if _, err := f.pool.Exec(f.t.Context(),
		`UPDATE cache_backends SET retention_window = $2::interval, quota_bytes = $3 WHERE id = $1`,
		row.ID, window, quota,
	); err != nil {
		f.t.Fatalf("set retention/quota: %v", err)
	}

	return row.ID
}

func (f *fixture) ref(backendID int64, namespace, kind, key string) blob.Ref {
	return blob.Ref{
		BackendID: backendID,
		Org:       f.orgSlug,
		Project:   f.projectSlug,
		Backend:   metrics.BackendSstate,
		Kind:      kind,
		Namespace: namespace,
		Key:       key,
	}
}

// put writes one object and returns its digest.
func (f *fixture) put(backendID int64, namespace, key, content string) blob.Digest {
	f.t.Helper()

	res, err := f.blobs.Put(f.t.Context(), f.ref(backendID, namespace, "object", key),
		bytes.NewReader([]byte(content)),
		blob.PutOptions{Overwrite: false, Verify: blob.NoVerify(), ContentType: ""})
	if err != nil {
		f.t.Fatalf("Put(%q) error = %v", key, err)
	}

	return res.Digest
}

// age rewrites the timestamps the sweep reads. created_at is backdated but live_xid
// is NOT touched -- it is a DEFAULT set at INSERT, so the row stays visible in every
// later snapshot, which is exactly the state a genuinely old row is in.
//
// accessed is optional: the zero time writes NULL, which is what every row in the
// database looks like the moment 000012 lands.
func (f *fixture) age(backendID int64, namespace, key string, created, accessed time.Time) {
	f.t.Helper()

	var acc any
	if !accessed.IsZero() {
		acc = accessed
	}

	tag, err := f.pool.Exec(f.t.Context(),
		`UPDATE cache_objects SET created_at = $4, updated_at = $4, accessed_at = $5
		  WHERE backend_id = $1 AND namespace = $2 AND key = $3`,
		backendID, namespace, key, created, acc)
	if err != nil {
		f.t.Fatalf("age(%q): %v", key, err)
	}

	if tag.RowsAffected() != 1 {
		f.t.Fatalf("age(%q) touched %d rows, want 1", key, tag.RowsAffected())
	}
}

// keys lists the surviving keys of one namespace.
func (f *fixture) keys(backendID int64, namespace string) []string {
	f.t.Helper()

	rows, err := f.pool.Query(f.t.Context(),
		`SELECT key FROM cache_objects WHERE backend_id = $1 AND namespace = $2 ORDER BY key`,
		backendID, namespace)
	if err != nil {
		f.t.Fatalf("list keys: %v", err)
	}

	defer rows.Close()

	out := []string{}

	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			f.t.Fatalf("scan key: %v", err)
		}

		out = append(out, k)
	}

	if err := rows.Err(); err != nil {
		f.t.Fatalf("list keys: %v", err)
	}

	return out
}

func (f *fixture) exists(backendID int64, namespace, key string) bool {
	f.t.Helper()

	var n int64
	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM cache_objects WHERE backend_id = $1 AND namespace = $2 AND key = $3`,
		backendID, namespace, key).Scan(&n); err != nil {
		f.t.Fatalf("exists(%q): %v", key, err)
	}

	return n > 0
}

// refreshed rewrites updated_at alone, which is the ONLY column the tag stage reads:
// a tag's freshness is maintained by the stale-while-revalidate touch, and a tag
// deliberately never traverses the LRU, so its accessed_at is always NULL. A test
// that moved created_at instead would pass for the wrong reason.
func (f *fixture) refreshed(backendID int64, namespace, key string, updated time.Time) {
	f.t.Helper()

	tag, err := f.pool.Exec(f.t.Context(),
		`UPDATE cache_objects SET updated_at = $4
		  WHERE backend_id = $1 AND namespace = $2 AND key = $3`,
		backendID, namespace, key, updated)
	if err != nil {
		f.t.Fatalf("refreshed(%q): %v", key, err)
	}

	if tag.RowsAffected() != 1 {
		f.t.Fatalf("refreshed(%q) touched %d rows, want 1", key, tag.RowsAffected())
	}
}

// digestOf reads back the blob digest an object row names. The tag/manifest
// anti-join is a comparison of exactly this column.
func (f *fixture) digestOf(backendID int64, namespace, key string) blob.Digest {
	f.t.Helper()

	var raw []byte

	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT digest FROM cache_objects WHERE backend_id = $1 AND namespace = $2 AND key = $3`,
		backendID, namespace, key).Scan(&raw); err != nil {
		f.t.Fatalf("digestOf(%q): %v", key, err)
	}

	var d blob.Digest

	copy(d[:], raw)

	return d
}

func (f *fixture) blobState(digest blob.Digest) string {
	f.t.Helper()

	var state string

	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT state::text FROM blobs WHERE digest = $1`, digest.Bytes()).Scan(&state); err != nil {
		f.t.Fatalf("blob state: %v", err)
	}

	return state
}

// markPendingDelete forges the tombstone a process that died between Layer B's mark
// and its unlink would have left behind.
func (f *fixture) markPendingDelete(digest blob.Digest) {
	f.t.Helper()

	if _, err := f.pool.Exec(f.t.Context(),
		`UPDATE blobs SET state = 'pending_delete', delete_started_at = now() WHERE digest = $1`,
		digest.Bytes()); err != nil {
		f.t.Fatalf("mark pending delete: %v", err)
	}
}

// unihash inserts one hashserv row and backdates it, so a test can put the GC root
// on either side of its window.
func (f *fixture) unihash(backendID int64, taskhash, unihash string, created time.Time) {
	f.t.Helper()

	if _, err := f.store.InsertUnihash(f.t.Context(), repository.InsertUnihashParams{
		BackendID: backendID, Method: "oe.sstatesig.OEOuthashBasic",
		Taskhash: taskhash, Unihash: unihash,
	}); err != nil {
		f.t.Fatalf("InsertUnihash: %v", err)
	}

	if created.IsZero() {
		return
	}

	if _, err := f.pool.Exec(f.t.Context(),
		`UPDATE hashserv_unihashes SET created_at = $2 WHERE backend_id = $1 AND taskhash = $3`,
		backendID, created, taskhash); err != nil {
		f.t.Fatalf("age unihash: %v", err)
	}
}

// unihashExists reports whether one hashserv row survived the sweep.
func (f *fixture) unihashExists(backendID int64, taskhash string) bool {
	f.t.Helper()

	var n int64

	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT count(*) FROM hashserv_unihashes WHERE backend_id = $1 AND taskhash = $2`,
		backendID, taskhash).Scan(&n); err != nil {
		f.t.Fatalf("unihashExists(%q): %v", taskhash, err)
	}

	return n > 0
}

// unihashAccessedAt reads one hashserv row's accessed_at. An invalid (NULL) value
// is what a mark that is still only in this process's memory looks like from the
// sweep's side.
func (f *fixture) unihashAccessedAt(backendID int64, taskhash string) pgtype.Timestamptz {
	f.t.Helper()

	var at pgtype.Timestamptz

	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT accessed_at FROM hashserv_unihashes WHERE backend_id = $1 AND taskhash = $2`,
		backendID, taskhash).Scan(&at); err != nil {
		f.t.Fatalf("unihashAccessedAt(%q): %v", taskhash, err)
	}

	return at
}

// run executes one real sweep and fails the test if it did not reach a terminal
// success.
func (f *fixture) run() Summary {
	f.t.Helper()

	sum, err := f.eng.Run(f.t.Context(), TriggerInterval, false)
	if err != nil {
		f.t.Fatalf("Run() error = %v", err)
	}

	// Every caller gets this for free: a run that returned nil but left its row
	// 'running' still holds gc_runs' active slot, and the next real run would collide
	// with it rather than report anything.
	if status, msg := f.runRow(sum.RunID); status != "succeeded" {
		f.t.Fatalf("gc_runs %d ended %q (%s), want succeeded", sum.RunID, status, msg)
	}

	return sum
}

// runRow reads back a gc_runs row.
func (f *fixture) runRow(id int64) (status, errMsg string) {
	f.t.Helper()

	var msg pgtype.Text

	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT status::text, error FROM gc_runs WHERE id = $1`, id).Scan(&status, &msg); err != nil {
		f.t.Fatalf("read gc_runs %d: %v", id, err)
	}

	return status, msg.String
}

// ago is shorthand for "this many days before now", which is how every window in
// this package is expressed.
func ago(days int) time.Time { return time.Now().Add(-time.Duration(days) * 24 * time.Hour) }

func day(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// storageObjects reads bakery_storage_objects for one backend off the real
// registry, or -1 when the series is absent. Absence is the assertion in P: R6#8 --
// a backend that vanishes from the gauges is exactly the failure -- so it has to be
// distinguishable from a genuine zero.
func (f *fixture) storageObjects(backend metrics.Backend) float64 {
	f.t.Helper()

	families, err := f.mx.Registry().Gather()
	if err != nil {
		f.t.Fatalf("gather metrics: %v", err)
	}

	for _, fam := range families {
		if fam.GetName() != "bakery_storage_objects" {
			continue
		}

		for _, metric := range fam.GetMetric() {
			match := 0

			for _, label := range metric.GetLabel() {
				switch {
				case label.GetName() == "org" && label.GetValue() == f.orgSlug,
					label.GetName() == "project" && label.GetValue() == f.projectSlug,
					label.GetName() == "backend" && label.GetValue() == string(backend):
					match++
				}
			}

			if match == 3 {
				return metric.GetGauge().GetValue()
			}
		}
	}

	return -1
}

// usageRow reads a backend's recorded measurement.
func (f *fixture) usageRow(backendID int64) (objects, bytes int64) {
	f.t.Helper()

	if err := f.pool.QueryRow(f.t.Context(),
		`SELECT objects_count, logical_bytes FROM cache_backend_usage WHERE backend_id = $1`,
		backendID).Scan(&objects, &bytes); err != nil {
		f.t.Fatalf("usage row for backend %d: %v", backendID, err)
	}

	return objects, bytes
}
