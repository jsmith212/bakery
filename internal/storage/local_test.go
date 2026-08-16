package storage

import (
	"bytes"
	"errors"
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

// TestLocal_Conformance runs the Store-interface-only suite (conformance_test.go)
// against Local. This is what proves the extraction in feedback wave 1 (spec
// section 9's prerequisite) is non-regressive: every assertion Local used to
// make inline, it still makes here, driven through the shared suite instead.
func TestLocal_Conformance(t *testing.T) {
	runConformance(t, func(t *testing.T) Store { return newTestStore(t) }, metrics.DriverLocal)
}

// TestLocal_FanOutLayout is LOCAL'S OWN on-disk layout choice (2/2-hex
// fan-out directories), not part of the Store interface's contract -- a
// driver with no filesystem at all (S3) has nothing analogous to assert.
func TestLocal_FanOutLayout(t *testing.T) {
	s := newTestStore(t)
	k := put(t, s, []byte("layout"))
	h := k.String()

	want := filepath.Join(s.Root(), objectsDir, h[0:2], h[2:4], h)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("object not at %s: %v", want, err)
	}
}

// TestLocal_NotObservableBeforeCommit is Local's stronger, driver-specific
// guarantee: nothing is observable at the object's key until Commit RETURNS,
// and the partial bytes live outside objects/ entirely while staged. S3
// publishes at Sync instead (storage.go's Writer doc carries the amended,
// driver-aware property -- see feedback wave 1 spec section 9), so this
// assertion would fail a compliant S3 driver for being more permissive in a
// documented way, and stays out of the shared conformance suite.
//
// The interface-safe half of the old combined test -- "after Commit, the
// object reads back whole" -- is conformance_test.go's
// CommittedObjectReadsBackWhole, and runs against every driver.
func TestLocal_NotObservableBeforeCommit(t *testing.T) {
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

// TestLocal_AbortLeavesNoStagingFiles is the staging-DIRECTORY-listing half of
// the old combined "AbortLeavesNothing" test -- Local-specific because
// staging/ is a Local implementation detail. The driver-generic half (Abort
// leaves no object reachable at the key) is
// conformance_test.go's AbortLeavesNoObject, and runs against every driver.
func TestLocal_AbortLeavesNoStagingFiles(t *testing.T) {
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

	entries, err := os.ReadDir(filepath.Join(s.Root(), stagingDir))
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("staging/ holds %d files after Abort, want 0", len(entries))
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

// seriesCount returns how many series a metric family currently exposes.
// Shared with conformance_test.go.
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
// Shared with conformance_test.go.
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
