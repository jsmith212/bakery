package storage

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jsmith212/bakery/internal/metrics"
)

// Storage operation labels. A CLOSED set, by construction: these five constants are
// the only values that ever reach the `op` label, so bakery_storage_operations_total
// has at most driver x 5 x result series no matter what a client sends.
const (
	opGet    = "get"
	opPut    = "put"
	opStat   = "stat"
	opExists = "exists"
	opDelete = "delete"
)

// Instrumented decorates a Store with the storage metrics.
//
// A DECORATOR, not a field on Local, because the S3 store that lands later gets the
// same series for free and because a test can measure a bare Local without a
// registry. `driver` is the metrics.Driver* constant of the wrapped store.
type Instrumented struct {
	inner  Store
	driver string

	ops     *prometheus.CounterVec // curried on {driver}
	latency prometheus.ObserverVec // curried on {driver}
}

var _ Store = (*Instrumented)(nil)

// NewInstrumented wraps inner. driver is metrics.DriverLocal today; S3 is deferred.
func NewInstrumented(inner Store, m *metrics.Metrics, driver string) *Instrumented {
	return &Instrumented{
		inner:   inner,
		driver:  driver,
		ops:     m.StorageOps.MustCurryWith(prometheus.Labels{"driver": driver}),
		latency: m.StorageLatency.MustCurryWith(prometheus.Labels{"driver": driver}),
	}
}

// observe records one storage operation.
//
// The RESULT MAPPING IS THE POINT, and it is not the obvious one: ErrNotFound is
// `miss`, not `error`. A cold cache is nothing but misses -- counting them as
// errors would make the error rate of a healthy first build 100% and make the alert
// on it worthless.
func (i *Instrumented) observe(op string, start time.Time, err error) {
	res := metrics.ResultHit

	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		res = metrics.ResultMiss
	default:
		res = metrics.ResultError
	}

	i.ops.WithLabelValues(op, string(res)).Inc()
	i.latency.WithLabelValues(op).Observe(time.Since(start).Seconds())
}

// Create is not itself timed: the interesting duration is the Commit, which is where
// the fsyncs are. The returned Writer is wrapped so Commit reports as a `put`.
func (i *Instrumented) Create(ctx context.Context) (Writer, error) {
	w, err := i.inner.Create(ctx)
	if err != nil {
		i.observe(opPut, time.Now(), err)

		return nil, err
	}

	return &instrumentedWriter{Writer: w, parent: i, start: time.Now()}, nil
}

func (i *Instrumented) Get(ctx context.Context, k Key) (io.ReadCloser, error) {
	start := time.Now()

	rc, err := i.inner.Get(ctx, k)

	// Measures time-to-first-byte, NOT the transfer: the body is streamed to the
	// client afterwards and its duration is the client's link speed, not ours. A
	// histogram that included it would measure the network and be useless for
	// spotting a slow disk.
	i.observe(opGet, start, err)

	return rc, err
}

func (i *Instrumented) Stat(ctx context.Context, k Key) (Info, error) {
	start := time.Now()

	info, err := i.inner.Stat(ctx, k)
	i.observe(opStat, start, err)

	return info, err
}

func (i *Instrumented) Exists(ctx context.Context, k Key) (bool, error) {
	start := time.Now()

	ok, err := i.inner.Exists(ctx, k)

	res := err
	if err == nil && !ok {
		res = ErrNotFound // an absent object is a miss, not a success
	}

	i.observe(opExists, start, res)

	return ok, err
}

func (i *Instrumented) Delete(ctx context.Context, k Key) error {
	start := time.Now()

	err := i.inner.Delete(ctx, k)
	i.observe(opDelete, start, err)

	return err
}

// instrumentedWriter times the whole staged write, from Create to Commit -- which is
// what a "storage put" costs, fsyncs included. An Abort records nothing: dedup
// elided the write, and blob.Service already counts that as a put/hit on the
// headline series.
type instrumentedWriter struct {
	Writer

	parent *Instrumented
	start  time.Time
}

func (w *instrumentedWriter) Commit(ctx context.Context) (Info, error) {
	info, err := w.Writer.Commit(ctx)
	w.parent.observe(opPut, w.start, err)

	return info, err
}
