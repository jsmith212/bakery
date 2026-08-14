package ociconf

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestDockerEngineMirrorURLShape drives the URLs Docker Engine ACTUALLY BUILDS at
// Bakery, reconstructed from moby's own code rather than from a doc.
//
// # Why a shape test and not the daemon
//
// The daemon cannot run in CI (it needs root, its own storage driver and a live network
// stack), and it is simultaneously the client with the MOST silent failure mode of the
// four: it is Docker Hub only, it always falls back to Hub on any mirror failure, and it
// forwards the user's real Hub credentials to the mirror on every pull. A broken mirror
// there is invisible twice over. So its URL construction is replicated exactly and
// driven at Bakery as ordinary HTTP -- which is the half that can actually be wrong,
// because the rest of the exchange is the same registry protocol every other client
// speaks.
//
// # The chain, verified in moby v28.5.2
//
//	registry.ValidateMirror  ->  strings.TrimSuffix(mirrorURL, "/") + "/"    (config.go)
//	URLBuilder.cloneRoute    ->  routeURL.Path = routeURL.Path[1:]           (urls.go)
//	                         ->  root.ResolveReference(routeURL)
//
// Docker Engine accepts a mirror WITH A PATH -- only query, fragment and userinfo are
// rejected, and `http://mirror-1.example.com/v1/` is a passing case in moby's own test
// file. That is what makes it a fourth supported client at all, contrary to three
// shipped docs that say mirror URLs must be domain roots.
//
// AND THE WHOLE THING HANGS ON ONE TRAILING SLASH. RFC 3986 reference resolution merges
// a relative reference against the base by discarding everything after the base's LAST
// slash. `http://host/cache/acme/proj/docker/` + `v2/x/manifests/y` keeps the prefix;
// `http://host/cache/acme/proj/docker` + `v2/x/manifests/y` silently drops `docker` and
// produces `/cache/acme/proj/v2/x/manifests/y` -- a 404, a silent Hub fallback, and a
// mirror that appears configured and is not. ValidateMirror is the only thing that
// normalizes it, so any path that skips ValidateMirror loses the segment. Both halves
// are asserted below.
func TestDockerEngineMirrorURLShape(t *testing.T) {
	e := newEnv(t)

	// What an operator writes in daemon.json: "registry-mirrors": ["<this>"]. No trailing
	// slash, because nobody types one.
	configured := e.httpBase + e.mirrorPrefix(projMain)

	mirror, err := validateMirror(configured)
	if err != nil {
		t.Fatalf("moby ValidateMirror(%q): %v", configured, err)
	}

	if !strings.HasSuffix(mirror, "/") {
		t.Fatalf("ValidateMirror returned %q without a trailing slash -- the normalization the "+
			"whole scheme depends on did not happen", mirror)
	}

	// ---- the ping. PingV2Registry builds it from the MIRROR path, not the host root,
	// and harvests its auth challenge from the response exactly as containers/image does.
	ping := mustResolve(t, mirror, "/v2/")

	if got := request(t, http.MethodGet, ping, nil); got.status != http.StatusOK {
		t.Fatalf("Docker Engine ping GET %s = %d, want 200", ping, got.status)
	} else if got.header.Get("WWW-Authenticate") == "" {
		t.Errorf("the mirror-path ping answered 200 with no WWW-Authenticate; a docker-login'd " +
			"engine would conclude the mirror needs no auth and stop authenticating")
	}

	// ---- a manifest pull, at the URL the engine's own builder produces.
	manifest := mustResolve(t, mirror, "/v2/"+manifestPath(repoIndex, tagIndex))

	wantPath := e.containerdPath(projMain) + "/" + manifestPath(repoIndex, tagIndex)
	if u, perr := url.Parse(manifest); perr != nil || u.Path != wantPath {
		t.Fatalf("the engine's manifest URL path = %q, want %q", manifest, wantPath)
	}

	res := request(t, http.MethodGet, manifest, bearer(e.key(projMain)))
	if res.status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200; body %s", manifest, res.status, res.body)
	}

	if digestOf(res.body) != indexDigest {
		t.Errorf("the manifest served to Docker Engine hashes to %s, want %s",
			digestOf(res.body), indexDigest)
	}

	if got := res.header.Get("Docker-Content-Digest"); got != indexDigest {
		t.Errorf("Docker-Content-Digest = %q, want %q", got, indexDigest)
	}

	// ---- the trap, asserted as a property of the CHAIN rather than of Bakery: skip the
	// normalization and the last path segment vanishes. This is documentation with a
	// failing test attached; if RFC 3986 resolution ever stops behaving this way, the
	// warning in client-config.md can be deleted.
	unnormalized := mustResolve(t, configured, "/v2/"+manifestPath(repoIndex, tagIndex))

	if strings.Contains(unnormalized, "/docker/v2/") {
		t.Fatalf("resolving against an unnormalized base produced %q, which still contains the "+
			"mirror prefix -- the trailing-slash trap this test documents no longer exists, "+
			"and the client-config warning should be revisited", unnormalized)
	}

	if got := request(t, http.MethodGet, unnormalized, bearer(e.key(projMain))); got.status != http.StatusNotFound {
		t.Errorf("GET %s (the segment-dropped URL) = %d, want 404 -- an operator who omits the "+
			"trailing slash outside ValidateMirror must get a clean miss, not something worse",
			unnormalized, got.status)
	}
}

// validateMirror is moby v28.5.2's registry.ValidateMirror, reproduced. Only the checks
// that can change the URL are kept; the userinfo branch is included because it is the
// one that would otherwise let a credential into a log line.
func validateMirror(mirrorURL string) (string, error) {
	if scheme, _, ok := strings.Cut(mirrorURL, "://"); !ok || scheme == "" {
		return "", errNoScheme
	}

	uri, err := url.Parse(mirrorURL)
	if err != nil {
		return "", errBadURI
	}

	if uri.Scheme != "http" && uri.Scheme != "https" {
		return "", errBadScheme
	}

	if uri.RawQuery != "" || uri.Fragment != "" {
		return "", errQueryOrFragment
	}

	if uri.User != nil {
		return "", errUserinfo
	}

	// THE LINE THE WHOLE SCHEME RESTS ON.
	return strings.TrimSuffix(mirrorURL, "/") + "/", nil
}

// mustResolve is moby's clonedRoute.URL: strip the route path's leading slash, then
// resolve it as a RELATIVE reference against the mirror base. The leading-slash strip is
// what makes it relative, and therefore what makes the base's path prefix survive.
func mustResolve(t *testing.T, base, routePath string) string {
	t.Helper()

	root, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse mirror base %q: %v", base, err)
	}

	ref := &url.URL{Path: strings.TrimPrefix(routePath, "/")}

	out := root.ResolveReference(ref)
	out.Scheme = root.Scheme

	return out.String()
}

// The ValidateMirror rejections, as sentinels so the reproduction carries no message
// text that could drift from moby's.
var (
	errNoScheme        = mirrorError("no scheme specified")
	errBadURI          = mirrorError("not a valid URI")
	errBadScheme       = mirrorError("unsupported scheme")
	errQueryOrFragment = mirrorError("query or fragment at end of the URI")
	errUserinfo        = mirrorError("username/password not allowed in URI")
)

type mirrorError string

func (e mirrorError) Error() string { return "invalid mirror: " + string(e) }
