package oci

import (
	"errors"
	"testing"
)

// TestSplitRef is the marker-scan gate. The case that matters is `acme/manifests/app`:
// a repository whose NAME contains the marker word. Splitting on the first occurrence
// parses it as repository "acme" with a reference containing slashes -- which resolves
// to nothing and silently 404s a real image forever, for exactly one customer. This is
// the same invariant as REAPI ByteStream resource names, where instance_name contains
// slashes.
func TestSplitRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rest     string
		wantName string
		wantKind string
		wantRef  string
		wantErr  error
	}{
		{
			name: "simple tag", rest: "alpine/manifests/latest",
			wantName: "alpine", wantKind: kindManifests, wantRef: "latest", wantErr: nil,
		},
		{
			name: "namespaced repository", rest: "library/alpine/manifests/3.20",
			wantName: "library/alpine", wantKind: kindManifests, wantRef: "3.20", wantErr: nil,
		},
		{
			name: "deeply nested repository", rest: "team/infra/base/alpine/manifests/latest",
			wantName: "team/infra/base/alpine", wantKind: kindManifests, wantRef: "latest", wantErr: nil,
		},
		{
			name: "blob by digest", rest: "library/alpine/blobs/sha256:" + testIndexDigest,
			wantName: "library/alpine", wantKind: kindBlobs,
			wantRef: "sha256:" + testIndexDigest, wantErr: nil,
		},
		{
			// THE PATHOLOGICAL CASE. A first-occurrence split gives
			// name="acme", ref="app/manifests/latest".
			name: "repository literally named x/manifests/y", rest: "acme/manifests/app/manifests/latest",
			wantName: "acme/manifests/app", wantKind: kindManifests, wantRef: "latest", wantErr: nil,
		},
		{
			// Same shape, mixed markers: the rightmost marker is the real separator.
			name:     "repository containing manifests, pulled as a blob",
			rest:     "acme/manifests/app/blobs/sha256:" + testIndexDigest,
			wantName: "acme/manifests/app", wantKind: kindBlobs,
			wantRef: "sha256:" + testIndexDigest, wantErr: nil,
		},
		{
			name:     "repository containing blobs, pulled by tag",
			rest:     "acme/blobs/app/manifests/latest",
			wantName: "acme/blobs/app", wantKind: kindManifests, wantRef: "latest", wantErr: nil,
		},
		{
			name: "leading slash is tolerated", rest: "/alpine/manifests/latest",
			wantName: "alpine", wantKind: kindManifests, wantRef: "latest", wantErr: nil,
		},
		{name: "no marker at all", rest: "alpine/latest", wantErr: errBadRef},
		{name: "empty", rest: "", wantErr: errBadRef},
		{name: "empty repository name", rest: "manifests/latest", wantErr: errBadRef},
		{name: "empty reference", rest: "alpine/manifests/", wantErr: errBadRef},
		{
			name: "reference containing a slash is refused",
			rest: "alpine/manifests/a/b", wantErr: errBadRef,
		},
		{name: "push api", rest: "alpine/blobs/uploads/", wantErr: errPushPath},
		{name: "push api with a uuid", rest: "alpine/blobs/uploads/abc-123", wantErr: errPushPath},
		{
			// tags/list is dispatched by splitTagsList BEFORE splitRef ever sees it
			// (see TestSplitTagsList); to splitRef alone it is just a marker-less tail.
			name: "tags list is not a manifests or blobs ref", rest: "alpine/tags/list",
			wantErr: errBadRef,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, kind, ref, err := splitRef(tt.rest)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("splitRef(%q) error = %v, want %v", tt.rest, err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("splitRef(%q) unexpected error: %v", tt.rest, err)
			}

			if name != tt.wantName || kind != tt.wantKind || ref != tt.wantRef {
				t.Errorf("splitRef(%q) = (%q, %q, %q), want (%q, %q, %q)",
					tt.rest, name, kind, ref, tt.wantName, tt.wantKind, tt.wantRef)
			}
		})
	}
}

// TestSplitTagsList is the suffix-check gate, and the ordering case is the one that
// earns it: a repository legally named `acme/manifests` has a tags URL that splitRef's
// marker scan would mis-carve, so serve() must ask splitTagsList FIRST.
func TestSplitTagsList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rest     string
		wantName string
		ok       bool
	}{
		{name: "simple", rest: "alpine/tags/list", wantName: "alpine", ok: true},
		{name: "namespaced", rest: "library/alpine/tags/list", wantName: "library/alpine", ok: true},
		{name: "leading slash is tolerated", rest: "/alpine/tags/list", wantName: "alpine", ok: true},
		{
			// The ordering case: a repo whose name contains a marker word. splitRef
			// would carve this into name "acme", ref "tags/list" and refuse it.
			name: "repository literally named x/manifests", rest: "acme/manifests/tags/list",
			wantName: "acme/manifests", ok: true,
		},
		{
			// A repository literally named "tags" listing its tags.
			name: "repository named tags", rest: "tags/tags/list", wantName: "tags", ok: true,
		},
		{name: "manifest pull is not a listing", rest: "alpine/manifests/latest", ok: false},
		{
			// `<repo>/manifests/list` is a TAG named "list", not a listing.
			name: "tag literally named list", rest: "alpine/manifests/list", ok: false,
		},
		{name: "bare tags/list has no repository", rest: "tags/list", ok: false},
		{name: "empty", rest: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			name, ok := splitTagsList(tt.rest)

			if ok != tt.ok || name != tt.wantName {
				t.Errorf("splitTagsList(%q) = (%q, %v), want (%q, %v)",
					tt.rest, name, ok, tt.wantName, tt.ok)
			}
		})
	}
}

// TestDigestHex pins the two rejections that are silent-failure shaped: uppercase hex
// (two casings of one digest are two cache_objects keys naming one blob) and sha512
// (a legal OCI digest this proxy cannot fetch, which must be a clean miss rather than
// a 400 so the client falls back to a registry that can serve it).
func TestDigestHex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  string
		want string
		ok   bool
	}{
		{name: "sha256", ref: "sha256:" + testIndexDigest, want: testIndexDigest, ok: true},
		{name: "no algorithm", ref: testIndexDigest, want: "", ok: false},
		{name: "sha512 is a clean miss", ref: "sha512:" + testIndexDigest, want: "", ok: false},
		{name: "too short", ref: "sha256:abc", want: "", ok: false},
		{
			name: "uppercase is refused",
			ref:  "sha256:D9E853E87E55526F6B2917DF91A2115C36DD7C696A35BE12163D44E6E2A4B6BC",
			want: "", ok: false,
		},
		{name: "not hex", ref: "sha256:" + testIndexDigest[:63] + "z", want: "", ok: false},
		{name: "a tag", ref: "latest", want: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := digestHex(tt.ref)
			if got != tt.want || ok != tt.ok {
				t.Errorf("digestHex(%q) = (%q, %v), want (%q, %v)", tt.ref, got, ok, tt.want, tt.ok)
			}
		})
	}
}
