package regconf

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/cache/oci"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/metrics"
	"github.com/jsmith212/bakery/internal/server"
	"github.com/jsmith212/bakery/internal/storage"
)

// The one tenant this suite drives, and the literal that carves the writable namespace
// out of the registry route families.
//
// ONE project carries BOTH backends -- `registry` (writable) and `oci` (the
// pull-through mirror, pointed at a counting fake upstream). That is not economy: it is
// spec §8.4's assertion. The two kinds mount overlapping patterns in the same path
// space, distinguished only by ServeMux preferring the literal `buildcache` segment
// over the mirror's `{rest...}`, so the only way to prove a buildcache request never
// reaches the mirror is to make the mirror REACHABLE, on the same project, and count.
const (
	envOrg  = auth.DevOrgSlug
	envProj = "arcturus"

	// segBuildCache is the ref shape's fixed first component. It duplicates an
	// unexported constant in internal/cache/oci deliberately: this package drives the
	// server from OUTSIDE, over the wire, the way a client does.
	segBuildCache = "buildcache"
)

// buildkitImage is what the docker-container driver runs, and what the reachability
// probe runs too so the gate pulls exactly one image.
//
// DELIBERATELY NOT PINNED to the BuildKit commit the spec was read against. The other
// conformance gates pin their client (bitbake 2.8.0) because they assert against a
// specific client's behaviour; this one asserts that the client OPERATORS ACTUALLY HAVE
// can export and import a cache. buildx-stable-1 is the tag `docker buildx create`
// itself defaults to, so pinning anything else would gate against a BuildKit nobody
// runs.
const buildkitImage = "moby/buildkit:buildx-stable-1"

// listenSpec is where the harness binds and what address it ADVERTISES -- the address
// buildkitd is told to dial, which is also the registry host in every
// `--cache-to`/`--cache-from` ref and the host in the Bearer realm.
//
// The two coincide on a native dockerd (every CI runner): `--network=host` puts
// buildkitd on the same loopback as this process, so 127.0.0.1 is correct AND minimal --
// nothing is exposed off the machine. They do not coincide where the daemon runs in its
// own VM (Docker Desktop), and which case applies is PROBED, never assumed. See
// dockerListenSpec.
type listenSpec struct {
	// network is "tcp4" for the wildcard bind, and that is not a preference.
	// net.Listen answers a "tcp" wildcard with a DUAL-STACK [::] socket, and Docker
	// Desktop's host proxy cannot reach one: the forward that carries the host back to
	// this machine follows an AF_INET socket and nothing else. Measured -- the same
	// listener, the same container, the same wget, reached on "tcp4" and refused on
	// "tcp".
	network string
	bind    string

	// advertise is one of THIS MACHINE'S OWN addresses, discovered by the probe, and
	// never a name.
	//
	// It must be reachable from TWO places, which is the finding that shaped this whole
	// mechanism: buildkitd performs the registry requests, but the TOKEN request is
	// performed by the buildx CLI in this process's machine -- BuildKit's session auth
	// provider runs client-side and buildkitd asks it over the session for every token.
	// So an address that only the daemon can reach authenticates nothing: the manifest
	// HEAD arrives, answers 401 with a realm, and the client then times out dialing a
	// realm it cannot route to. That is why host.docker.internal is NOT a candidate
	// however well a container reaches it, and why a literal address beats a name:
	// resolved in the daemon's VM, one name can mean several addresses, and BuildKit
	// picked a working one for the fetch and a dead one for the token.
	advertise string
}

// authority renders "<advertised address>:<port>" for a bound listener.
func (s listenSpec) authority(ln net.Listener) string {
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return ln.Addr().String()
	}

	return net.JoinHostPort(s.advertise, port)
}

// loopback is the spec for clients that run on THIS machine: skopeo, the raw HTTP push
// helpers, everything but buildkitd.
func loopback() listenSpec {
	return listenSpec{network: "tcp", bind: "127.0.0.1:0", advertise: "127.0.0.1"}
}

// probeScript runs INSIDE a --network=host container: the candidate addresses, tried in
// order, and the first one that answers is printed on stdout.
//
// Every candidate is an address of THIS machine (see hostAddrs), so whatever it prints
// is reachable from this process too -- which the token leg requires. wget's exit status
// is the whole test; nothing but the winning address reaches stdout.
const probeScript = `
for addr in %s; do
  if wget -q -T 3 -O /dev/null "http://$addr:%s/"; then echo "$addr"; exit 0; fi
done
exit 1
`

// hostAddrs is the candidate list, in preference order: loopback, then this machine's
// own routable IPv4 addresses.
//
// 127.0.0.1 first because on a native daemon it is both the answer and the one that
// exposes nothing off the machine. The routable addresses exist for the daemon-in-a-VM
// case, where the VM cannot reach this machine's loopback but can reach its interface --
// and where those addresses still work for the client-side token leg, because they are
// this machine's.
func hostAddrs(t *testing.T) []string {
	t.Helper()

	addrs := []string{"127.0.0.1"}

	ifaces, err := net.Interfaces()
	if err != nil {
		t.Logf("enumerate interfaces: %v", err)

		return addrs
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		bound, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, a := range bound {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}

			if ip4 := ipNet.IP.To4(); ip4 != nil && ip4.IsGlobalUnicast() {
				addrs = append(addrs, ip4.String())
			}
		}
	}

	return addrs
}

// dockerListenSpec finds an address the docker daemon's host network can reach, or
// skips.
//
// It is a real container making real requests at a real listener, because every cheaper
// test is a guess: `docker info` does not say whether host networking shares this
// process's namespace, and a daemon reached over a socket may be anywhere. The probe
// costs one or two container starts and it is the difference between a gate that hangs
// for a minute per build on an auth timeout and one that says, up front, that this
// daemon cannot reach this test.
//
// The loopback bind is tried FIRST and separately from the wildcard bind, so a machine
// where loopback works never exposes this test server on another interface.
//
// A skip here FAILS `just registry-conformance`, which is right: on a CI runner the
// loopback candidate always wins, so reaching the skip means the environment changed
// under the gate.
func dockerListenSpec(t *testing.T, docker string) listenSpec {
	t.Helper()

	for _, spec := range []listenSpec{
		{network: "tcp", bind: "127.0.0.1:0"},
		{network: "tcp4", bind: "0.0.0.0:0"},
	} {
		if addr, ok := probeFromDocker(t, docker, spec); ok {
			spec.advertise = addr

			t.Logf("reachability probe: %s bound on %s is reachable from a container at %s",
				spec.network, spec.bind, addr)

			return spec
		}
	}

	t.Skip(skipMsg("no address this test can bind is reachable from a --network=host " +
		"container on this docker daemon. The docker-container driver runs buildkitd " +
		"inside the daemon, so buildkitd cannot dial this test's listener and no export " +
		"can land."))

	return listenSpec{}
}

// probeFromDocker binds a throwaway listener the way newEnv will, then asks a
// --network=host container -- the same network mode the builder uses -- which address it
// can reach that listener by.
func probeFromDocker(t *testing.T, docker string, spec listenSpec) (string, bool) {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), spec.network, spec.bind)
	if err != nil {
		t.Logf("reachability probe: cannot bind %s %s: %v", spec.network, spec.bind, err)

		return "", false
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the throwaway httptest listener: %v", err)
	}

	srv.Listener = ln
	srv.Start()

	defer srv.Close()

	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split the probe listener address %q: %v", ln.Addr().String(), err)
	}

	script := fmt.Sprintf(probeScript, strings.Join(hostAddrs(t), " "), port)

	// Retried, because a listener becoming reachable is not instantaneous everywhere:
	// where the daemon is not in this network namespace, a port forward has to notice
	// the new socket first. One attempt would make this gate's availability depend on
	// that race.
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(time.Second)
		}

		cmd := exec.CommandContext(t.Context(), docker, "run", "--rm", "--network=host",
			"--entrypoint", "/bin/sh", buildkitImage, "-c", script)

		var stderr strings.Builder

		cmd.Stderr = &stderr

		out, err := cmd.Output()
		if err == nil {
			return strings.TrimSpace(string(out)), true
		}

		t.Logf("reachability probe on %s port %s: %v (%s)",
			spec.bind, port, err, strings.TrimSpace(stderr.String()))
	}

	return "", false
}

// env is a booted Bakery tree carrying BOTH OCI-shaped backends for one project.
//
// Nothing here is a lookalike. The HTTP tree is the real server.NewHandler -- the same
// function `bakery serve` runs, middleware and the /cache and /v2 catch-alls included --
// carrying the real oci.BuildCache and the real oci.Backend over the real blob.Service
// against a real, ephemeral Postgres. The org, the project, both backend rows and both
// API keys are created by driving the production HTTP API, exactly as a human would in
// the console.
type env struct {
	// host is the ADVERTISED authority -- "<host>:<port>" -- which is what a registry
	// client uses as the registry name, what buildkitd.toml keys its `http = true` on,
	// and what oci.Config.ExternalURL bakes into every Bearer realm. It is known BEFORE
	// the handler is built because the realm must be an absolute URL, and a realm the
	// clients cannot resolve is the one bug that reproduces only in a deployment.
	host string

	// local is the base URL THIS PROCESS dials, and it is always loopback. The
	// advertised address is one of this machine's own, so it would work too -- but the
	// harness's control-plane calls and push helpers have no reason to leave the
	// loopback interface, and a helper that failed because a host firewall disliked its
	// own routable address would look exactly like a Bakery bug. On the wildcard bind
	// the two differ; on the loopback bind they are the same string.
	local string

	// up is the pull-through mirror's fake upstream, mounted on THIS project. Its
	// request log is spec §8.4: the writable namespace has no upstream by construction,
	// and this counter is what proves no buildcache request ever fell through to the
	// mirror that does.
	up *fakeUpstream

	// rec wraps the public handler and records every request, which is what lets a test
	// assert that BuildKit's export really pushed (201s on the write verbs) rather than
	// that its build happened to be green -- and with ignore-error=true in the snippet,
	// green is exactly what a totally broken cache also produces.
	rec *recorder

	// writeKey and readKey are both `bkry_` tokens for the SAME project. One opaque
	// token, not an id:secret pair, so each goes verbatim into a Basic password, a Basic
	// username or a Bearer header alike.
	writeKey string
	readKey  string
}

// newEnv boots the server and provisions the tenant through the real control-plane API.
//
// It must NOT be called before the require* guards: dbtest.New spawns a Postgres (or
// clones a template on TEST_DB_URL), and `just race`/`just coverage` glob ./... -- so on
// a runner with no docker this package is compiled and run, and the client-driven tests
// have to cost nothing when they cannot prove anything.
func newEnv(t *testing.T, spec listenSpec) *env {
	t.Helper()

	ctx := t.Context()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The fake upstream first: its host goes into the mirror's config, so it has to
	// exist before the control-plane calls at the bottom of this function.
	up := newFakeUpstream(t)

	pool := dbtest.New(t)
	store := db.NewStore(pool)
	m := metrics.New()

	sessions := auth.NewSessionManager(auth.NewSessionStore(pool, log), false)

	authSvc, err := auth.New(auth.Deps{
		Store: store, Sessions: sessions, Provider: nil, Groups: nil,
		Metrics: m, Log: log, DevLogin: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	if err := authSvc.SeedDevLogin(ctx); err != nil {
		t.Fatalf("seed dev login: %v", err)
	}

	apiSrv, err := api.New(api.Config{
		Store: store, Auth: authSvc, Metrics: m, Log: log,
		AllowSelfServeOrgs: true, AllowLocalSiteAdmins: true,
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewLocal: %v", err)
	}

	blobs, err := blob.New(blob.Config{
		Reader:  store,
		Tx:      store,
		Storage: storage.NewInstrumented(local, m, metrics.DriverLocal),
		Metrics: m,
	})
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	deps := cache.Deps{Blobs: blobs, Metrics: m, Logger: log}
	if err := deps.Validate(); err != nil {
		t.Fatalf("cache.Deps.Validate: %v", err)
	}

	// Bind the public listener BEFORE building the handler. oci.Config.ExternalURL has
	// to be an absolute URL the CLIENTS can reach -- and here "the clients" includes a
	// buildkitd in a container, which is why the advertised authority comes from the
	// probed listenSpec and not from ln.Addr().
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, spec.network, spec.bind)
	if err != nil {
		t.Fatalf("bind public listener on %s: %v", spec.bind, err)
	}

	host := spec.authority(ln)
	base := "http://" + host

	// What THIS process dials. The wildcard bind the desktop candidate uses also
	// answers on loopback, so 127.0.0.1 is always right here even when the advertised
	// name is not.
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split the listener address %q: %v", ln.Addr().String(), err)
	}

	localBase := "http://" + net.JoinHostPort("127.0.0.1", port)

	ociUp, err := oci.NewRegistry(oci.Config{ExternalURL: base}, m)
	if err != nil {
		t.Fatalf("oci.NewRegistry: %v", err)
	}

	routes := httpblob.NewCachedResolver(store, log)

	// The mirror. It is here to be COUNTED, not to serve (see fakeUpstream) -- and it
	// is also the subject's routing-shadow fallback, exactly as boot wires it.
	mirror := oci.New(deps, routes, ociAuth{svc: authSvc}, ociUp, oci.Config{ExternalURL: base})

	backends := []cache.Backend{
		mirror,

		// The subject. No Fetcher parameter exists to pass; the mirror is the read
		// fallback for projects with no registry backend, not an upstream.
		oci.NewBuildCache(deps, routes, ociAuth{svc: authSvc}, oci.Config{ExternalURL: base}, mirror),
	}

	rec := &recorder{next: server.NewHandler(server.Config{
		API:           apiSrv.Handler(),
		CacheBackends: backends,
		Headless:      true, // no SPA: there is no embedded dist in a test binary.
		Pool:          pool,
	})}

	srv := httptest.NewUnstartedServer(rec)

	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close the throwaway httptest listener: %v", err)
	}

	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	c := newAPIClient(t, localBase)
	c.devLogin(t)
	c.createProject(t, envProj)

	// read_auth_required on BOTH: the private shape, and the only one that makes a
	// client run the Bearer token dance at all. An open backend never 401s, so BuildKit
	// would never authenticate and the auth half of this gate would prove nothing.
	c.createBackend(t, envProj, "registry", true, `{}`)
	c.createBackend(t, envProj, "oci", true, fmt.Sprintf(
		`{"default_upstream":%q,"upstreams":[%q],"tag_ttl":"1h"}`, up.host, up.host))

	e := &env{
		host: host, local: localBase, up: up, rec: rec,
		writeKey: c.createKey(t, envProj, "write"),
		readKey:  c.createKey(t, envProj, "read"),
	}

	// The control-plane calls above all hit /api/v1/... and are recorded too. Drop them
	// so a test's recorder assertions see only the registry clients' own traffic.
	rec.reset()

	return e
}

// ref is a BuildKit `--cache-to`/`--cache-from` reference:
// <host>/{org}/{project}/buildcache/<repo>:<tag>.
func (e *env) ref(repo, tag string) string {
	return e.host + "/" + envOrg + "/" + envProj + "/" + segBuildCache + "/" + repo + ":" + tag
}

// repoURL is the same repository as a URL on the second route family -- the one
// BuildKit, podman and skopeo all use, where the tenant prefix lands in the repository
// position after /v2.
func (e *env) repoURL(repo string) string {
	return e.local + "/v2/" + envOrg + "/" + envProj + "/" + segBuildCache + "/" + repo
}

// assertNoUpstream is spec §8.4, and it is the assertion every test in this package
// ends with.
//
// The writable namespace has no upstream by CONSTRUCTION -- NewBuildCache takes no
// Fetcher -- so this cannot fail through the buildcache handler. What it can catch is a
// ROUTING regression: the mirror is mounted on this same project with a real upstream
// allowlist, and a buildcache request that fell through to the mirror's `{rest...}`
// pattern would fetch, and would show up here and nowhere else.
func (e *env) assertNoUpstream(t *testing.T) {
	t.Helper()

	if n := e.up.count(); n != 0 {
		t.Errorf("the fake upstream saw %d request(s) during a buildcache-only gate: %v\n"+
			"the writable namespace has no upstream -- a request reaching one means a "+
			"buildcache URL was routed to the pull-through mirror", n, e.up.requests())
	}
}

// ---------------------------------------------------------------------------
// The fake upstream: a counter with an HTTP interface.
// ---------------------------------------------------------------------------

// fakeUpstream is the mirror backend's configured upstream. It holds NOTHING and
// answers 404 to everything, on purpose: its whole job is to be counted. A registry
// that served content could make a routing bug look like a successful pull.
type fakeUpstream struct {
	host string

	mu   sync.Mutex
	reqs []string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()

	up := &fakeUpstream{}

	srv := httptest.NewServer(http.HandlerFunc(up.serve))
	t.Cleanup(srv.Close)

	up.host = strings.TrimPrefix(srv.URL, "http://")

	return up
}

func (u *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.reqs = append(u.reqs, r.Method+" "+r.URL.Path)
	u.mu.Unlock()

	http.NotFound(w, r)
}

func (u *fakeUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return len(u.reqs)
}

func (u *fakeUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()

	out := make([]string, len(u.reqs))
	copy(out, u.reqs)

	return out
}

// ---------------------------------------------------------------------------
// The widening adapter boot.go declares, replicated here so the harness wires the same
// auth surface production does.
// ---------------------------------------------------------------------------

// ociAuth widens *auth.Service to oci.Authenticator. It takes a bare TOKEN, not an
// *http.Request, and that asymmetry is a security property: the shape gate inside the
// oci package is what keeps a forwarded foreign registry credential from ever reaching
// a database probe or a log line.
type ociAuth struct{ svc *auth.Service }

func (a ociAuth) AuthenticateToken(ctx context.Context, token string) (oci.Principal, error) {
	return a.svc.AuthenticateToken(ctx, token)
}

// ---------------------------------------------------------------------------
// The recorder: the network truth every assertion in this package rests on.
// ---------------------------------------------------------------------------

type recordedReq struct {
	method string
	path   string
	status int
}

// recorder wraps the public handler and captures method, path and status.
//
// It is load-bearing here in a way it is not in a normal test. A cache export failure
// is invisible to the client by design (the snippet carries ignore-error=true, which is
// what stops a Bakery outage from failing an arcturus build), so the build's exit status
// cannot distinguish "exported" from "silently gave up". The recorded status codes can.
type recorder struct {
	next http.Handler
	mu   sync.Mutex
	reqs []recordedReq
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w}
	rec.next.ServeHTTP(sw, r)

	rec.mu.Lock()
	rec.reqs = append(rec.reqs, recordedReq{
		method: r.Method, path: r.URL.Path, status: sw.statusCode(),
	})
	rec.mu.Unlock()
}

func (rec *recorder) reset() {
	rec.mu.Lock()
	rec.reqs = nil
	rec.mu.Unlock()
}

func (rec *recorder) snapshot() []recordedReq {
	rec.mu.Lock()
	defer rec.mu.Unlock()

	out := make([]recordedReq, len(rec.reqs))
	copy(out, rec.reqs)

	return out
}

// count returns how many recorded requests match a predicate.
func (rec *recorder) count(match func(recordedReq) bool) int {
	n := 0

	for _, r := range rec.snapshot() {
		if match(r) {
			n++
		}
	}

	return n
}

// summary renders the recorded traffic for a failure message. A push that went wrong is
// a SEQUENCE that went wrong, and the sequence is the only thing worth printing.
func (rec *recorder) summary() string {
	var b strings.Builder

	for _, r := range rec.snapshot() {
		fmt.Fprintf(&b, "\n    %-6s %-3d %s", r.method, r.status, r.path)
	}

	return b.String()
}

// isBuildCache matches a recorded request against the writable namespace, on either
// route family and whatever repository it named.
func isBuildCache(r recordedReq) bool {
	return strings.Contains(r.path, "/"+segBuildCache+"/")
}

// isWriteVerb matches the verbs the push API is made of. GET and HEAD are reads, and
// only these two can create or mutate anything.
func isWriteVerb(r recordedReq) bool {
	return r.method == http.MethodPost || r.method == http.MethodPut
}

// statusWriter captures the status code without disturbing the response. It
// deliberately does not forward ReadFrom, so the sendfile fast path is bypassed on blob
// reads -- correctness is unaffected and a conformance gate does not need zero-copy.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}

	return sw.ResponseWriter.Write(b)
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statusWriter) statusCode() int {
	if sw.status == 0 {
		return http.StatusOK
	}

	return sw.status
}

// ---------------------------------------------------------------------------
// The control-plane client: the console's flow, over HTTP.
// ---------------------------------------------------------------------------

type apiClient struct {
	base   string
	client *http.Client
}

func newAPIClient(t *testing.T, base string) *apiClient {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}

	return &apiClient{base: base, client: &http.Client{Jar: jar}}
}

func (c *apiClient) do(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	r, err := http.NewRequestWithContext(t.Context(), method, c.base+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(r)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}

	return resp.StatusCode, raw
}

func (c *apiClient) devLogin(t *testing.T) {
	t.Helper()

	status, body := c.do(t, http.MethodPost, api.Prefix+"/auth/dev-login", "")
	if status != http.StatusOK {
		t.Fatalf("dev-login: status = %d, body %s", status, body)
	}
}

func (c *apiClient) createProject(t *testing.T, project string) {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects", api.Prefix, envOrg)
	body := fmt.Sprintf(`{"slug":%q,"name":%q}`, project, project)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create project %s: status = %d, body %s", project, status, raw)
	}
}

// createBackend writes one backend row. The `registry` kind's config is EMPTY in v1 and
// that is the design: there is no upstream, so there is no allowlist, no default and no
// ?ns= resolution to configure.
func (c *apiClient) createBackend(t *testing.T, project, kind string, readAuth bool, config string) {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects/%s/backends", api.Prefix, envOrg, project)
	body := fmt.Sprintf(`{"kind":%q,"enabled":true,"read_auth_required":%t,"config":%s}`,
		kind, readAuth, config)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create %s backend for %s: status = %d, body %s", kind, project, status, raw)
	}
}

// createKey mints a key through the real minting path and returns the plaintext EXACTLY
// once -- there is no second way to read it, by design.
func (c *apiClient) createKey(t *testing.T, project, scope string) string {
	t.Helper()

	path := fmt.Sprintf("%s/orgs/%s/projects/%s/keys", api.Prefix, envOrg, project)
	body := fmt.Sprintf(`{"name":"registry-conformance-%s","scope":%q}`, scope, scope)

	status, raw := c.do(t, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("create %s key for %s: status = %d, body %s", scope, project, status, raw)
	}

	var created struct {
		Token string `json:"token"`
		Scope string `json:"scope"`
	}

	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode the created key: %v (body %s)", err, raw)
	}

	if !strings.HasPrefix(created.Token, auth.TokenPrefix) || created.Scope != scope {
		t.Fatalf("minted key = %q scope %q, want a %s token with scope %s",
			created.Token, created.Scope, auth.TokenPrefix, scope)
	}

	return created.Token
}
