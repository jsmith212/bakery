package storage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/jsmith212/bakery/internal/metrics"
)

// runConformance is the Store-interface-only contract: everything a caller of
// blob.Service may assume of ANY driver, proven once here and run against
// every one of them (see internal/storage/local_test.go's TestLocal_Conformance
// for the Local instance, and the future S3 driver's own test file for the
// minio instance -- feedback wave 1, spec section 9's prerequisite).
//
// What is deliberately NOT here, and stays in a driver's own test file
// instead: fan-out layout, staging-dir listing, and "nothing is observable at
// the object's key before Commit returns" are LOCAL's own on-disk
// implementation choices, not the interface's contract. S3 publishes at
// Sync, not Commit (storage.go's Writer doc carries the amended, driver-aware
// property) -- asserting Local's stronger guarantee here would fail a
// correctly-built S3 driver for being more permissive in a documented way.
func runConformance(t *testing.T, newStore func(t *testing.T) Store, driver string) {
	t.Helper()

	t.Run("RoundTrip", func(t *testing.T) { conformanceRoundTrip(t, newStore) })
	t.Run("MissesAreErrNotFound", func(t *testing.T) { conformanceMissesAreErrNotFound(t, newStore) })
	t.Run("Delete", func(t *testing.T) { conformanceDelete(t, newStore) })
	t.Run("AbortLeavesNoObject", func(t *testing.T) { conformanceAbortLeavesNoObject(t, newStore) })
	t.Run("AbortAfterCommitIsANoop", func(t *testing.T) { conformanceAbortAfterCommitIsANoop(t, newStore) })
	t.Run("CommitIsIdempotentAcrossWriters", func(t *testing.T) {
		conformanceCommitIsIdempotentAcrossWriters(t, newStore)
	})
	t.Run("DigestBeforeCommit", func(t *testing.T) { conformanceDigestBeforeCommit(t, newStore) })
	t.Run("CommittedObjectReadsBackWhole", func(t *testing.T) {
		conformanceCommittedObjectReadsBackWhole(t, newStore)
	})
	t.Run("Instrumented_EmitsStorageMetrics", func(t *testing.T) {
		conformanceInstrumentedEmitsStorageMetrics(t, newStore, driver)
	})
	t.Run("Instrumented_AbortRecordsNoPut", func(t *testing.T) {
		conformanceInstrumentedAbortRecordsNoPut(t, newStore, driver)
	})
	t.Run("Instrumented_PreRegistersSeriesAtZero", func(t *testing.T) {
		conformanceInstrumentedPreRegistersSeriesAtZero(t, newStore, driver)
	})
}

func conformanceRoundTrip(t *testing.T, newStore func(t *testing.T) Store) {
	tests := []struct {
		name    string
		content []byte
	}{
		// The REAPI empty blob. It MUST round-trip: every Bazel client asks for it
		// and a store that treats zero bytes as "absent" breaks all of them.
		{name: "empty", content: []byte{}},
		{name: "small", content: []byte("sstate:busybox")},
		{name: "binary", content: bytes.Repeat([]byte{0x00, 0xff, 0x7f}, 4096)},
		{name: "multi chunk", content: bytes.Repeat([]byte("a"), 1<<20)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStore(t)
			k := put(t, s, tt.content)

			ok, err := s.Exists(t.Context(), k)
			if err != nil || !ok {
				t.Fatalf("Exists() = %v, %v; want true, nil", ok, err)
			}

			info, err := s.Stat(t.Context(), k)
			if err != nil {
				t.Fatalf("Stat() error = %v", err)
			}

			if info.Size != int64(len(tt.content)) {
				t.Errorf("Stat() size = %d, want %d", info.Size, len(tt.content))
			}

			rc, err := s.Get(t.Context(), k)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}

			got, err := io.ReadAll(rc)
			_ = rc.Close()

			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}

			if !bytes.Equal(got, tt.content) {
				t.Errorf("Get() returned %d bytes, want %d", len(got), len(tt.content))
			}
		})
	}
}

func conformanceMissesAreErrNotFound(t *testing.T, newStore func(t *testing.T) Store) {
	s := newStore(t)
	k := KeyOf([]byte("never written"))

	if _, err := s.Get(t.Context(), k); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}

	if _, err := s.Stat(t.Context(), k); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat() error = %v, want ErrNotFound", err)
	}

	ok, err := s.Exists(t.Context(), k)
	if ok || err != nil {
		t.Errorf("Exists() = %v, %v; want false, nil", ok, err)
	}

	// Idempotent: the GC re-drives its queue after a crash, and the second unlink
	// must not be an error.
	if err := s.Delete(t.Context(), k); err != nil {
		t.Errorf("Delete(absent) error = %v, want nil", err)
	}
}

func conformanceDelete(t *testing.T, newStore func(t *testing.T) Store) {
	s := newStore(t)
	k := put(t, s, []byte("bytes"))

	if err := s.Delete(t.Context(), k); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	ok, err := s.Exists(t.Context(), k)
	if ok || err != nil {
		t.Errorf("after Delete, Exists() = %v, %v; want false, nil", ok, err)
	}
}

// A writer that never Committed must leave no object reachable at its key --
// the driver-generic half of "AbortLeavesNothing". Whether staging state
// survives on disk is a Local implementation detail (see
// TestLocal_AbortLeavesNoStagingFiles); what every driver owes the caller is
// that the key resolves to nothing.
func conformanceAbortLeavesNoObject(t *testing.T, newStore func(t *testing.T) Store) {
	s := newStore(t)

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := w.Write([]byte("discard me")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}

	// Abort is idempotent -- `defer w.Abort()` alongside an explicit Abort must not
	// be an error.
	if err := w.Abort(); err != nil {
		t.Fatalf("second Abort() error = %v", err)
	}

	if ok, err := s.Exists(t.Context(), KeyOf([]byte("discard me"))); ok || err != nil {
		t.Errorf("Exists() = %v, %v; want false, nil", ok, err)
	}
}

// Abort after Commit is a no-op, which is what makes `defer w.Abort()` safe on the
// happy path. If it removed the object, every successful PUT would delete its own
// bytes and every subsequent GET would be a permanent 500.
func conformanceAbortAfterCommitIsANoop(t *testing.T, newStore func(t *testing.T) Store) {
	s := newStore(t)

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := w.Write([]byte("keep me")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	info, err := w.Commit(t.Context())
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if err := w.Abort(); err != nil {
		t.Errorf("Abort() after Commit error = %v, want nil", err)
	}

	if ok, err := s.Exists(t.Context(), info.Key); !ok || err != nil {
		t.Errorf("Exists() after post-Commit Abort = %v, %v; want true, nil", ok, err)
	}

	if _, err := w.Commit(t.Context()); !errors.Is(err, ErrCommitted) {
		t.Errorf("second Commit() error = %v, want ErrCommitted", err)
	}
}

// Two writers staging identical content must both succeed and land on one object.
// This is the dedup path at the byte layer, and it is why Commit onto an existing
// object is a no-op rather than an error.
func conformanceCommitIsIdempotentAcrossWriters(t *testing.T, newStore func(t *testing.T) Store) {
	s := newStore(t)

	k1 := put(t, s, []byte("identical"))
	k2 := put(t, s, []byte("identical"))

	if k1 != k2 {
		t.Fatalf("keys differ: %s vs %s", k1, k2)
	}

	info, err := s.Stat(t.Context(), k1)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}

	if info.Size != int64(len("identical")) {
		t.Errorf("size = %d, want %d", info.Size, len("identical"))
	}
}

// Digest must be available BEFORE Commit -- that is the seam blob.Service uses to
// take the digest advisory lock and decide whether to commit these bytes at all.
func conformanceDigestBeforeCommit(t *testing.T, newStore func(t *testing.T) Store) {
	s := newStore(t)
	content := []byte("hash me")

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	defer func() { _ = w.Abort() }()

	if _, err := io.Copy(w, bytes.NewReader(content)); err != nil {
		t.Fatalf("Copy() error = %v", err)
	}

	k, n := w.Digest()
	if k != KeyOf(content) {
		t.Errorf("Digest() = %s, want %s", k, KeyOf(content))
	}

	if n != int64(len(content)) {
		t.Errorf("Digest() size = %d, want %d", n, len(content))
	}
}

// The interface-safe rewrite of "torn file is never observable": rather than
// inspecting a driver's own on-disk staging area (Local-specific, and
// meaningless for S3), this proves the CONTRACT every driver actually owes --
// once Commit returns, Get reads back the full, exact bytes, never a partial
// or truncated object. A multi-chunk write is used deliberately, so a driver
// that publishes a prefix of the stream before the last chunk lands would be
// caught here.
func conformanceCommittedObjectReadsBackWhole(t *testing.T, newStore func(t *testing.T) Store) {
	s := newStore(t)
	content := bytes.Repeat([]byte("z"), 1<<20)
	k := put(t, s, content)

	rc, err := s.Get(t.Context(), k)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	got, err := io.ReadAll(rc)
	_ = rc.Close()

	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Fatalf("object read back %d bytes, want the full %d -- a torn/partial "+
			"object must never be readable", len(got), len(content))
	}

	if ok, err := s.Exists(t.Context(), k); !ok || err != nil {
		t.Errorf("Exists() after Commit = %v, %v; want true, nil", ok, err)
	}
}

func conformanceInstrumentedEmitsStorageMetrics(t *testing.T, newStore func(t *testing.T) Store, driver string) {
	m := metrics.New()
	s := NewInstrumented(newStore(t), m, driver)

	k := put(t, s, []byte("instrumented"))

	rc, err := s.Get(t.Context(), k)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	_ = rc.Close()

	if _, err := s.Get(t.Context(), KeyOf([]byte("absent"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(absent) error = %v, want ErrNotFound", err)
	}

	if err := s.Delete(t.Context(), k); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	tests := []struct {
		op, result string
		want       float64
	}{
		{op: opPut, result: string(metrics.ResultHit), want: 1},
		{op: opGet, result: string(metrics.ResultHit), want: 1},
		// A cold cache is nothing but misses. ErrNotFound MUST NOT count as an
		// error, or a healthy first build reports a 100% storage error rate.
		{op: opGet, result: string(metrics.ResultMiss), want: 1},
		{op: opGet, result: string(metrics.ResultError), want: 0},
		{op: opDelete, result: string(metrics.ResultHit), want: 1},
	}

	for _, tt := range tests {
		got := counterValue(t, m, "bakery_storage_operations_total", map[string]string{
			"driver": driver, "op": tt.op, "result": tt.result,
		})
		if got != tt.want {
			t.Errorf("storage_operations_total{op=%q,result=%q} = %v, want %v", tt.op, tt.result, got, tt.want)
		}
	}
}

// An aborted write records NO put: dedup elided it, and blob.Service already counts
// that on the headline series as put/hit. Counting it here too would double-count
// every deduped upload.
func conformanceInstrumentedAbortRecordsNoPut(t *testing.T, newStore func(t *testing.T) Store, driver string) {
	m := metrics.New()
	s := NewInstrumented(newStore(t), m, driver)

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := w.Write([]byte("elided")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}

	got := counterValue(t, m, "bakery_storage_operations_total", map[string]string{
		"driver": driver, "op": opPut, "result": string(metrics.ResultHit),
	})
	if got != 0 {
		t.Errorf("aborted write recorded %v puts, want 0", got)
	}
}

// conformanceInstrumentedPreRegistersSeriesAtZero proves the store's operation series
// EXIST the moment it is constructed, before any call. This is the storage half of
// the "STORAGE_DIR is not dead config" guarantee: bakery_storage_operations_total
// must be present from boot so a rate() alert can distinguish "no storage traffic
// yet" from "the store was never wired up at all". counterValue reads zero for both
// an absent series and a present-but-zero one, so this test counts series instead.
func conformanceInstrumentedPreRegistersSeriesAtZero(t *testing.T, newStore func(t *testing.T) Store, driver string) {
	m := metrics.New()

	// Construct and discard: the series live in the registry, not on the store.
	_ = NewInstrumented(newStore(t), m, driver)

	ops := []string{opGet, opPut, opStat, opExists, opDelete}
	results := []string{string(metrics.ResultHit), string(metrics.ResultMiss), string(metrics.ResultError)}

	want := len(ops) * len(results)

	got := seriesCount(t, m, "bakery_storage_operations_total")
	if got != want {
		t.Fatalf("bakery_storage_operations_total has %d series after construction, want %d "+
			"(op x result, pre-registered at zero) -- the store's series were not initialized at boot", got, want)
	}

	// Every one of them must be exactly zero: pre-registration seeds the labels,
	// it must never fabricate traffic.
	for _, op := range ops {
		for _, res := range results {
			if v := counterValue(t, m, "bakery_storage_operations_total", map[string]string{
				"driver": driver, "op": op, "result": res,
			}); v != 0 {
				t.Errorf("pre-registered series {op=%q,result=%q} = %v, want 0", op, res, v)
			}
		}
	}
}

// put is the whole write protocol in one line, for tests that are not about the
// protocol. Shared with local_test.go's Local-only tests.
func put(t *testing.T, s Store, content []byte) Key {
	t.Helper()

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	defer func() { _ = w.Abort() }()

	if _, err := w.Write(content); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	info, err := w.Commit(t.Context())
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if want := sha256.Sum256(content); info.Key != want {
		t.Fatalf("Commit() key = %s, want %s", info.Key, Key(want))
	}

	if info.Size != int64(len(content)) {
		t.Errorf("Commit() size = %d, want %d", info.Size, len(content))
	}

	return info.Key
}
