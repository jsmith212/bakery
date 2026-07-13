package cache

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// THE STRUCTURAL INVARIANT, ASSERTED.
//
// "cache.Deps must NOT carry *repository.Queries" is what enforces "blob.Service is
// the only writer of object metadata". A comment saying so is worth nothing the day
// somebody adds a field to hurry a backend along -- so this walks Deps' fields
// (transitively, through structs and pointers-to-structs) and fails if the
// repository package appears anywhere in the reachable EXPORTED surface.
//
// It deliberately does not follow into blob.Service: blob holds a Reader and a Txer
// and MUST, because it is the one writer. Its fields are unexported, so a backend
// cannot reach them.
func TestDepsCarriesNoQueries(t *testing.T) {
	banned := reflect.TypeOf(repository.Queries{}).PkgPath()

	var walk func(t reflect.Type, path string, depth int)

	walk = func(rt reflect.Type, path string, depth int) {
		if depth > 4 {
			return
		}

		for rt.Kind() == reflect.Pointer {
			rt = rt.Elem()
		}

		if rt.PkgPath() == banned {
			t.Errorf("cache.Deps exposes %s at %s -- a backend could write object metadata directly", rt, path)

			return
		}

		if rt.Kind() != reflect.Struct {
			return
		}

		for i := range rt.NumField() {
			f := rt.Field(i)

			// Unexported fields are unreachable from a backend, and blob.Service's are
			// unexported precisely so that its Reader/Txer stay its own.
			if !f.IsExported() {
				continue
			}

			walk(f.Type, path+"."+f.Name, depth+1)
		}
	}

	walk(reflect.TypeOf(Deps{}), "Deps", 0)
}

func TestDeps_Validate(t *testing.T) {
	full := Deps{Blobs: nil, Metrics: metrics.New(), Logger: slog.Default()}

	// Blobs is nil here (constructing a real one needs a DB), so Validate must reject
	// it -- which is itself the assertion that Blobs is required.
	if err := full.Validate(); err == nil {
		t.Error("Validate() accepted a Deps with no blob.Service")
	}

	if err := (Deps{Blobs: nil, Metrics: nil, Logger: nil}).Validate(); err == nil {
		t.Error("Validate() accepted an empty Deps")
	}

	var missing Deps
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "Blobs") {
		t.Errorf("Validate() error = %v, want one naming Blobs", err)
	}
}

// The DB enum and the metrics constant are two closed sets, and the mapping between
// them must be total: a kind that fell through to string(k) would put an unbounded
// value on a label.
func TestRouteRefLabels(t *testing.T) {
	tests := []struct {
		kind repository.BackendKind
		want metrics.Backend
	}{
		{kind: repository.BackendKindSstate, want: metrics.BackendSstate},
		{kind: repository.BackendKindDownloads, want: metrics.BackendDownloads},
		{kind: repository.BackendKindHashserv, want: metrics.BackendHashserv},
		{kind: repository.BackendKindBazel, want: metrics.BackendBazel},
		{kind: repository.BackendKindOci, want: metrics.BackendOCI},
	}

	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			r := Route{
				Org: "acme", Project: "widget",
				BackendID: 7, Kind: tt.kind,
				Enabled: true, ReadAuthRequired: true,
			}

			ref := r.Ref("cas", "cas", "deadbeef")

			if ref.Backend != tt.want {
				t.Errorf("Ref().Backend = %q, want %q", ref.Backend, tt.want)
			}

			if ref.BackendID != 7 || ref.Org != "acme" || ref.Project != "widget" {
				t.Errorf("Ref() = %+v, want backend 7 / acme / widget", ref)
			}

			if ref.Namespace != "cas" || ref.Key != "deadbeef" {
				t.Errorf("Ref() namespace/key = %q/%q, want cas/deadbeef", ref.Namespace, ref.Key)
			}
		})
	}
}
