package blob

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
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

	err error
}

func newFakeReader() *fakeReader {
	return &fakeReader{queries: atomic.Int64{}, rows: map[string]repository.StatObjectRow{}, latency: 0, err: nil}
}

func (f *fakeReader) StatObject(
	_ context.Context, arg repository.StatObjectParams,
) (repository.StatObjectRow, error) {
	f.queries.Add(1)

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

// add seeds one object.
func (f *fakeReader) add(key string, digest []byte, size int64) {
	f.rows[key] = repository.StatObjectRow{
		Digest:    digest,
		SizeBytes: size,
		UpdatedAt: pgtype.Timestamptz{Time: time.Unix(0, 0), InfinityModifier: 0, Valid: true},
	}
}
