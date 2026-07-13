package httpblob

import (
	"errors"
	"testing"
)

// TestClassifySstate pins the STRUCTURAL validator: legitimate objects (including the
// awkward ones) pass, the kind label splits object vs siginfo, and every traversal
// shape is rejected. It deliberately does NOT encode an sstate: grammar -- a strict
// regex would 404 legitimate do_populate_lic swspec objects.
func TestClassifySstate(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantKind string
		wantErr  bool
	}{
		{name: "universal object", in: "universal/aa/bb/sstate:zlib:x86_64.tar.zst", wantKind: "object"},
		{name: "arch-scoped object", in: "x86_64/aa/bb/sstate:busybox.tar.zst", wantKind: "object"},
		{name: "swspec object with empty arch prefix", in: "aa/bb/sstate:lic.tar.zst", wantKind: "object"},
		{name: "siginfo sidecar", in: "universal/aa/bb/sstate:zlib.tar.zst.siginfo", wantKind: "siginfo"},
		{name: "sig sidecar", in: "universal/aa/bb/sstate:zlib.tar.zst.sig", wantKind: "siginfo"},

		{name: "empty", in: "", wantErr: true},
		{name: "leading slash", in: "/etc/passwd", wantErr: true},
		{name: "dotdot segment", in: "universal/../bb/x", wantErr: true},
		{name: "dot segment", in: "universal/./bb/x", wantErr: true},
		{name: "empty segment", in: "universal//bb/x", wantErr: true},
		{name: "backslash", in: "universal\\bb\\x", wantErr: true},
		{name: "trailing slash", in: "universal/aa/bb/", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, key, err := classifySstate(tt.in)

			if tt.wantErr {
				if !errors.Is(err, errBadKey) {
					t.Fatalf("classifySstate(%q) err = %v, want errBadKey", tt.in, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("classifySstate(%q) err = %v, want nil", tt.in, err)
			}

			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}

			if key != tt.in {
				t.Errorf("key = %q, want %q (stored verbatim)", key, tt.in)
			}
		})
	}
}

// TestClassifyDownloads: a single safe basename. A real '/' (a %2F that already
// decoded) is the traversal attempt and must be rejected -- the {basename} route shape
// does not stop it.
func TestClassifyDownloads(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "basename", in: "zlib-1.3.1.tar.xz"},
		{name: "basename with colon", in: "git2:libfoo.tar.gz"},

		{name: "empty", in: "", wantErr: true},
		{name: "dot", in: ".", wantErr: true},
		{name: "dotdot", in: "..", wantErr: true},
		{name: "decoded slash is traversal", in: "../../etc/passwd", wantErr: true},
		{name: "backslash", in: "..\\..\\etc", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, key, err := classifyDownloads(tt.in)

			if tt.wantErr {
				if !errors.Is(err, errBadKey) {
					t.Fatalf("classifyDownloads(%q) err = %v, want errBadKey", tt.in, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("classifyDownloads(%q) err = %v, want nil", tt.in, err)
			}

			if kind != "file" {
				t.Errorf("kind = %q, want file", kind)
			}

			if key != tt.in {
				t.Errorf("key = %q, want %q", key, tt.in)
			}
		})
	}
}
