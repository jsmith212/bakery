package oci

import (
	"errors"
	"strings"
)

// The three things the {rest...} tail of a registry route can name.
const (
	kindManifests = "manifests"
	kindBlobs     = "blobs"
	kindTags      = "tags"

	// refList is the fixed final segment of the tag-listing endpoint: the spec
	// spells it `<name>/tags/list`, with no other reference ever legal there.
	refList = "list"
)

// errBadRef is what splitRef returns for a tail that is not a legal
// <name>/manifests/<ref> or <name>/blobs/<digest>. The handler renders it 404, never
// 400: to a registry client "this mirror does not serve that" and "that is not a
// registry URL" are the same event, and both mean "fall back upstream".
var errBadRef = errors.New("oci: not a manifests or blobs reference")

// errPushPath is the tail that names the PUSH API (<name>/blobs/uploads/...). It is
// separated from errBadRef only so the handler can answer with UNSUPPORTED rather than
// pretend the repository does not exist.
var errPushPath = errors.New("oci: push is not implemented")

// splitRef parses a registry path tail into (repository name, kind, reference).
//
// THE SCAN GOES RIGHT TO LEFT, AND THAT IS THE WHOLE POINT. A repository name contains
// slashes and its grammar puts NO restriction on the words in it: `library/alpine`,
// `my-team/infra/base-images/alpine`, and -- legally, and this is the case that breaks
// the naive parser -- `acme/manifests/alpine`. Splitting on the FIRST "/manifests/"
// parses that last one as repository "acme" with a reference of "alpine/manifests/1.0",
// which resolves to nothing and 404s a real image forever, silently, for exactly one
// customer.
//
// This is the same invariant as REAPI's ByteStream resource names, where instance_name
// contains slashes and the parser must scan for the blobs/uploads marker instead of
// splitting positionally. Same class of bug, same fix, and it gets the same
// pathological-name test.
//
// The reference is returned verbatim and UNVALIDATED beyond being non-empty: it is
// either a tag or a digest, and telling those apart is the caller's job because they
// take completely different paths (mutable + revalidated vs immutable + never).
func splitRef(rest string) (name, kind, ref string, err error) {
	rest = strings.TrimPrefix(rest, "/")

	// The push API lives under <name>/blobs/uploads/. It is checked BEFORE the marker
	// scan because "uploads" would otherwise parse as an ordinary blob reference named
	// "uploads" and get a confusing miss instead of an honest "not implemented".
	if i := strings.LastIndex(rest, "/blobs/uploads"); i >= 0 {
		return "", "", "", errPushPath
	}

	mi := strings.LastIndex(rest, "/"+kindManifests+"/")
	bi := strings.LastIndex(rest, "/"+kindBlobs+"/")

	// Whichever marker is FURTHEST RIGHT wins. A repository named `x/manifests/y`
	// pulled by digest has both markers present, and only the rightmost one is the
	// real separator.
	var (
		idx  int
		mark string
	)

	switch {
	case mi < 0 && bi < 0:
		return "", "", "", errBadRef
	case mi > bi:
		idx, mark = mi, kindManifests
	default:
		idx, mark = bi, kindBlobs
	}

	name = rest[:idx]
	ref = rest[idx+len(mark)+2:]

	// A reference may not itself contain a slash: the registry grammar puts the
	// reference in the LAST path segment, so anything else is a malformed URL and a
	// silent invitation to path traversal in the storage key.
	if name == "" || ref == "" || strings.Contains(ref, "/") {
		return "", "", "", errBadRef
	}

	return name, mark, ref, nil
}

// splitTagsList reports whether a registry path tail is the spec's tag-listing
// endpoint, `<name>/tags/list`, and returns the repository name.
//
// IT IS CHECKED BEFORE splitRef, AND THE ORDER IS CORRECTNESS, NOT TASTE. No legal
// manifests or blobs URL can end in "/tags/list" -- the reference occupies the final
// path segment and may not contain a slash -- so the suffix check can never steal a
// manifest or blob request. The reverse is not true: a repository legally named
// `acme/manifests` (every component of a repo name is unrestricted) has a tags URL of
// `acme/manifests/tags/list`, which splitRef's marker scan would carve into repository
// "acme" with reference "tags/list" and reject. Suffix first parses both correctly.
func splitTagsList(rest string) (string, bool) {
	name, ok := strings.CutSuffix(strings.TrimPrefix(rest, "/"), "/"+kindTags+"/"+refList)
	if !ok || name == "" {
		return "", false
	}

	return name, true
}

// digestHex validates a `sha256:<64 lowercase hex>` reference and returns the bare hex,
// which IS the cache_objects key for the manifests and blobs namespaces.
//
// Everything else is ok=false, and the handler renders that as a clean 404 rather than
// a 400. That deliberately includes `sha512:...`: it is a legal OCI digest that this
// proxy cannot serve (go-containerregistry is sha256-only, so we could not fetch it
// upstream either), and answering "I do not have it" makes the client fall back to a
// registry that does, which is the correct outcome.
//
// LOWERCASE ONLY. hex.Decode would accept uppercase, and two casings of one digest are
// two cache_objects keys naming one blob -- a silent doubling of the store and a tag
// that can point at whichever one was written last.
func digestHex(ref string) (string, bool) {
	hex, ok := strings.CutPrefix(ref, "sha256:")
	if !ok || len(hex) != 64 {
		return "", false
	}

	for i := range len(hex) {
		c := hex[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}

	return hex, true
}

// isDigestRef reports whether a reference is digest-shaped at all (any algorithm).
// It separates "pull by digest" from "pull by tag" BEFORE validation, so an
// unsupported algorithm takes the immutable path and 404s, rather than being mistaken
// for a tag named "sha512:..." and sent to the upstream tag resolver.
func isDigestRef(ref string) bool { return strings.Contains(ref, ":") }
