package blob

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// fakeReader is the hand-written metadata fake, and its ONLY interesting property is
// that it COUNTS QUERIES. The HEAD-path gates assert on that count -- "an LRU hit
// that still touches Postgres" is exactly the failure they exist to catch, and it is
// invisible to any test that only asserts on the returned value.
//
// stdlib only, no gomock: matches the repo convention, and a counter and a map are
// all this needs.
type fakeReader struct {
	// queries counts StatObject calls. Atomic: the gates hammer it from 64
	// goroutines under -race.
	queries atomic.Int64

	// rows is the pretend cache_objects table, keyed by object key. Written before
	// the goroutines start and read-only afterwards, so no lock.
	rows map[string]repository.StatObjectRow

	// latency is fake Postgres round-trip time. THE SINGLEFLIGHT GATE NEEDS THIS: with
	// a zero-latency fake the race window is too small to observe and the test passes
	// vacuously even with singleflight deleted -- a gate that cannot fail is
	// decoration.
	latency time.Duration

	// entered and gate give a test DETERMINISTIC control over the probe instead of
	// relying on a sleep. When entered is non-nil, StatObject sends on it as it begins
	// (so a test can wait until the flight is genuinely in progress). When gate is
	// non-nil, StatObject blocks until the gate is closed OR the context is cancelled
	// -- the latter is what lets the singleflight-cancellation test observe whether a
	// caller's disconnect propagates to the shared probe.
	entered chan struct{}
	gate    chan struct{}

	err error
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		queries: atomic.Int64{},
		rows:    map[string]repository.StatObjectRow{},
		latency: 0,
		entered: nil,
		gate:    nil,
		err:     nil,
	}
}

func (f *fakeReader) StatObject(
	ctx context.Context, arg repository.StatObjectParams,
) (repository.StatObjectRow, error) {
	f.queries.Add(1)

	if f.entered != nil {
		f.entered <- struct{}{}
	}

	if f.gate != nil {
		// Honour the context here, exactly as a real Postgres round-trip does: this is
		// what makes the singleflight-cancellation gate able to fail. If the probe rode
		// a caller's context, cancelling that caller would return ctx.Err() from here
		// and singleflight would hand it to every waiter.
		select {
		case <-f.gate:
		case <-ctx.Done():
			return repository.StatObjectRow{}, ctx.Err()
		}
	}

	if f.latency > 0 {
		time.Sleep(f.latency)
	}

	if f.err != nil {
		return repository.StatObjectRow{}, f.err
	}

	row, ok := f.rows[arg.Key]
	if !ok {
		return repository.StatObjectRow{}, pgx.ErrNoRows
	}

	return row, nil
}

// StatObjectsBatch is the ExistsBatch probe. It counts as ONE query no matter how
// many keys it is handed -- that is the whole point of the batch, and counting per
// key would silently defeat the db/batch gates. It returns only the keys that exist,
// exactly as the SQL does.
func (f *fakeReader) StatObjectsBatch(
	ctx context.Context, arg repository.StatObjectsBatchParams,
) ([]repository.StatObjectsBatchRow, error) {
	f.queries.Add(1)

	if f.entered != nil {
		f.entered <- struct{}{}
	}

	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if f.latency > 0 {
		time.Sleep(f.latency)
	}

	if f.err != nil {
		return nil, f.err
	}

	out := make([]repository.StatObjectsBatchRow, 0, len(arg.Keys))

	for _, k := range arg.Keys {
		row, ok := f.rows[k]
		if !ok {
			continue
		}

		out = append(out, repository.StatObjectsBatchRow{
			Key:       k,
			Digest:    row.Digest,
			SizeBytes: row.SizeBytes,
			UpdatedAt: row.UpdatedAt,
		})
	}

	return out, nil
}

// ListObjectKeysByPrefix mirrors the SQL: sorted keys under a LIKE prefix pattern.
// It counts as one query. The pattern is unescaped back to a literal prefix here;
// real LIKE semantics (the escaping this fake undoes) are asserted against Postgres
// in TestListKeysByPrefix_DB.
func (f *fakeReader) ListObjectKeysByPrefix(
	_ context.Context, arg repository.ListObjectKeysByPrefixParams,
) ([]string, error) {
	f.queries.Add(1)

	if f.err != nil {
		return nil, f.err
	}

	prefix := unLikePattern(arg.Prefix)

	var out []string

	for k := range f.rows {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}

	sort.Strings(out)

	return out, nil
}

// unLikePattern turns the Service's escaped LIKE pattern back into the literal
// prefix it encodes: strip the trailing %, undo the metacharacter escapes. A single
// left-to-right pass, because `\\` followed by `\%` must not collapse into `\%`.
func unLikePattern(pattern string) string {
	prefix := strings.TrimSuffix(pattern, "%")

	var b strings.Builder

	for i := 0; i < len(prefix); i++ {
		if prefix[i] == '\\' && i+1 < len(prefix) {
			i++
		}

		b.WriteByte(prefix[i])
	}

	return b.String()
}

// add seeds one object.
func (f *fakeReader) add(key string, digest []byte, size int64) {
	f.rows[key] = repository.StatObjectRow{
		Digest:    digest,
		SizeBytes: size,
		UpdatedAt: pgtype.Timestamptz{Time: time.Unix(0, 0), InfinityModifier: 0, Valid: true},
	}
}

// fakeTxer is the WRITE half against a fake DBTX, which means the tests below run the
// REAL generated queries -- the same SQL text, the same argument order sqlc built --
// without a Postgres. What is being asserted is what the Service issues and how often
// (one UPDATE for N reads; one DELETE, retried on 40P01), which is a property of this
// package, not of the database. Everything that is a property of the DATABASE (the
// digest ordering actually preventing a deadlock, the refcount trigger) is asserted
// against a real Postgres instead.
type fakeTxer struct{ db *fakeDBTX }

func (f *fakeTxer) Tx(ctx context.Context, fn func(*repository.Queries) error) error {
	return fn(repository.New(f.db))
}

// fakeExec is one recorded statement.
type fakeExec struct {
	sql  string
	args []any

	// ctxErr is the context's error AS SEEN INSIDE Exec. The shutdown-flush gate reads
	// it: the final flush must run under context.WithoutCancel, so a nil here is the
	// assertion that the already-cancelled shutdown context did not travel into the
	// write.
	ctxErr error
}

type fakeDBTX struct {
	mu    sync.Mutex
	execs []fakeExec

	// errs is a queue of per-call outcomes, popped from the front. A non-nil entry is
	// returned instead of running the statement -- that is how the deadlock retry is
	// driven without a second connection.
	errs []error

	// reader, when set, is mutated by a DeleteObjectsByKeys so a follow-up Exists
	// really misses.
	reader *fakeReader

	// onExec runs INSIDE the statement, which is what lets a test observe the world
	// while a flush is in flight -- the window in which the pending-touch veto must
	// still answer true.
	onExec func(sql string)
}

func (f *fakeDBTX) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.execs = append(f.execs, fakeExec{sql: sql, args: args, ctxErr: ctx.Err()})

	if f.onExec != nil {
		f.onExec(sql)
	}

	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]

		if err != nil {
			return pgconn.CommandTag{}, err
		}
	}

	switch {
	case strings.Contains(sql, "name: DeleteObjectsByKeys"):
		keys, _ := args[0].([]string)

		if f.reader != nil {
			for _, k := range keys {
				delete(f.reader.rows, k)
			}
		}

		return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", len(keys))), nil

	case strings.Contains(sql, "name: TouchObjectsAccessed"):
		keys, _ := args[2].([]string)

		return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", len(keys))), nil
	}

	return pgconn.NewCommandTag("SELECT 0"), nil
}

func (f *fakeDBTX) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeDBTX: Query is not implemented")
}

func (f *fakeDBTX) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	return errRow{}
}

type errRow struct{}

func (errRow) Scan(...any) error { return errors.New("fakeDBTX: QueryRow is not implemented") }

// calls returns the recorded statements matching name.
func (f *fakeDBTX) calls(name string) []fakeExec {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []fakeExec

	for _, e := range f.execs {
		if strings.Contains(e.sql, "name: "+name) {
			out = append(out, e)
		}
	}

	return out
}
