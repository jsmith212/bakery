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
	"github.com/jsmith212/bakery/internal/storage"
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

	// VerifyFromKey derives the PUT body verification policy from THIS request's key.
	// It is the one thing the static Verify above cannot express: /cas must verify the
	// body against a digest parsed out of the key, which varies per request. nil ->
	// use the static Verify. A non-nil error renders 400, never 500 -- a malformed key
	// is a client error of exactly the same class as a body that fails VerifyDigest,
	// which servePut already maps to 400. M5's OCI blobs (sha256:<hex>) reuse this with
	// a different parser that CAN reject, which is why it is a func and not a bool.
	VerifyFromKey func(key string) (blob.Verify, error)

	// AllowDelete registers DELETE. ccache's HTTP backend uses it as a first-class verb
	// and treats ANY non-2xx -- including the 405 an UNregistered route returns -- as a
	// hard failure that latches the WHOLE backend off, reads included, for that compile.
	AllowDelete bool

	// WebDAV registers PROPFIND and MKCOL. sccache's WebDAV mode is opendal, which
	// PROPFINDs the parent collection before EVERY write and MKCOLs any parent the
	// PROPFIND reports missing. A 405 on either does not fail loudly: sccache degrades
	// to CacheMode::ReadOnly for the whole process and the cache silently never fills.
	WebDAV bool

	// SkipIngestIfPresent short-circuits a PUT whose key is already present: probe Stat
	// (LRU-backed, zero queries warm) and return the idempotent 200 WITHOUT staging,
	// fsyncing, hashing or opening a transaction. TRUE FOR /cas ONLY -- /cas is
	// Overwrite=false + digest-verified, so an existing key PROVABLY names byte-identical
	// content and skipping is a semantic no-op. On /ac (Overwrite=true, opaque) an
	// existing key says nothing about the incoming body, so applying it there is content
	// loss.
	SkipIngestIfPresent bool
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

// maxACKeyLen bounds a /ac key. A sha256 hex is 64 and ccache's padded key is 64; 128 is
// generous headroom that still keeps the key well inside the cache_objects btree and caps
// the Prometheus/DB key space. It is a leniency bound, not a grammar.
const maxACKeyLen = 128

// ACPolicy serves the bazel /ac action-cache mount: ccache @layout=bazel writes a binary
// CacheEntry, moon api=http writes Manifest JSON, and Bazel --remote_cache=http:// writes
// a serialized REAPI ActionResult -- three unrelated encodings under three unrelated key
// derivations, all landing here. It is the ONE mutable namespace (Overwrite) and it is
// OPAQUE: NoVerify, never parsed, never digest-verified. Parsing would break two of the
// three clients at once. DELETE is a first-class ccache verb here.
var ACPolicy = Policy{
	Namespace:   "ac",
	Overwrite:   true,
	Verify:      blob.NoVerify(),
	Classify:    classifyAC,
	AllowDelete: true,
}

// CASPolicy serves the bazel /cas content-addressed mount: key == sha256(body), verified
// per request via VerifyFromKey. No overwrite (a content address is immutable), and
// SkipIngestIfPresent because a re-PUT of a present key is a byte-identical no-op that
// must not pay for a staged write + fsync + a whole transaction on the redundant-PUT
// storm (Bazel's HTTP client re-PUTs every blob every build).
var CASPolicy = Policy{
	Namespace:           "cas",
	Overwrite:           false,
	VerifyFromKey:       verifyCASKey,
	Classify:            classifyCAS,
	SkipIngestIfPresent: true,
}

// SccachePolicy serves sccache's WebDAV mount. Like /ac it is opaque and mutable, but it
// is a SEPARATE namespace on a SEPARATE, sharded route (a/b/c/<hex>) reached over WebDAV
// -- it is NOT on /ac (three shipped docs said it was; all three are wrong). WebDAV
// answers the PROPFIND -> MKCOL -> PUT sequence opendal issues before every write.
var SccachePolicy = Policy{
	Namespace: "sccache",
	Overwrite: true,
	Verify:    blob.NoVerify(),
	Classify:  classifySccache,
	WebDAV:    true,
}

// classifyCAS is STRICT: a /cas key is a content address, so it must be exactly 64
// LOWERCASE hex characters -- the string form of a sha256 -- and nothing else. That is
// what lets verifyCASKey call storage.ParseKey knowing it cannot fail on a well-formed
// request, and what makes `key == sha256(body)` a total contract. hex.Decode would accept
// uppercase, so the lowercase check is not redundant: two casings of one digest are two
// cache_objects keys for one blob. kind "cas".
func classifyCAS(decoded string) (kind, key string, err error) {
	if len(decoded) != 64 || !isLowerHex(decoded) {
		return "", "", errBadKey
	}

	return "cas", decoded, nil
}

// classifyAC is LENIENT on purpose. /ac is opaque and its clients key it three
// incompatible ways: ccache @layout=bazel writes a 40-hex BLAKE3-160 PADDED to 64 by
// repeating its own prefix (it hashes NOTHING), Bazel writes a 64-hex sha256(Action), and
// moon writes a 64-hex action digest. Requiring 64 hex would 404 a legitimate client --
// the exact shape of the hashserv UNIHASH_REGEX bug, which failed 11 of 17 gate tests. So
// it validates only that the key is non-empty, hex, and bounded: hex-only INHERENTLY
// rejects traversal ('/', '.', '\', NUL are not hex), and it never parses the body. kind
// "ac".
func classifyAC(decoded string) (kind, key string, err error) {
	if len(decoded) == 0 || len(decoded) > maxACKeyLen || !isHex(decoded) {
		return "", "", errBadKey
	}

	return "ac", decoded, nil
}

// classifySccache validates sccache's sharded WebDAV key: normalize_key shards every key
// into {a}/{b}/{c}/{64-hex}, so the tail is multi-segment (route {path...}). It is opaque
// -- never parsed, never digest-verified -- so this only rejects what would escape the
// namespace or corrupt a DB key, exactly like classifySstate: empty, a leading '/', and
// any ""/"."/".."/backslash/NUL segment. It also serves the parent-collection paths
// opendal PROPFINDs before a write (a/b/c, a/b, a). kind "sccache".
func classifySccache(decoded string) (kind, key string, err error) {
	if decoded == "" || decoded[0] == '/' {
		return "", "", errBadKey
	}

	for _, seg := range strings.Split(decoded, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "\\\x00") {
			return "", "", errBadKey
		}
	}

	return "sccache", decoded, nil
}

// verifyCASKey turns a validated /cas key (64 lowercase hex, guaranteed by classifyCAS)
// into the per-request VerifyDigest policy. storage.ParseKey cannot fail on a
// classifyCAS-approved key; the error return is the seam M5's OCI blobs reuse with a
// sha256:<hex> parser that CAN reject, which servePut renders 400.
func verifyCASKey(key string) (blob.Verify, error) {
	d, err := storage.ParseKey(key)
	if err != nil {
		return blob.Verify{}, err
	}

	return blob.VerifyDigest(d), nil
}

// isLowerHex reports whether s is all lowercase hex digits. hand-rolled rather than a
// regexp: it is on the /cas classify path, which fronts the redundant-PUT storm.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// isHex accepts either case: /ac clients emit lowercase, but leniency here costs nothing
// and a case check is not the /ac namespace's job (its keys are opaque, not addresses).
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}

	return true
}
