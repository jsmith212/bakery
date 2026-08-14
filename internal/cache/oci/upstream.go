package oci

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/jsmith212/bakery/internal/metrics"
)

// ErrNoPrincipal is the refusal that keeps this proxy from being an open relay. See
// Registry's method preamble.
var ErrNoPrincipal = errors.New("oci: upstream fetch requires a verified principal")

// ErrUpstreamNotFound is a clean upstream miss, distinct from an upstream failure. The
// handler renders both as 404, but only the failure is counted as an outage.
var ErrUpstreamNotFound = errors.New("oci: upstream does not have it")

// UpstreamRef names one repository at one upstream registry.
//
// Host is ALREADY NORMALIZED AND ALREADY ALLOWLISTED. Nothing in this file re-checks
// it, because by the time a ref exists the ?ns= gate (policy.resolveUpstream) has
// already decided that this deployment is willing to dial this host. Constructing an
// UpstreamRef from a raw request value, anywhere, reopens the SSRF hole.
type UpstreamRef struct {
	Host string
	Name string
}

// String renders the ref the way go-containerregistry parses it.
func (r UpstreamRef) String() string { return r.Host + "/" + r.Name }

// Manifest is one manifest as the upstream described it.
type Manifest struct {
	// Raw is the manifest body, EXACTLY as the registry sent it. It is never
	// unmarshalled and re-marshalled: a JSON round trip reorders keys and rewrites
	// whitespace, which changes the digest, which breaks Docker-Content-Digest -- and
	// it only shows up on multi-arch index manifests, i.e. not in your test and yes in
	// production. Empty on a Resolve (a HEAD has no body).
	Raw []byte

	// MediaType is the response Content-Type, verbatim. containerd assigns it straight
	// into ocispec.Descriptor.MediaType and dispatches on it, so it is a fact to be
	// carried, not a value to be recomputed.
	MediaType string

	// Digest is the digest the UPSTREAM CLAIMED, as `sha256:<hex>`.
	//
	// IT IS ADVISORY AND IT IS NEVER A STORAGE KEY. go-containerregistry does not
	// verify a tag-fetched manifest against Docker-Content-Digest -- its own source
	// says so, because too many registries get the header wrong -- so this value is an
	// unverified string from a third party. It is used for exactly one thing: the
	// "did this tag move?" comparison in a revalidation, where being wrong costs a
	// redundant full fetch and nothing else, because the bytes that fetch returns are
	// re-hashed by us before they are stored under anything.
	Digest string

	// Size is the Content-Length the registry reported.
	Size int64
}

// Fetcher is the NARROW upstream surface the handlers use: four reads, no writes, and
// every one of them takes a Principal.
//
// It is an interface for the usual consumer-side reason (the handlers are testable
// against a hand-written fake, including a deliberately LYING one) and for one
// specific reason: the test that proves an upstream which reports a digest its bytes
// do not hash to cannot get those bytes stored under the digest it claimed.
type Fetcher interface {
	// Resolve is the upstream HEAD: digest and media type, no body. It is what a stale
	// tag revalidates with, and against Docker Hub it is FREE -- a HEAD on a manifest
	// does not decrement the pull rate limit (verified live).
	Resolve(ctx context.Context, p Principal, ref UpstreamRef, reference string) (Manifest, error)

	// Manifest is the upstream GET: the raw bytes.
	Manifest(ctx context.Context, p Principal, ref UpstreamRef, reference string) (Manifest, error)

	// StatBlob is a blob HEAD -- size only, NEVER a body. It is what answers a
	// downstream HEAD on an uncached blob, and it is deliberately better than
	// distribution's own proxy, which downloads the entire blob to answer a HEAD.
	StatBlob(ctx context.Context, p Principal, ref UpstreamRef, digest string) (int64, error)

	// Blob streams a blob body. The caller MUST Close it.
	Blob(ctx context.Context, p Principal, ref UpstreamRef, digest string) (io.ReadCloser, int64, error)
}

// Registry is the go-containerregistry-backed Fetcher.
//
// # Why every method takes a Principal
//
// This is the open-relay seam, and DESIGN.md names it as one of M5's top risks. If any
// code path can reach an upstream fetch without an authenticated caller behind it,
// then Bakery serves Docker Hub to the entire internet using the OPERATOR'S
// credentials and rate limit -- and it does so while looking perfectly healthy, because
// a relay's traffic is indistinguishable from a busy cache.
//
// auth.Principal is sealed (an unexported method, no exported constructor, no usable
// zero value), so a caller cannot forge one; the nil check at the top of each method
// closes the only remaining hole, which is passing nothing. Together those two make
// "an anonymous request never causes an outbound fetch" a property of the type system
// rather than of a convention someone has to keep remembering -- which is exactly the
// upgrade DESIGN.md asked for over hashserv's call-site-convention gate.
//
// # Why ONE long-lived Puller
//
// The package-level remote.Get/remote.Head re-run the entire registry token dance on
// every call: a request to /v2/, a 401, a fetch from the auth realm, then the real
// request. A *remote.Puller memoizes the authenticated fetcher per repository
// (readers sync.Map + a sync.Once per entry, verified in v0.21.9), so holding one for
// the process lifetime turns four round trips per layer into one. It is shared across
// upstream hosts because that memo is keyed per repository, not per host, so per-host
// Pullers would create exactly the same number of fetchers and only add bookkeeping.
//
// The keychain is OURS and is built from server config. authn.DefaultKeychain is never
// used and must never be: it reads the SERVER's ~/.docker/config.json, which in a
// multi-tenant proxy means whatever the operator happened to `docker login` with
// becomes every tenant's upstream identity.
type Registry struct {
	puller   *remote.Puller
	keychain authn.Keychain
}

var _ Fetcher = (*Registry)(nil)

// NewRegistry builds the upstream client over the server-level credential map.
func NewRegistry(cfg Config, m *metrics.Metrics) (*Registry, error) {
	kc := configKeychain{creds: cfg.UpstreamAuth}

	puller, err := remote.NewPuller(
		remote.WithAuthFromKeychain(kc),
		remote.WithTransport(newInstrumentedTransport(http.DefaultTransport, m)),
	)
	if err != nil {
		return nil, err
	}

	return &Registry{puller: puller, keychain: kc}, nil
}

// Resolve heads a manifest.
func (u *Registry) Resolve(
	ctx context.Context, p Principal, ref UpstreamRef, reference string,
) (Manifest, error) {
	if p == nil {
		return Manifest{}, ErrNoPrincipal
	}

	r, err := parseReference(ref, reference)
	if err != nil {
		return Manifest{}, err
	}

	ctx = withUpstreamHost(ctx, NormalizeUpstream(ref.Host))

	desc, err := u.puller.Head(ctx, r)
	if err != nil {
		return Manifest{}, upstreamErr(err)
	}

	return Manifest{
		Raw: nil, MediaType: string(desc.MediaType),
		Digest: desc.Digest.String(), Size: desc.Size,
	}, nil
}

// Manifest gets a manifest body.
func (u *Registry) Manifest(
	ctx context.Context, p Principal, ref UpstreamRef, reference string,
) (Manifest, error) {
	if p == nil {
		return Manifest{}, ErrNoPrincipal
	}

	r, err := parseReference(ref, reference)
	if err != nil {
		return Manifest{}, err
	}

	// Puller.Get requests the full four-type Accept list and returns the RAW bytes it
	// received. Nothing here decodes the manifest, so nothing here can re-serialize it.
	ctx = withUpstreamHost(ctx, NormalizeUpstream(ref.Host))

	desc, err := u.puller.Get(ctx, r)
	if err != nil {
		return Manifest{}, upstreamErr(err)
	}

	return Manifest{
		Raw: desc.Manifest, MediaType: string(desc.MediaType),
		Digest: desc.Digest.String(), Size: int64(len(desc.Manifest)),
	}, nil
}

// StatBlob heads a blob.
func (u *Registry) StatBlob(
	ctx context.Context, p Principal, ref UpstreamRef, digest string,
) (int64, error) {
	if p == nil {
		return 0, ErrNoPrincipal
	}

	layer, err := u.layer(ctx, p, ref, digest)
	if err != nil {
		return 0, err
	}

	size, err := layer.Size() // a HEAD; never a body
	if err != nil {
		return 0, upstreamErr(err)
	}

	return size, nil
}

// Blob streams a blob body. go-containerregistry verifies the stream against the
// requested digest as it reads, which is a second independent check on top of the
// VerifyDigest the ingest Put performs.
func (u *Registry) Blob(
	ctx context.Context, p Principal, ref UpstreamRef, digest string,
) (io.ReadCloser, int64, error) {
	if p == nil {
		return nil, 0, ErrNoPrincipal
	}

	layer, err := u.layer(ctx, p, ref, digest)
	if err != nil {
		return nil, 0, err
	}

	size, err := layer.Size()
	if err != nil {
		return nil, 0, upstreamErr(err)
	}

	rc, err := layer.Compressed()
	if err != nil {
		return nil, 0, upstreamErr(err)
	}

	return rc, size, nil
}

// layer resolves a blob reference through the shared Puller.
func (u *Registry) layer(
	ctx context.Context, _ Principal, ref UpstreamRef, digest string,
) (v1Layer, error) {
	d, err := name.NewDigest(ref.String() + "@" + digest)
	if err != nil {
		return nil, ErrUpstreamNotFound
	}

	ctx = withUpstreamHost(ctx, NormalizeUpstream(ref.Host))

	l, err := u.puller.Layer(ctx, d)
	if err != nil {
		return nil, upstreamErr(err)
	}

	return l, nil
}

// v1Layer is the sliver of v1.Layer this package uses. Naming it keeps the gcr type
// out of the function signatures above.
type v1Layer interface {
	Size() (int64, error)
	Compressed() (io.ReadCloser, error)
}

// parseReference builds a gcr reference from a repository and either a tag or a
// digest. A reference gcr cannot parse is a clean upstream miss, not a server error:
// it means the client asked for something no registry could answer either.
func parseReference(ref UpstreamRef, reference string) (name.Reference, error) {
	sep := ":"
	if isDigestRef(reference) {
		sep = "@"
	}

	r, err := name.ParseReference(ref.String() + sep + reference)
	if err != nil {
		return nil, ErrUpstreamNotFound
	}

	return r, nil
}

// upstreamErr folds a gcr transport error into our two sentinels. A 404 from the
// upstream is a MISS (the client will fall back to the real registry, which is
// correct); everything else is an outage and stays an error so the metrics can see it.
func upstreamErr(err error) error {
	var terr *transportError
	if errors.As(err, &terr) && terr.status == http.StatusNotFound {
		return ErrUpstreamNotFound
	}

	// gcr surfaces registry errors as its own transport.Error, whose string form
	// carries the status. Matching on the sentinel above covers our own instrumented
	// path; this covers gcr's, without importing its internal error type.
	if strings.Contains(err.Error(), "MANIFEST_UNKNOWN") ||
		strings.Contains(err.Error(), "NAME_UNKNOWN") ||
		strings.Contains(err.Error(), "BLOB_UNKNOWN") {
		return ErrUpstreamNotFound
	}

	return err
}

// transportError is what the instrumented transport attaches to a non-2xx it observes,
// so upstreamErr can tell a 404 from a 503 without parsing an error string.
type transportError struct {
	status int
}

func (e *transportError) Error() string { return "oci: upstream status " + strconv.Itoa(e.status) }

// configKeychain resolves upstream credentials from SERVER config and nowhere else.
//
// The alternative, authn.DefaultKeychain, reads the server's own docker config,
// credential helpers and cloud metadata endpoints. In a multi-tenant proxy that turns
// "the operator once ran docker login on this box" into an ambient identity every
// tenant's pulls silently inherit -- and a credential nobody can see in Bakery's
// configuration.
type configKeychain struct {
	creds map[string]Credential
}

func (k configKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	c, ok := k.creds[NormalizeUpstream(r.RegistryStr())]
	if !ok {
		return authn.Anonymous, nil
	}

	return authn.FromConfig(authn.AuthConfig{Username: c.Username, Password: c.Password}), nil
}

// instrumentedTransport is where every bakery_oci_upstream_* series comes from.
//
// It lives at the RoundTripper rather than at the Fetcher methods because that is the
// only layer that sees the actual HTTP exchange: the status code, the latency, and the
// rate-limit headers, which go-containerregistry does not expose to its callers. It
// also means the token-dance round trips are observed rather than invisible.
//
// The `op` label is inferred from the request itself and is a CLOSED set; a request
// that matches nothing (the /v2/ ping, the auth realm) is not recorded at all rather
// than being given an open-ended label.
type instrumentedTransport struct {
	base    http.RoundTripper
	metrics *metrics.Metrics

	mu   sync.Mutex
	recs map[string]*metrics.OCIRecorder
}

// upstreamHostKey carries the ALLOWLISTED upstream host from the Registry entry
// points down to the instrumented transport. The transport must not label from
// req.URL.Host: a blob download redirects to a CDN host (Docker Hub does this in
// production and not in any test), so per-request hosts would leak unbounded CDN
// hostnames into the `upstream` label -- the exact cardinality failure the
// slugs-never-digests invariant exists to prevent -- and misattribute the traffic.
type upstreamHostKey struct{}

func withUpstreamHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, upstreamHostKey{}, host)
}

func newInstrumentedTransport(base http.RoundTripper, m *metrics.Metrics) *instrumentedTransport {
	return &instrumentedTransport{
		base: base, metrics: m,
		mu: sync.Mutex{}, recs: map[string]*metrics.OCIRecorder{},
	}
}

func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	op, ok := classifyUpstreamRequest(req)
	if !ok || t.metrics == nil {
		//nolint:wrapcheck // a pass-through RoundTripper must not decorate the error.
		return t.base.RoundTrip(req)
	}

	// The allowlisted host, stamped by the Registry entry point -- never the request
	// URL's host, which is the redirect TARGET on a CDN-redirected blob download.
	host, _ := req.Context().Value(upstreamHostKey{}).(string)
	if host == "" {
		host = "other" // closed fallback: never a raw per-request hostname
	}

	start := time.Now()

	resp, err := t.base.RoundTrip(req)

	rec := t.recorder(host)

	if err != nil {
		// code 0: no HTTP response at all. Distinct from a 429, and the distinction is
		// what tells "Hub is throttling us" apart from "Hub is unreachable".
		rec.Upstream(op, 0, time.Since(start), err)

		//nolint:wrapcheck // a pass-through RoundTripper must not decorate the error.
		return resp, err
	}

	// A non-2xx is recorded with its status AND as an error only when it is one: a 404
	// is an ordinary miss on a pull-through proxy, not an outage.
	var observed error
	if resp.StatusCode >= http.StatusInternalServerError || resp.StatusCode == http.StatusTooManyRequests {
		observed = &transportError{status: resp.StatusCode}
	}

	rec.Upstream(op, resp.StatusCode, time.Since(start), observed)
	t.observeRateLimit(rec, resp)

	return resp, nil
}

// observeRateLimit publishes the upstream's remaining pull budget.
//
// The value and its window are SERVER-SUPPLIED and are never hardcoded: Docker's own
// documentation says the window is six hours and the live header says w=3600, and only
// the header is true. The header's form is "<n>;w=<seconds>"; we publish n.
func (t *instrumentedTransport) observeRateLimit(rec *metrics.OCIRecorder, resp *http.Response) {
	raw := resp.Header.Get("Ratelimit-Remaining")
	if raw == "" {
		return
	}

	value, _, _ := strings.Cut(raw, ";")

	n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return
	}

	rec.RateLimitRemaining(n)
}

func (t *instrumentedTransport) recorder(host string) *metrics.OCIRecorder {
	t.mu.Lock()
	defer t.mu.Unlock()

	if r, ok := t.recs[host]; ok {
		return r
	}

	r := t.metrics.OCI(host)
	t.recs[host] = r

	return r
}

// classifyUpstreamRequest infers the metrics op from the request. Registry URLs are
// regular enough that this is exact: a manifest HEAD is a resolve, a manifest GET is a
// manifest fetch, and the same split holds for blobs.
func classifyUpstreamRequest(req *http.Request) (metrics.OCIOp, bool) {
	path := req.URL.Path

	switch {
	case strings.Contains(path, "/manifests/"):
		if req.Method == http.MethodHead {
			return metrics.OCIOpResolve, true
		}

		return metrics.OCIOpManifest, true

	case strings.Contains(path, "/blobs/"):
		if req.Method == http.MethodHead {
			return metrics.OCIOpStatBlob, true
		}

		return metrics.OCIOpBlob, true
	}

	return "", false
}
