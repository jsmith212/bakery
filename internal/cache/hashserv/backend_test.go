package hashserv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db"
	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// These tests drive the backend over a REAL WebSocket with a RAW client, deliberately not
// reusing our own upstream.go client. A client and a server written by the same hand can share
// a mutual misunderstanding of the wire and agree with each other perfectly while agreeing with
// bitbake not at all. Here the frames are spelled out by hand, so what is asserted is the wire.

// fakePrincipal answers the only two questions a cache credential can ever answer.
type fakePrincipal struct{ read, write bool }

func (p fakePrincipal) CanReadProject(pgtype.UUID, pgtype.UUID) bool  { return p.read }
func (p fakePrincipal) CanWriteProject(pgtype.UUID, pgtype.UUID) bool { return p.write }

// fakeAuthenticator maps a token to a principal, and anything else to an error.
type fakeAuthenticator map[string]fakePrincipal

func (a fakeAuthenticator) AuthenticateToken(_ context.Context, token string) (Principal, error) {
	if p, ok := a[token]; ok {
		return p, nil
	}

	return nil, errBadToken
}

type staticRoutes struct{ route cache.Route }

func (s staticRoutes) Resolve(context.Context, string, string, repository.BackendKind) (cache.Route, bool) {
	if s.route.BackendID == 0 {
		return cache.Route{}, false
	}

	return s.route, true
}

const writeToken = "bkry_wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww"
const readToken = "bkry_rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"

// rawClient is a hand-rolled hashserv client: it writes the exact WebSocket messages bitbake
// writes, and nothing more.
type rawClient struct {
	t   *testing.T
	c   *websocket.Conn
	ctx context.Context
}

func (r *rawClient) send(msg string) {
	r.t.Helper()

	if err := r.c.Write(r.ctx, websocket.MessageText, []byte(msg)); err != nil {
		r.t.Fatalf("write %q: %v", msg, err)
	}
}

func (r *rawClient) sendJSON(v any) {
	r.t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		r.t.Fatalf("marshal: %v", err)
	}

	r.send(string(b))
}

func (r *rawClient) recv() string {
	r.t.Helper()

	typ, data, err := r.c.Read(r.ctx)
	if err != nil {
		r.t.Fatalf("read: %v", err)
	}

	if typ != websocket.MessageText {
		r.t.Fatalf("got %v frame, want text", typ)
	}

	return string(data)
}

// handshake sends the opening exchange the way bitbake does: ONE MESSAGE PER LINE, no newlines,
// terminated by an EMPTY MESSAGE. If the server expected a newline-delimited blob instead, this
// call would block until the test timed out -- which is exactly how it would fail in production,
// except there it would be someone's build hanging for four hours.
func (r *rawClient) handshake() {
	r.t.Helper()

	r.send("OEHASHEQUIV 1.1")
	r.send("needs-headers: false")
	r.send("")
}

// newBackend stands the real backend up behind httptest over a real migrated Postgres.
func newBackend(t *testing.T, readAuthRequired bool) (*httptest.Server, cache.Route) {
	t.Helper()

	pool := dbtest.New(t)
	s := db.NewStore(pool)
	ctx := t.Context()

	org, err := s.CreateOrganization(ctx, repository.CreateOrganizationParams{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}

	project, err := s.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "widget", Name: "Widget",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	backend, err := s.CreateBackend(ctx, repository.CreateBackendParams{
		ProjectID:        project.ID,
		Kind:             repository.BackendKindHashserv,
		Enabled:          true,
		ReadAuthRequired: readAuthRequired,
		Config:           []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateBackend: %v", err)
	}

	route := cache.Route{
		OrgID: org.ID, ProjectID: project.ID,
		Org: "acme", Project: "widget",
		BackendID: backend.ID, Kind: repository.BackendKindHashserv,
		Enabled: true, ReadAuthRequired: readAuthRequired,
		Config: []byte(`{}`),
	}

	deps := cache.Deps{Blobs: &blob.Service{}, Metrics: metrics.New(), Logger: discardLogger()}

	authn := fakeAuthenticator{
		writeToken: {read: true, write: true},
		readToken:  {read: true},
	}

	b := New(deps, staticRoutes{route: route}, authn, s, nil)

	mux := http.NewServeMux()
	b.Register(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv, route
}

func dial(t *testing.T, srv *httptest.Server) *rawClient {
	t.Helper()

	ctx := t.Context()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/cache/acme/widget/hashserv"

	c, resp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// The trap, from the other side. coder/websocket's 32 KiB default applies to READS, so a
	// client that does not raise it cannot receive a get-outhash carrying a 128 KiB siginfo --
	// which is a response the server is entitled to send. Python's websockets defaults to 1 MiB,
	// which is why MaxMessageBytes is 1 MiB and not more: we must not accept a request we could
	// never serve back.
	c.SetReadLimit(MaxMessageBytes)

	// THE ASSERTION THAT MATTERS MOST ON THIS PATH. A stock bitbake client sends no
	// Authorization header on the upgrade -- credentials go in band, in the `auth` RPC, after
	// this point. So the upgrade must succeed even with no credential whatsoever. A server that
	// answered 401 here would be caught by the Python client as a ConnectionError, warned about
	// by bb.siggen, and the build would COMPLETE with unihash = taskhash and a silently
	// degraded cache.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101: auth is denied IN BAND, never at the upgrade",
			resp.StatusCode)
	}

	t.Cleanup(func() { _ = c.CloseNow() })

	return &rawClient{t: t, c: c, ctx: ctx}
}

// TestUpgradeCarriesNoCredentialAndStillSucceeds pins finding 2: the stock client sends no
// Authorization header, so gating the upgrade on one rejects the connection before the client
// has had any chance to present it.
func TestUpgradeCarriesNoCredentialAndStillSucceeds(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, true) // read auth REQUIRED, and the upgrade still must not 401

	c := dial(t, srv)
	c.handshake()
	c.sendJSON(map[string]any{"ping": map[string]any{}})

	if got := c.recv(); got != `{"alive":true}` {
		t.Fatalf("ping = %s, want {\"alive\":true}", got)
	}
}

// TestAuthIsDeniedInBand is the single most important behavioral test in this package.
//
// An unauthenticated client on a private backend must be refused with {"invoke-error": ...} --
// which raises InvokeError in bitbake, is caught nowhere on the build path, and HALTS the build.
// The alternative -- a 401 at the upgrade -- produces a build that goes GREEN with a silently
// poisoned cache. Loud beats silent.
func TestAuthIsDeniedInBand(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, true)

	c := dial(t, srv)
	c.handshake()

	// ping is free, as it is upstream: it tells you nothing.
	c.sendJSON(map[string]any{"ping": map[string]any{}})
	c.recv()

	// The first RPC that needs @read is refused, in band.
	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})

	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(c.recv()), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if _, ok := resp["invoke-error"]; !ok {
		t.Fatalf("response = %v, want an invoke-error: a denial MUST be in-band and build-halting",
			resp)
	}
}

func TestBadCredentialIsDeniedInBand(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, true)

	c := dial(t, srv)
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": "bkry_notarealtokenatall"}})

	if got := c.recv(); !strings.Contains(got, "invoke-error") {
		t.Fatalf("auth with a bad token = %s, want an invoke-error", got)
	}
}

// TestAuthAcceptsTheTokenInEitherField: a Bakery cache credential is ONE opaque bkry_ token,
// not an id:secret pair. There is no secret half to split off, so it must authenticate whether
// the client put it in the token field or the username field.
func TestAuthAcceptsTheTokenInEitherField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		auth map[string]any
	}{
		{name: "token field", auth: map[string]any{"username": "anything", "token": writeToken}},
		{name: "username field", auth: map[string]any{"username": writeToken, "token": ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, _ := newBackend(t, true)

			c := dial(t, srv)
			c.handshake()
			c.sendJSON(map[string]any{"auth": tt.auth})

			var resp struct {
				Result      bool     `json:"result"`
				Permissions []string `json:"permissions"`
			}

			if err := json.Unmarshal([]byte(c.recv()), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if !resp.Result {
				t.Fatal("auth failed; the token must authenticate from either field")
			}

			want := []string{"@read", "@report", "@db-admin"}
			if !equalStrings(resp.Permissions, want) {
				t.Errorf("permissions = %v, want %v", resp.Permissions, want)
			}
		})
	}
}

// TestReadScopedKeyCannotReport: a read-scoped key gets @read and NOTHING else. Its report must
// take the read-only path -- answered, never written.
func TestReadScopedKeyCannotReport(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, true)

	c := dial(t, srv)
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": readToken}})

	var resp struct {
		Permissions []string `json:"permissions"`
	}

	if err := json.Unmarshal([]byte(c.recv()), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !equalStrings(resp.Permissions, []string{"@read"}) {
		t.Fatalf("permissions = %v, want only @read", resp.Permissions)
	}

	// The report is answered (echoed back), not refused -- and not written.
	c.sendJSON(map[string]any{"report": map[string]any{
		"method": testMethod, "taskhash": taskhash1, "outhash": outhash1, "unihash": unihash1,
	}})

	if got := c.recv(); !strings.Contains(got, unihash1) {
		t.Fatalf("read-only report = %s, want the echoed unihash", got)
	}

	// Prove it did not write: a fresh get must miss.
	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})

	if got := c.recv(); got != "null" {
		t.Fatalf("get after a read-only report = %s, want null: the report must NEVER have written", got)
	}

	// And `remove` -- a @db-admin op -- is refused outright.
	c.sendJSON(map[string]any{"remove": map[string]any{"where": map[string]string{"method": testMethod}}})

	if got := c.recv(); !strings.Contains(got, "invoke-error") {
		t.Fatalf("remove with a read key = %s, want an invoke-error", got)
	}
}

// TestReportThenGetRoundTrip drives the happy path over the real wire.
func TestReportThenGetRoundTrip(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, true)

	c := dial(t, srv)
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	c.sendJSON(map[string]any{"report": map[string]any{
		"method": testMethod, "taskhash": taskhash1, "outhash": outhash1, "unihash": unihash1,
	}})

	var rep unihashResponse
	if err := json.Unmarshal([]byte(c.recv()), &rep); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}

	if rep.Unihash != unihash1 {
		t.Fatalf("report = %s, want %s", rep.Unihash, unihash1)
	}

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})

	var got unihashResponse
	if err := json.Unmarshal([]byte(c.recv()), &got); err != nil {
		t.Fatalf("unmarshal get: %v", err)
	}

	if got.Unihash != unihash1 {
		t.Fatalf("get = %s, want %s", got.Unihash, unihash1)
	}
}

// TestGetStreamOverTheWire drives the hot path exactly as bitbake does: enter stream mode,
// PIPELINE a burst of lines without waiting, then read the replies back and check they line up.
//
// The client's Batch asserts it received exactly as many results as it sent. A server that drops
// one reply, or reorders two, misaligns that list -- and every subsequent lookup then returns the
// PREVIOUS task's unihash. That is not an error, it is a wrong sstate object, silently.
func TestGetStreamOverTheWire(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, true)

	c := dial(t, srv)
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	// Seed one known hash; everything else will miss.
	c.sendJSON(map[string]any{"report": map[string]any{
		"method": testMethod, "taskhash": taskhash1, "outhash": outhash1, "unihash": unihash1,
	}})
	c.recv()

	c.sendJSON(map[string]any{"get-stream": nil})

	// Entry is JSON-QUOTED. Exit will be raw. Getting these the same way round is the classic
	// silent hang.
	if got := c.recv(); got != `"ok"` {
		t.Fatalf("stream entry = %s, want the JSON-quoted \"ok\"", got)
	}

	// Pipeline: send everything, then read everything.
	const n = 50

	want := make([]string, 0, n)

	for i := range n {
		if i == 25 {
			c.send(testMethod + " " + taskhash1)
			want = append(want, unihash1)

			continue
		}

		c.send(testMethod + " " + strings.Repeat("0", 63) + string(rune('a'+i%16)))
		want = append(want, "") // a miss is an EMPTY MESSAGE
	}

	for i := range n {
		if got := c.recv(); got != want[i] {
			t.Fatalf("stream reply %d = %q, want %q -- responses must be strictly ordered, one per request",
				i, got, want[i])
		}
	}

	c.send("END")

	// Exit is RAW, unquoted.
	if got := c.recv(); got != "ok" {
		t.Fatalf("stream exit = %q, want the raw unquoted ok", got)
	}

	// And the connection is back in normal mode.
	c.sendJSON(map[string]any{"ping": map[string]any{}})

	if got := c.recv(); got != `{"alive":true}` {
		t.Fatalf("ping after stream = %s; the connection did not return to normal mode", got)
	}
}

// TestHugeSiginfo is upstream's test_huge_message, and it is the test that catches a missing
// SetReadLimit. coder/websocket defaults to a 32768-byte read limit; this message is 131072
// bytes of outhash_siginfo in ONE frame, because WebSocket messages are not chunked. Without
// SetReadLimit the connection dies with StatusMessageTooBig and the build hangs.
func TestHugeSiginfo(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, true)

	c := dial(t, srv)
	c.handshake()
	c.sendJSON(map[string]any{"auth": map[string]any{"username": "u", "token": writeToken}})
	c.recv()

	// 32 KiB * 4 -- upstream's own figure.
	siginfo := strings.Repeat("0", 32*1024*4)

	c.sendJSON(map[string]any{"report": map[string]any{
		"method": testMethod, "taskhash": taskhash1, "outhash": outhash1, "unihash": unihash1,
		"outhash_siginfo": siginfo,
	}})

	var rep unihashResponse
	if err := json.Unmarshal([]byte(c.recv()), &rep); err != nil {
		t.Fatalf("unmarshal: %v; a 128 KiB frame was rejected -- SetReadLimit is missing", err)
	}

	if rep.Unihash != unihash1 {
		t.Fatalf("report = %s, want %s", rep.Unihash, unihash1)
	}

	// Read it back out: get-outhash returns the siginfo, and it must survive the round trip whole.
	c.sendJSON(map[string]any{"get-outhash": map[string]any{
		"method": testMethod, "outhash": outhash1, "taskhash": taskhash1, "with_unihash": true,
	}})

	var out outhashResponse
	if err := json.Unmarshal([]byte(c.recv()), &out); err != nil {
		t.Fatalf("unmarshal get-outhash: %v", err)
	}

	if out.OuthashSiginfo != siginfo {
		t.Fatalf("siginfo round-trip lost data: got %d bytes, want %d",
			len(out.OuthashSiginfo), len(siginfo))
	}
}

// TestAnonymousReadOnOpenMirror: read_auth_required = false means anyone may read -- and no one
// may write. There is no configuration that grants an anonymous connection @report.
func TestAnonymousReadOnOpenMirror(t *testing.T) {
	t.Parallel()

	srv, _ := newBackend(t, false)

	c := dial(t, srv)
	c.handshake()

	// A read succeeds with no credential at all.
	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})

	if got := c.recv(); got != "null" {
		t.Fatalf("anonymous get = %s, want null (a miss, not a refusal)", got)
	}

	// A report is answered but NOT written -- the read-only path.
	c.sendJSON(map[string]any{"report": map[string]any{
		"method": testMethod, "taskhash": taskhash1, "outhash": outhash1, "unihash": unihash1,
	}})
	c.recv()

	c.sendJSON(map[string]any{"get": map[string]any{"method": testMethod, "taskhash": taskhash1}})

	if got := c.recv(); got != "null" {
		t.Fatalf("get after an anonymous report = %s, want null: an unauthenticated write is a "+
			"cache-poisoning vector and must not be representable", got)
	}

	// And remove is refused.
	c.sendJSON(map[string]any{"remove": map[string]any{"where": map[string]string{"method": testMethod}}})

	if got := c.recv(); !strings.Contains(got, "invoke-error") {
		t.Fatalf("anonymous remove = %s, want an invoke-error", got)
	}
}

// TestUnconfiguredBackendIs404 -- and specifically NOT a websocket upgrade, and not a 500.
func TestUnconfiguredBackendIs404(t *testing.T) {
	t.Parallel()

	deps := cache.Deps{Blobs: &blob.Service{}, Metrics: metrics.New(), Logger: discardLogger()}
	b := New(deps, staticRoutes{}, fakeAuthenticator{}, nil, nil)

	mux := http.NewServeMux()
	b.Register(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/cache/nope/nope/hashserv")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: an unconfigured backend never mounts a mount point it "+
			"cannot serve", resp.StatusCode)
	}
}
