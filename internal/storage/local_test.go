package storage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/metrics"
)

func newTestStore(t *testing.T) *Local {
	t.Helper()

	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	return s
}

// put is the whole write protocol in one line, for tests that are not about the
// protocol.
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

func TestLocal_RoundTrip(t *testing.T) {
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
			s := newTestStore(t)
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

func TestLocal_MissesAreErrNotFound(t *testing.T) {
	s := newTestStore(t)
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

func TestLocal_Delete(t *testing.T) {
	s := newTestStore(t)
	k := put(t, s, []byte("bytes"))

	if err := s.Delete(t.Context(), k); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	ok, err := s.Exists(t.Context(), k)
	if ok || err != nil {
		t.Errorf("after Delete, Exists() = %v, %v; want false, nil", ok, err)
	}
}

// The atomicity claim, tested by looking at the filesystem rather than by trusting
// rename(2): while a write is staged, NOTHING is observable at the object's key,
// and the partial bytes live outside objects/ entirely.
func TestLocal_TornFileIsNeverObservable(t *testing.T) {
	s := newTestStore(t)
	content := bytes.Repeat([]byte("z"), 1<<16)
	k := KeyOf(content)

	w, err := s.Create(t.Context())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := w.Write(content[:1024]); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	ok, err := s.Exists(t.Context(), k)
	if ok || err != nil {
		t.Fatalf("mid-write Exists() = %v, %v; want false, nil", ok, err)
	}

	// The partial bytes are in staging/, never under objects/.
	var found []string

	root := filepath.Join(s.Root(), objectsDir)

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			found = append(found, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk objects/: %v", err)
	}

	if len(found) != 0 {
		t.Errorf("mid-write objects/ contains %v, want nothing", found)
	}

	if _, err := w.Write(content[1024:]); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if _, err := w.Commit(t.Context()); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	if ok, err := s.Exists(t.Context(), k); !ok || err != nil {
		t.Errorf("after Commit, Exists() = %v, %v; want true, nil", ok, err)
	}
}

func TestLocal_AbortLeavesNothing(t *testing.T) {
	s := newTestStore(t)

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

	entries, err := os.ReadDir(filepath.Join(s.Root(), stagingDir))
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("staging/ holds %d files after Abort, want 0", len(entries))
	}
}

// Abort after Commit is a no-op, which is what makes `defer w.Abort()` safe on the
// happy path. If it removed the object, every successful PUT would delete its own
// bytes and every subsequent GET would be a permanent 500.
func TestLocal_AbortAfterCommitIsANoop(t *testing.T) {
	s := newTestStore(t)

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
func TestLocal_CommitIsIdempotentAcrossWriters(t *testing.T) {
	s := newTestStore(t)

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

func TestLocal_FanOutLayout(t *testing.T) {
	s := newTestStore(t)
	k := put(t, s, []byte("layout"))
	h := k.String()

	want := filepath.Join(s.Root(), objectsDir, h[0:2], h[2:4], h)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("object not at %s: %v", want, err)
	}
}

// Digest must be available BEFORE Commit -- that is the seam blob.Service uses to
// take the digest advisory lock and decide whether to commit these bytes at all.
func TestLocal_DigestBeforeCommit(t *testing.T) {
	s := newTestStore(t)
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

	// ...and nothing is durable yet.
	if ok, _ := s.Exists(t.Context(), k); ok {
		t.Error("Digest() made the object visible; only Commit may do that")
	}
}

func TestParseKey(t *testing.T) {
	valid := strings.Repeat("ab", KeySize)

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "valid", in: valid, wantErr: false},
		{name: "too short", in: valid[:62], wantErr: true},
		{name: "too long", in: valid + "cd", wantErr: true},
		{name: "not hex", in: strings.Repeat("zz", KeySize), wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := ParseKey(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseKey(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}

			if tt.wantErr {
				if !errors.Is(err, ErrInvalidKey) {
					t.Errorf("ParseKey(%q) error = %v, want ErrInvalidKey", tt.in, err)
				}

				return
			}

			if k.String() != tt.in {
				t.Errorf("round trip = %q, want %q", k.String(), tt.in)
			}
		})
	}
}

func TestKeyFromBytes(t *testing.T) {
	raw := make([]byte, KeySize)
	for i := range raw {
		raw[i] = byte(i)
	}

	k, err := KeyFromBytes(raw)
	if err != nil {
		t.Fatalf("KeyFromBytes() error = %v", err)
	}

	if !bytes.Equal(k.Bytes(), raw) {
		t.Errorf("Bytes() = %x, want %x", k.Bytes(), raw)
	}

	if _, err := KeyFromBytes(raw[:31]); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("KeyFromBytes(31 bytes) error = %v, want ErrInvalidKey", err)
	}
}

func TestInstrumented_EmitsStorageMetrics(t *testing.T) {
	m := metrics.New()
	s := NewInstrumented(newTestStore(t), m, metrics.DriverLocal)

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
			"driver": metrics.DriverLocal, "op": tt.op, "result": tt.result,
		})
		if got != tt.want {
			t.Errorf("storage_operations_total{op=%q,result=%q} = %v, want %v", tt.op, tt.result, got, tt.want)
		}
	}
}

// An aborted write records NO put: dedup elided it, and blob.Service already counts
// that on the headline series as put/hit. Counting it here too would double-count
// every deduped upload.
func TestInstrumented_AbortRecordsNoPut(t *testing.T) {
	m := metrics.New()
	s := NewInstrumented(newTestStore(t), m, metrics.DriverLocal)

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
		"driver": metrics.DriverLocal, "op": opPut, "result": string(metrics.ResultHit),
	})
	if got != 0 {
		t.Errorf("aborted write recorded %v puts, want 0", got)
	}
}

// TestInstrumented_PreRegistersSeriesAtZero proves the store's operation series
// EXIST the moment it is constructed, before any call. This is the storage half of
// the "STORAGE_DIR is not dead config" guarantee: bakery_storage_operations_total
// must be present from boot so a rate() alert can distinguish "no storage traffic
// yet" from "the store was never wired up at all". counterValue reads zero for both
// an absent series and a present-but-zero one, so this test counts series instead.
func TestInstrumented_PreRegistersSeriesAtZero(t *testing.T) {
	m := metrics.New()

	// Construct and discard: the series live in the registry, not on the store.
	_ = NewInstrumented(newTestStore(t), m, metrics.DriverLocal)

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
				"driver": metrics.DriverLocal, "op": op, "result": res,
			}); v != 0 {
				t.Errorf("pre-registered series {op=%q,result=%q} = %v, want 0", op, res, v)
			}
		}
	}
}

// seriesCount returns how many series a metric family currently exposes.
func seriesCount(t *testing.T, m *metrics.Metrics, name string) int {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	for _, f := range families {
		if f.GetName() == name {
			return len(f.GetMetric())
		}
	}

	return 0
}

// counterValue reads one series out of the registry by name and exact label set.
func counterValue(t *testing.T, m *metrics.Metrics, name string, labels map[string]string) float64 {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}

	for _, f := range families {
		if f.GetName() != name {
			continue
		}

		for _, metric := range f.GetMetric() {
			got := map[string]string{}
			for _, lp := range metric.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}

			if len(got) != len(labels) {
				continue
			}

			match := true

			for k, v := range labels {
				if got[k] != v {
					match = false

					break
				}
			}

			if match {
				return metric.GetCounter().GetValue()
			}
		}
	}

	return 0
}
