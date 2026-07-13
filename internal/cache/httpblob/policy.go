// Package httpblob is the ONE shared HTTP cache handler, parameterized per backend
// by a Policy. sstate and downloads (M2) are two Backend values wrapping this handler
// with different Policy; bazel's /ac and /cas (M4) and the OCI proxy (M5) plug in the
// same way. Getting this seam right is what makes four future backends nearly free.
//
// The read path is a dumb static file server with three lethal protocol traps baked
// into it, all of which come from BitBake's fetcher and NONE of which are the obvious
// REST answer:
//
//   - HEAD is the HOT path, not GET: BitBake fires a BB_NUMBER_THREADS-parallel HEAD
//     storm over the whole setscene graph at build start. HEAD is answered from
//     blob.Service.Stat -- metadata only, it NEVER opens the object bytes.
//   - A miss is 404, NEVER 403 and never a 200 with an empty body: BitBake's
//     HTTPMethodFallback retries a 403 (and a 405) as a full-body GET, so a 403 on a
//     missing object turns the HEAD storm into a GET storm.
//   - A valid-but-unauthorized read is ALSO 401, not 403, for the same reason plus
//     the project-existence-oracle 403 would be.
//
// Metrics are labeled ONLY through Route.Ref, which pins the label set to resolved
// slugs and constant backend/kind -- never r.URL.Path, which mints one Prometheus
// series per sstate object and kills the scrape inside a single build.
package httpblob

import (
	"errors"
	"strings"

	"github.com/jsmith212/bakery/internal/blob"
)

// Policy parameterizes the shared handler for one backend kind. Everything that
// differs between sstate, downloads (and later /ac, /cas, oci) lives here; the
// GET/HEAD/PUT machinery is written exactly once.
type Policy struct {
	// Namespace is the cache_objects PRIMARY-KEY discriminator handed to Route.Ref:
	// "" for sstate and downloads, "ac"/"cas" (M4), "blobs"/"manifests" (M5). It is
	// CONSTANT per policy and part of the PK, which is what keeps /ac/<hex> and
	// /cas/<hex> -- both 64 hex, one verified and one not -- from colliding. It is
	// deliberately NOT a Classify return: letting it vary per key invites two keys
	// landing in different namespaces.
	Namespace string

	// Overwrite: may a PUT repoint an existing key at new content? Both M2 policies
	// false -- a PUT of an existing key is an idempotent 200 no-op, never a content
	// swap, never a 409. /ac (M4) is the only true.
	Overwrite bool

	// Verify is the per-call PUT body policy. It is stored as a blob.Verify rather
	// than a bool because blob.Verify has NO valid zero value
	// (ErrVerificationUnspecified) -- a bool would make a zero Policy a landmine and
	// force the handler to reconstruct the policy. Per-policy, not global, precisely
	// because M4's /cas verifies while /ac beside it does not. Both M2 policies:
	// blob.NoVerify().
	Verify blob.Verify

	// Classify validates and canonicalizes the DECODED key (r.PathValue has already
	// turned %3A into ':' and %2F into '/') and derives the metrics kind label in a
	// SINGLE walk that also carries the traversal rejection. One func, not two, so
	// the kind and the key can never disagree about what the key is. A rejection is
	// rendered 400. This is both the path-traversal defense and the Prometheus
	// cardinality bound.
	Classify func(decoded string) (kind, key string, err error)
}

// SstatePolicy serves the Yocto sstate mirror: [universal/]<hh>/<hh>/sstate:... keys
// (they contain slashes, so the route tail is {path...}), .siginfo/.sig sidecars, no
// overwrite, no digest verification (the sstate object name is not a content hash).
var SstatePolicy = Policy{
	Namespace: "",
	Overwrite: false,
	Verify:    blob.NoVerify(),
	Classify:  classifySstate,
}

// DownloadsPolicy serves the source premirror: a flat directory of basenames, no
// overwrite, no verification.
var DownloadsPolicy = Policy{
	Namespace: "",
	Overwrite: false,
	Verify:    blob.NoVerify(),
	Classify:  classifyDownloads,
}

// errBadKey is what Classify returns for a key that fails structural validation. The
// handler renders it 400.
var errBadKey = errors.New("httpblob: bad object key")

// classifySstate validates an sstate key STRUCTURALLY, not against a grammar regex: a
// strict sstate:-anchored regex 404s legitimate do_populate_lic swspec objects (empty
// arch, no universal/ prefix), and .siginfo/.sig sidecars are ordinary keys. It
// rejects only what would escape the namespace or corrupt a DB key: empty, a leading
// '/', and any segment that is ""/"."/".."/contains a backslash or NUL. The kind
// label is "siginfo" for a .siginfo/.sig sidecar, else "object".
func classifySstate(decoded string) (kind, key string, err error) {
	if decoded == "" || decoded[0] == '/' {
		return "", "", errBadKey
	}

	for _, seg := range strings.Split(decoded, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\\x00") {
			return "", "", errBadKey
		}
	}

	if strings.HasSuffix(decoded, ".siginfo") || strings.HasSuffix(decoded, ".sig") {
		return "siginfo", decoded, nil
	}

	return "object", decoded, nil
}

// classifyDownloads validates a single safe basename. %2F has already decoded to '/'
// by the time the key reaches here, so a real '/' is the traversal attempt and is
// rejected -- the {basename} route shape alone does NOT stop `..%2F..%2Fetc`, which
// arrives decoded as `../../etc`; this validator does. The kind label is always
// "file".
func classifyDownloads(decoded string) (kind, key string, err error) {
	if decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\\x00") {
		return "", "", errBadKey
	}

	return "file", decoded, nil
}
