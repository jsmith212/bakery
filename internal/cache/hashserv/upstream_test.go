package hashserv

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jsmith212/bakery/internal/metrics"
)

// ---------------------------------------------------------------------------
// The fake upstream: a REAL WebSocket server speaking the REAL wire protocol.
//
// It is deliberately not a mock. The handshake -- three separate messages, no newlines --
// is the single thing in this file most likely to be wrong, and its failure mode is a
// server that blocks forever with no error on either side. Only a real peer that reads real
// frames can prove the framing; a fake that "receives a handshake" proves nothing.
// ---------------------------------------------------------------------------

type fakeUpstream struct {
	srv    *httptest.Server
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	conns   int
	msgs    []string      // every message received, across all connections, in arrival order
	headers []http.Header // the upgrade headers of each accepted connection
}

// serveFunc handles ONE accepted connection, start to finish.
type serveFunc func(s *wsSession)

func newFakeUpstream(t *testing.T, serve serveFunc) *fakeUpstream {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeUpstream{ctx: ctx, cancel: cancel}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("fake upstream: accept: %v", err)

			return
		}

		defer func() { _ = ws.CloseNow() }()

		ws.SetReadLimit(MaxMessageBytes)

		f.wg.Add(1)
		defer f.wg.Done()

		f.mu.Lock()
		f.conns++
		index := f.conns
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()

		serve(&wsSession{t: t, f: f, ws: ws, index: index})
	}))

	// The cleanup order matters. httptest.Server.Close does NOT wait for hijacked
	// connections, so without the cancel+wait below a serve goroutine could call t.Errorf
	// after the test completed, which panics. Cancelling unblocks every read; wg.Wait then
	// joins the goroutines while the test is still alive.
	t.Cleanup(func() {
		f.cancel()
		f.srv.Close()
		f.wg.Wait()
	})

	return f
}

func (f *fakeUpstream) url() string {
	return "ws" + strings.TrimPrefix(f.srv.URL, "http")
}

func (f *fakeUpstream) connCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.conns
}

func (f *fakeUpstream) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.msgs...)
}

func (f *fakeUpstream) upgradeHeaders() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]http.Header(nil), f.headers...)
}

// wsSession is one accepted connection.
type wsSession struct {
	t     *testing.T
	f     *fakeUpstream
	ws    *websocket.Conn
	index int // 1-based: which connection this is
}

// recv reads one message and records it. ok=false means the peer is gone, which is the
// ordinary end of every connection and not a failure.
func (s *wsSession) recv() (string, bool) {
	_, b, err := s.ws.Read(s.f.ctx)
	if err != nil {
		return "", false
	}

	msg := string(b)

	s.f.mu.Lock()
	s.f.msgs = append(s.f.msgs, msg)
	s.f.mu.Unlock()

	return msg, true
}

func (s *wsSession) send(msg string) {
	if err := s.ws.Write(s.f.ctx, websocket.MessageText, []byte(msg)); err != nil {
		s.t.Errorf("fake upstream: write %q: %v", msg, err)
	}
}

// handshake consumes the client's three opening messages. It asserts nothing -- the
// framing assertions are made on f.received() by the test that cares -- but it does record
// them, so every test's message log begins with the handshake.
func (s *wsSession) handshake() bool {
	for range 3 {
		if _, ok := s.recv(); !ok {
			s.t.Errorf("fake upstream: connection closed during the handshake")

			return false
		}
	}

	return true
}

// scriptedServe answers the i-th request with replies[i], then drains until the client
// hangs up. Every hashserv RPC -- including the exists-stream mode switch, whose enter,
// query and exit are three requests and three replies -- is strictly one response per
// request, so a positional script is a faithful server.
func scriptedServe(replies ...string) serveFunc {
	return func(s *wsSession) {
		if !s.handshake() {
			return
		}

		for _, reply := range replies {
			if _, ok := s.recv(); !ok {
				return
			}

			s.send(reply)
		}

		// Keep reading: an unexpected extra request must show up in f.received() rather
		// than race with the close.
		for {
			if _, ok := s.recv(); !ok {
				return
			}
		}
	}
}

// echoServe answers every `get` with a unihash DERIVED from the taskhash it was asked
// about. That is what turns the concurrency test into a desynchronization detector: if two
// callers ever share one connection, one of them reads the other's reply, and the derived
// unihash will not match the taskhash it asked for.
func echoServe(s *wsSession) {
	if !s.handshake() {
		return
	}

	for {
		msg, ok := s.recv()
		if !ok {
			return
		}

		var req struct {
			Get struct {
				Method   string `json:"method"`
				Taskhash string `json:"taskhash"`
			} `json:"get"`
		}

		if err := json.Unmarshal([]byte(msg), &req); err != nil {
			s.t.Errorf("fake upstream: bad request %q: %v", msg, err)

			return
		}

		s.send(fmt.Sprintf(`{"method":%q,"taskhash":%q,"unihash":%q}`,
			req.Get.Method, req.Get.Taskhash, derivedUnihash(req.Get.Taskhash)))
	}
}

// derivedUnihash maps a taskhash onto a deterministic, WELL-FORMED unihash (64 lowercase
// hex), so a test can assert the answer it got is the answer to the question it asked.
func derivedUnihash(taskhash string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(taskhash)))
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRecorder(t *testing.T) (*metrics.Metrics, *metrics.HashservRecorder) {
	t.Helper()

	m := metrics.New()

	return m, m.Hashserv("acme", "widget")
}

// upstreamTotal reads one bakery_hashserv_upstream_total series out of the registry.
func upstreamTotal(t *testing.T, m *metrics.Metrics, op metrics.HashservUpstreamOp, res metrics.HashservUpstreamResult) float64 {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, f := range families {
		if f.GetName() != "bakery_hashserv_upstream_total" {
			continue
		}

		for _, metric := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}

			if labels["op"] == string(op) && labels["result"] == string(res) {
				return metric.GetCounter().GetValue()
			}
		}
	}

	return 0
}

func newTestUpstream(t *testing.T, f *fakeUpstream, cfg UpstreamConfig) (*Upstream, *metrics.Metrics) {
	t.Helper()

	m, rec := testRecorder(t)

	if cfg.URL == "" {
		cfg.URL = f.url()
	}

	up, err := NewUpstream(cfg, rec, testLogger())
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}

	t.Cleanup(func() { _ = up.Close() })

	return up, m
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// ---------------------------------------------------------------------------
// The handshake. This is the test the file exists for.
// ---------------------------------------------------------------------------

// TestUpstreamHandshakeIsThreeMessagesWithNoNewlines pins the trap.
//
// Over WebSocket the OEHASHEQUIV handshake is ONE MESSAGE PER LINE. A client that sends
// "OEHASHEQUIV 1.1\nneeds-headers: false\n\n" as a single message is not rejected -- the
// reference server parses the greeting out of it and then BLOCKS FOREVER waiting for
// headers it believes it has not received. So the assertion is on the exact message
// boundaries, and on the absence of a newline anywhere.
func TestUpstreamHandshakeIsThreeMessagesWithNoNewlines(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, scriptedServe(`{"alive":true}`))

	c, err := DialUpstream(ctx, UpstreamConfig{URL: f.url()})
	if err != nil {
		t.Fatalf("DialUpstream: %v", err)
	}

	defer func() { _ = c.Close() }()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	want := []string{
		"OEHASHEQUIV 1.1",
		"needs-headers: false",
		"", // THE EMPTY MESSAGE. Not an empty line -- there are no lines.
		`{"ping":{}}`,
	}

	got := f.received()

	if len(got) != len(want) {
		t.Fatalf("upstream received %d messages, want %d: %q", len(got), len(want), got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i], want[i])
		}

		if strings.Contains(got[i], "\n") {
			t.Errorf("message %d contains a newline: %q -- the WS transport has none", i, got[i])
		}
	}
}

// TestUpstreamSendsNoAuthorizationHeaderOnTheUpgrade. Credentials travel IN-BAND, via the
// `auth` RPC. Userinfo left in the URL would make net/http set a Basic Authorization header
// on the upgrade all by itself -- which is not what a hashserv client does, and would put
// the token on a header of a request we did not intend to authenticate.
func TestUpstreamSendsNoAuthorizationHeaderOnTheUpgrade(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, scriptedServe(`{"result":true}`))

	url := strings.Replace(f.url(), "ws://", "ws://someone:bkry_secret@", 1)

	c, err := DialUpstream(ctx, UpstreamConfig{URL: url})
	if err != nil {
		t.Fatalf("DialUpstream: %v", err)
	}

	defer func() { _ = c.Close() }()

	for _, h := range f.upgradeHeaders() {
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("upgrade carried Authorization: %q -- hashserv auth is in-band", got)
		}
	}

	// The credentials in the URL still authenticate: they drive the in-band `auth` RPC.
	got := f.received()
	if len(got) != 4 {
		t.Fatalf("upstream received %d messages, want 4 (handshake + auth): %q", len(got), got)
	}

	if want := `{"auth":{"username":"someone","token":"bkry_secret"}}`; got[3] != want {
		t.Errorf("auth request = %q, want %q", got[3], want)
	}
}

// ---------------------------------------------------------------------------
// The RPCs.
// ---------------------------------------------------------------------------

func TestUpstreamClientRPCs(t *testing.T) {
	t.Parallel()

	const (
		method   = "oe.sstatesig.OEOuthashBasic"
		taskhash = "1111111111111111111111111111111111111111111111111111111111111111"
		outhash  = "2222222222222222222222222222222222222222222222222222222222222222"
		unihash  = "3333333333333333333333333333333333333333333333333333333333333333"
	)

	tests := []struct {
		name     string
		replies  []string
		call     func(t *testing.T, ctx context.Context, c *UpstreamClient)
		wantReqs []string
	}{
		{
			name:    "ping",
			replies: []string{`{"alive":true}`},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				if err := c.Ping(ctx); err != nil {
					t.Errorf("Ping: %v", err)
				}
			},
			wantReqs: []string{`{"ping":{}}`},
		},
		{
			name:    "auth",
			replies: []string{`{"result":true,"username":"u","permissions":["@read"]}`},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				if err := c.Auth(ctx, "u", "bkry_tok"); err != nil {
					t.Errorf("Auth: %v", err)
				}
			},
			wantReqs: []string{`{"auth":{"username":"u","token":"bkry_tok"}}`},
		},
		{
			name:    "get: a hit",
			replies: []string{fmt.Sprintf(`{"method":%q,"taskhash":%q,"unihash":%q}`, method, taskhash, unihash)},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				got, ok, err := c.GetUnihash(ctx, method, taskhash)
				if err != nil || !ok || got != unihash {
					t.Errorf("GetUnihash = (%q, %v, %v), want (%q, true, nil)", got, ok, err, unihash)
				}
			},
			wantReqs: []string{fmt.Sprintf(`{"get":{"method":%q,"taskhash":%q,"all":false}}`, method, taskhash)},
		},
		{
			// `null` is an ANSWER, not a failure: upstream does not know this taskhash.
			name:    "get: a miss is null, not an error",
			replies: []string{`null`},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				got, ok, err := c.GetUnihash(ctx, method, taskhash)
				if err != nil {
					t.Errorf("GetUnihash: %v", err)
				}

				if ok || got != "" {
					t.Errorf("GetUnihash = (%q, %v), want (\"\", false)", got, ok)
				}
			},
			wantReqs: []string{fmt.Sprintf(`{"get":{"method":%q,"taskhash":%q,"all":false}}`, method, taskhash)},
		},
		{
			name: "get-outhash: the joined row",
			replies: []string{fmt.Sprintf(
				`{"method":%q,"taskhash":%q,"outhash":%q,"created":"2026-07-13T00:00:00","unihash":%q}`,
				method, taskhash, outhash, unihash)},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				row, ok, err := c.GetOuthash(ctx, method, outhash, taskhash)
				if err != nil || !ok {
					t.Fatalf("GetOuthash = (%v, %v, %v)", row, ok, err)
				}

				if row.Unihash != unihash || row.Outhash != outhash {
					t.Errorf("GetOuthash row = %+v", row)
				}
			},
			wantReqs: []string{fmt.Sprintf(
				`{"get-outhash":{"method":%q,"outhash":%q,"taskhash":%q,"with_unihash":true}}`,
				method, outhash, taskhash)},
		},
		{
			name:    "get-outhash: a miss",
			replies: []string{`null`},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				row, ok, err := c.GetOuthash(ctx, method, outhash, taskhash)
				if err != nil || ok || row != nil {
					t.Errorf("GetOuthash = (%v, %v, %v), want (nil, false, nil)", row, ok, err)
				}
			},
			wantReqs: []string{fmt.Sprintf(
				`{"get-outhash":{"method":%q,"outhash":%q,"taskhash":%q,"with_unihash":true}}`,
				method, outhash, taskhash)},
		},
		{
			// The two encodings of "ok": JSON (quoted) to ENTER stream mode, raw
			// (unquoted) to LEAVE it. Neither side is lenient about the difference.
			name:    "unihash-exists: true, and the connection leaves stream mode",
			replies: []string{`"ok"`, `true`, `ok`},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				got, err := c.UnihashExists(ctx, unihash)
				if err != nil || !got {
					t.Errorf("UnihashExists = (%v, %v), want (true, nil)", got, err)
				}

				if c.broken {
					t.Error("a clean exists-stream round trip left the connection broken")
				}
			},
			wantReqs: []string{`{"exists-stream":null}`, unihash, "END"},
		},
		{
			name:    "unihash-exists: false",
			replies: []string{`"ok"`, `false`, `ok`},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				got, err := c.UnihashExists(ctx, unihash)
				if err != nil || got {
					t.Errorf("UnihashExists = (%v, %v), want (false, nil)", got, err)
				}
			},
			wantReqs: []string{`{"exists-stream":null}`, unihash, "END"},
		},
		{
			// A server that echoes the JSON "ok" on the way OUT has not left stream mode.
			// Reusing that connection would answer the next caller's JSON request with a
			// bare line, so it must be marked broken.
			name:    "unihash-exists: a quoted ok on exit breaks the connection",
			replies: []string{`"ok"`, `true`, `"ok"`},
			call: func(t *testing.T, ctx context.Context, c *UpstreamClient) {
				if _, err := c.UnihashExists(ctx, unihash); err == nil {
					t.Error("UnihashExists accepted a quoted ok on exit from stream mode")
				}

				if !c.broken {
					t.Error("a connection left in stream mode was not marked broken")
				}
			},
			wantReqs: []string{`{"exists-stream":null}`, unihash, "END"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := testContext(t)
			f := newFakeUpstream(t, scriptedServe(tc.replies...))

			c, err := DialUpstream(ctx, UpstreamConfig{URL: f.url()})
			if err != nil {
				t.Fatalf("DialUpstream: %v", err)
			}

			defer func() { _ = c.Close() }()

			tc.call(t, ctx, c)

			got := f.received()
			if len(got) < 3 {
				t.Fatalf("upstream received %d messages, want at least the handshake", len(got))
			}

			gotReqs := got[3:]
			if len(gotReqs) != len(tc.wantReqs) {
				t.Fatalf("requests = %q, want %q", gotReqs, tc.wantReqs)
			}

			for i := range tc.wantReqs {
				if gotReqs[i] != tc.wantReqs[i] {
					t.Errorf("request %d = %q, want %q", i, gotReqs[i], tc.wantReqs[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Failure modes.
// ---------------------------------------------------------------------------

// TestUpstreamInvokeErrorIsDistinctAndIsNotRetried.
//
// An invoke-error is not a network blip: upstream sends it and CLOSES the connection, and
// the condition that produced it -- a rejected credential, a denied permission -- is
// deterministic. Retrying it re-dials, re-authenticates, fails identically, and turns one
// clean error into a reconnect loop aimed at a third party. So: it must surface as its own
// type, it must dial exactly once, and the connection it arrived on must never go back in
// the pool.
func TestUpstreamInvokeErrorIsDistinctAndIsNotRetried(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)

	f := newFakeUpstream(t, func(s *wsSession) {
		if !s.handshake() {
			return
		}

		if _, ok := s.recv(); !ok {
			return
		}

		s.send(`{"invoke-error":{"message":"Not authenticated"}}`)
		// ... and upstream closes. That is the protocol, not an accident.
	})

	up, m := newTestUpstream(t, f, UpstreamConfig{})

	_, _, err := up.GetUnihash(ctx, "m", "t")
	if err == nil {
		t.Fatal("GetUnihash succeeded against an invoke-error")
	}

	var invErr UpstreamInvokeError
	if !errors.As(err, &invErr) {
		t.Fatalf("error = %v (%T), want an UpstreamInvokeError", err, err)
	}

	if invErr.Message != "Not authenticated" {
		t.Errorf("invoke-error message = %q, want %q", invErr.Message, "Not authenticated")
	}

	if got := f.connCount(); got != 1 {
		t.Errorf("upstream was dialed %d times; an invoke-error must not be retried", got)
	}

	up.mu.Lock()
	idle := len(up.idle)
	up.mu.Unlock()

	if idle != 0 {
		t.Errorf("idle connections = %d; a connection that errored must be discarded", idle)
	}

	if got := upstreamTotal(t, m, metrics.UpstreamGet, metrics.UpstreamError); got != 1 {
		t.Errorf("bakery_hashserv_upstream_total{op=get,result=error} = %v, want 1", got)
	}
}

// TestUpstreamServerClosesMidRequest: a connection that dies mid-flight is an error, is NOT
// an invoke-error, and is discarded rather than pooled -- its stream position is unknown.
func TestUpstreamServerClosesMidRequest(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)

	f := newFakeUpstream(t, func(s *wsSession) {
		if !s.handshake() {
			return
		}

		_, _ = s.recv() // read the request, answer nothing, hang up
	})

	up, _ := newTestUpstream(t, f, UpstreamConfig{})

	_, _, err := up.GetUnihash(ctx, "m", "t")
	if err == nil {
		t.Fatal("GetUnihash succeeded against a server that hung up")
	}

	var invErr UpstreamInvokeError
	if errors.As(err, &invErr) {
		t.Errorf("a dead connection surfaced as an invoke-error: %v", err)
	}

	up.mu.Lock()
	idle := len(up.idle)
	up.mu.Unlock()

	if idle != 0 {
		t.Errorf("idle connections = %d; a broken connection must be discarded", idle)
	}
}

// TestUpstreamHealthyConnectionIsReused is the positive control for the two tests above: a
// connection that did NOT error goes back in the pool and is used again, so "discarded" is
// a real distinction and not the only thing this pool can do.
func TestUpstreamHealthyConnectionIsReused(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, echoServe)
	up, _ := newTestUpstream(t, f, UpstreamConfig{})

	for i := range 3 {
		taskhash := fmt.Sprintf("%064x", i)

		got, ok, err := up.GetUnihash(ctx, "m", taskhash)
		if err != nil || !ok {
			t.Fatalf("GetUnihash: (%q, %v, %v)", got, ok, err)
		}

		if want := derivedUnihash(taskhash); got != want {
			t.Fatalf("GetUnihash = %q, want %q", got, want)
		}
	}

	if got := f.connCount(); got != 1 {
		t.Errorf("upstream was dialed %d times for 3 sequential lookups, want 1", got)
	}
}

// TestUpstreamStalePooledConnectionIsRetriedOnce is the classic pooled-connection race: the
// peer reaped an idle connection and we had no way to know. One retry, on a FRESH
// connection, and it is only safe because EVERY RPC Bakery issues upstream is a read.
func TestUpstreamStalePooledConnectionIsRetriedOnce(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)

	f := newFakeUpstream(t, func(s *wsSession) {
		if !s.handshake() {
			return
		}

		for {
			msg, ok := s.recv()
			if !ok {
				return
			}

			var req struct {
				Get struct {
					Taskhash string `json:"taskhash"`
				} `json:"get"`
			}

			if err := json.Unmarshal([]byte(msg), &req); err != nil {
				s.t.Errorf("fake upstream: bad request %q: %v", msg, err)

				return
			}

			s.send(fmt.Sprintf(`{"method":"m","taskhash":%q,"unihash":%q}`,
				req.Get.Taskhash, derivedUnihash(req.Get.Taskhash)))

			// The FIRST connection answers one request and then hangs up, while our pool
			// still believes it is idle and healthy.
			if s.index == 1 {
				return
			}
		}
	})

	up, _ := newTestUpstream(t, f, UpstreamConfig{})

	if _, ok, err := up.GetUnihash(ctx, "m", strings.Repeat("a", 64)); err != nil || !ok {
		t.Fatalf("first GetUnihash: (%v, %v)", ok, err)
	}

	// The pooled connection is now dead. This must still answer.
	taskhash := strings.Repeat("b", 64)

	got, ok, err := up.GetUnihash(ctx, "m", taskhash)
	if err != nil || !ok {
		t.Fatalf("GetUnihash over a stale pooled connection: (%q, %v, %v)", got, ok, err)
	}

	if want := derivedUnihash(taskhash); got != want {
		t.Errorf("GetUnihash = %q, want %q", got, want)
	}

	if n := f.connCount(); n != 2 {
		t.Errorf("upstream was dialed %d times, want 2 (the stale one plus its replacement)", n)
	}
}

// TestUpstreamRejectsAMalformedUnihash. A unihash from a third party is written through
// into our database and becomes an sstate object filename. "We could not read the answer"
// must never be laundered into "here is the answer", so a malformed one is an ERROR, and
// it is counted as an upstream error rather than a hit.
func TestUpstreamRejectsAMalformedUnihash(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, scriptedServe(`{"method":"m","taskhash":"t","unihash":"HELLO"}`))
	up, m := newTestUpstream(t, f, UpstreamConfig{})

	got, ok, err := up.GetUnihash(ctx, "m", "t")
	if err == nil {
		t.Fatalf("GetUnihash accepted %q as a unihash", got)
	}

	if !errors.Is(err, errUpstreamBadUnihash) {
		t.Errorf("error = %v, want errUpstreamBadUnihash", err)
	}

	if ok {
		t.Error("a malformed unihash was reported as a hit")
	}

	if n := upstreamTotal(t, m, metrics.UpstreamGet, metrics.UpstreamError); n != 1 {
		t.Errorf("bakery_hashserv_upstream_total{op=get,result=error} = %v, want 1", n)
	}
}

// TestUpstreamMissIsNotAnError pins the metric: `miss` is an answer.
func TestUpstreamMissIsNotAnError(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, scriptedServe(`null`))
	up, m := newTestUpstream(t, f, UpstreamConfig{})

	if _, ok, err := up.GetUnihash(ctx, "m", "t"); err != nil || ok {
		t.Fatalf("GetUnihash = (%v, %v), want (false, nil)", ok, err)
	}

	if n := upstreamTotal(t, m, metrics.UpstreamGet, metrics.UpstreamMiss); n != 1 {
		t.Errorf("bakery_hashserv_upstream_total{op=get,result=miss} = %v, want 1", n)
	}

	if n := upstreamTotal(t, m, metrics.UpstreamGet, metrics.UpstreamError); n != 0 {
		t.Errorf("a miss was counted as an error: %v", n)
	}
}

// ---------------------------------------------------------------------------
// The pool.
// ---------------------------------------------------------------------------

// TestUpstreamPoolIsExclusiveUnderConcurrency.
//
// A pooled connection is a STATEFUL, STRICTLY-ORDERED stream with NO REQUEST IDS. Two
// callers interleaving requests on one connection is not a data race that -race would
// necessarily catch -- it is a protocol desynchronization, where caller A reads caller B's
// reply and gets a WRONG UNIHASH, which is a wrong sstate object. So the fake answers each
// get with a unihash derived from the taskhash it was asked about, and every caller checks
// the answer against its own question.
func TestUpstreamPoolIsExclusiveUnderConcurrency(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, echoServe)

	const (
		maxConns  = 2
		callers   = 8
		perCaller = 20
	)

	up, _ := newTestUpstream(t, f, UpstreamConfig{MaxConns: maxConns})

	var wg sync.WaitGroup

	wg.Add(callers)

	for c := range callers {
		go func() {
			defer wg.Done()

			for i := range perCaller {
				taskhash := fmt.Sprintf("%064x", c*perCaller+i)

				got, ok, err := up.GetUnihash(ctx, "m", taskhash)
				if err != nil || !ok {
					t.Errorf("GetUnihash(%s): (%v, %v)", taskhash[:8], ok, err)

					return
				}

				if want := derivedUnihash(taskhash); got != want {
					t.Errorf("GetUnihash(%s) = %s, want %s -- a reply crossed connections",
						taskhash[:8], got[:8], want[:8])

					return
				}
			}
		}()
	}

	wg.Wait()

	if n := f.connCount(); n > maxConns {
		t.Errorf("pool opened %d connections, want at most %d", n, maxConns)
	}
}

// TestUpstreamPoolHonorsContextWhenExhausted. Blocking on a permit is fine; blocking past
// the caller's deadline is not.
func TestUpstreamPoolHonorsContextWhenExhausted(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	f := newFakeUpstream(t, func(s *wsSession) {
		if !s.handshake() {
			return
		}

		if _, ok := s.recv(); !ok {
			return
		}

		select {
		case <-release:
		case <-s.f.ctx.Done():
			return
		}

		s.send(`null`)
	})

	up, _ := newTestUpstream(t, f, UpstreamConfig{MaxConns: 1, OpTimeout: 5 * time.Second})

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		// Parks with the pool's only permit until release is closed.
		_, _, _ = up.GetUnihash(context.Background(), "m", "held")
	}()

	defer func() {
		close(release)
		wg.Wait()
	}()

	// Give the holder the permit before we contend for it. Without this the test could
	// pass for the wrong reason -- by winning the permit rather than by timing out.
	waitFor(t, func() bool { return f.connCount() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _, err := up.GetUnihash(ctx, "m", "waiting")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetUnihash on an exhausted pool = %v, want a DeadlineExceeded", err)
	}
}

// TestUpstreamDialsLazily. A dead upstream must not stall boot: constructing the pool must
// not touch the network.
func TestUpstreamDialsLazily(t *testing.T) {
	t.Parallel()

	_, rec := testRecorder(t)

	// Nothing is listening here, and nothing may try.
	up, err := NewUpstream(UpstreamConfig{URL: "ws://127.0.0.1:1/cache/acme/widget/hashserv"}, rec, testLogger())
	if err != nil {
		t.Fatalf("NewUpstream against a dead upstream: %v", err)
	}

	defer func() { _ = up.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := up.Ping(ctx); err == nil {
		t.Error("Ping succeeded against a dead upstream")
	}
}

func TestUpstreamConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "ws", url: "ws://host/cache/acme/widget/hashserv"},
		{name: "wss", url: "wss://hashserv.yoctoproject.org/ws"},
		{name: "empty", url: "", wantErr: true},
		{name: "http is not a websocket scheme", url: "http://host/ws", wantErr: true},
		{name: "unix is not served", url: "unix:///run/hashserv.sock", wantErr: true},
	}

	_, rec := testRecorder(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			up, err := NewUpstream(UpstreamConfig{URL: tc.url}, rec, testLogger())
			if (err != nil) != tc.wantErr {
				t.Fatalf("NewUpstream(%q) error = %v, wantErr %v", tc.url, err, tc.wantErr)
			}

			if err == nil {
				_ = up.Close()
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The backfill worker.
// ---------------------------------------------------------------------------

// fakeBackfillStore is the hand-written fake for the one write the worker performs.
type fakeBackfillStore struct {
	mu      sync.Mutex
	inserts []insertedUnihash

	// block, when non-nil, parks every insert until it is closed. It is what makes the
	// queue-depth assertions deterministic instead of a race against the worker.
	block chan struct{}

	// entered signals that an insert has begun. Waiting on it proves the worker is
	// in-flight -- and that pending counts the in-flight item, not just the queued ones.
	entered chan struct{}

	err error
}

type insertedUnihash struct {
	backendID int64
	method    string
	taskhash  string
	unihash   string
}

func (s *fakeBackfillStore) InsertUnihash(ctx context.Context, backendID int64, method, taskhash, unihash string) error {
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}

	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err != nil {
		return s.err
	}

	s.inserts = append(s.inserts, insertedUnihash{
		backendID: backendID,
		method:    method,
		taskhash:  taskhash,
		unihash:   unihash,
	})

	return nil
}

func (s *fakeBackfillStore) all() []insertedUnihash {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]insertedUnihash(nil), s.inserts...)
}

func newTestBackfiller(t *testing.T, up *Upstream, store backfillStore, cfg BackfillConfig) *Backfiller {
	t.Helper()

	_, rec := testRecorder(t)

	b, err := NewBackfiller(up, store, rec, testLogger(), cfg)
	if err != nil {
		t.Fatalf("NewBackfiller: %v", err)
	}

	t.Cleanup(func() { _ = b.Close() })

	return b
}

// waitFor polls until cond holds. Used only to establish a PRECONDITION -- never to decide
// an assertion, which is what would make it a sleep in disguise.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatal("timed out waiting for a precondition")
}

// TestBackfillWaitIsExactAndBlocksUntilDrained.
//
// `backfill-wait` is what makes the conformance run deterministic, so both halves must be
// exact: the depth it returns is the depth AT ENTRY, and it does not return until the queue
// has drained -- INCLUDING the item currently being written. A Wait that counted only the
// channel could return while the last write was still in the air, which is precisely the
// race it exists to close.
func TestBackfillWaitIsExactAndBlocksUntilDrained(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, echoServe)
	up, _ := newTestUpstream(t, f, UpstreamConfig{})

	store := &fakeBackfillStore{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	b := newTestBackfiller(t, up, store, BackfillConfig{BackendID: 42, QueueSize: 8})

	const items = 5

	taskhashes := make([]string, items)

	for i := range items {
		taskhashes[i] = fmt.Sprintf("%064x", i)

		if !b.Enqueue("m", taskhashes[i]) {
			t.Fatalf("Enqueue(%d) was dropped by a queue with room", i)
		}
	}

	// The worker is parked inside the insert, so one item is in flight and four are
	// queued. All five are still PENDING.
	<-store.entered

	waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	depth, err := b.Wait(waitCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait returned %v while the queue was still full", err)
	}

	if depth != items {
		t.Errorf("Wait depth = %d, want %d -- the in-flight item must count", depth, items)
	}

	close(store.block)

	depth, err = b.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if depth < 0 || depth > items {
		t.Errorf("Wait depth = %d, want 0..%d", depth, items)
	}

	got := store.all()
	if len(got) != items {
		t.Fatalf("inserted %d unihashes, want %d: %+v", len(got), items, got)
	}

	for _, ins := range got {
		if ins.backendID != 42 {
			t.Errorf("insert %+v: backend_id = %d, want 42", ins, ins.backendID)
		}

		if want := derivedUnihash(ins.taskhash); ins.unihash != want {
			t.Errorf("insert %+v: unihash = %q, want %q", ins, ins.unihash, want)
		}
	}

	// Drained: the depth at entry is now zero and Wait does not block.
	if depth, err := b.Wait(ctx); err != nil || depth != 0 {
		t.Errorf("Wait on a drained queue = (%d, %v), want (0, nil)", depth, err)
	}
}

// TestBackfillDropsWhenTheQueueIsFull.
//
// The hot path is a BB_NUMBER_THREADS-parallel burst of tens of thousands of get-stream
// lookups. Enqueue MUST NOT BLOCK it: a dropped backfill is a missed optimization -- the
// next build re-asks upstream and gets the same answer -- while a blocked one is a stalled
// build. This test would hang, not fail, if Enqueue ever blocked.
func TestBackfillDropsWhenTheQueueIsFull(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, echoServe)
	up, _ := newTestUpstream(t, f, UpstreamConfig{})

	store := &fakeBackfillStore{block: make(chan struct{}), entered: make(chan struct{}, 1)}

	const queueSize = 2

	b := newTestBackfiller(t, up, store, BackfillConfig{QueueSize: queueSize, Workers: 1})

	// Park the worker inside the insert FIRST, so the queue is empty and its capacity is
	// the only thing left in play. Without this the count is a race with the drain.
	if !b.Enqueue("m", fmt.Sprintf("%064x", 0)) {
		t.Fatal("the first Enqueue was dropped by an empty queue")
	}

	<-store.entered

	for i := range queueSize {
		if !b.Enqueue("m", fmt.Sprintf("%064x", i+1)) {
			t.Fatalf("Enqueue(%d) was dropped by a queue with room", i+1)
		}
	}

	const overflow = 3

	for i := range overflow {
		if b.Enqueue("m", fmt.Sprintf("%064x", 100+i)) {
			t.Errorf("Enqueue(%d) was accepted by a full queue", 100+i)
		}
	}

	if got := b.Dropped(); got != overflow {
		t.Errorf("Dropped() = %d, want %d", got, overflow)
	}

	close(store.block)

	if _, err := b.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := len(store.all()); got != queueSize+1 {
		t.Errorf("inserted %d unihashes, want %d (the dropped ones must not appear)", got, queueSize+1)
	}
}

// TestBackfillRefusesAMalformedUnihash. Never persist a malformed unihash from a third
// party: it becomes an sstate object filename and the GC root that keeps it alive.
func TestBackfillRefusesAMalformedUnihash(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)

	// A well-formed hashserv response carrying a unihash that is not a unihash.
	f := newFakeUpstream(t, scriptedServe(`{"method":"m","taskhash":"t","unihash":"../../etc/passwd"}`))
	up, _ := newTestUpstream(t, f, UpstreamConfig{})

	store := &fakeBackfillStore{}
	b := newTestBackfiller(t, up, store, BackfillConfig{BackendID: 1})

	if !b.Enqueue("m", "t") {
		t.Fatal("Enqueue was dropped")
	}

	if _, err := b.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := store.all(); len(got) != 0 {
		t.Errorf("a malformed unihash was persisted: %+v", got)
	}
}

// TestBackfillRetiresEveryItemWhateverHappens. Every exit path -- upstream miss, upstream
// error, insert error -- must retire its item. One that does not leaves `backfill-wait`
// blocked forever, and on the conformance run that is a hang, not a failure.
func TestBackfillRetiresEveryItemWhateverHappens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		reply string
		store *fakeBackfillStore
	}{
		{
			name:  "upstream misses",
			reply: `null`,
			store: &fakeBackfillStore{},
		},
		{
			name:  "upstream errors",
			reply: `{"invoke-error":{"message":"nope"}}`,
			store: &fakeBackfillStore{},
		},
		{
			name:  "the insert fails",
			reply: fmt.Sprintf(`{"method":"m","taskhash":"t","unihash":%q}`, strings.Repeat("a", 64)),
			store: &fakeBackfillStore{err: errors.New("db is down")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := testContext(t)
			f := newFakeUpstream(t, scriptedServe(tc.reply))
			up, _ := newTestUpstream(t, f, UpstreamConfig{})
			b := newTestBackfiller(t, up, tc.store, BackfillConfig{BackendID: 1})

			if !b.Enqueue("m", "t") {
				t.Fatal("Enqueue was dropped")
			}

			depth, err := b.Wait(ctx)
			if err != nil {
				t.Fatalf("Wait: %v -- a failed backfill must still retire its item", err)
			}

			if depth != 1 {
				t.Errorf("Wait depth = %d, want 1", depth)
			}

			if got, err := b.Wait(ctx); err != nil || got != 0 {
				t.Errorf("Wait on a drained queue = (%d, %v), want (0, nil)", got, err)
			}
		})
	}
}

// TestBackfillIsCountedAsBackfillNotAsGet. The op label distinguishes the write-behind path
// from the synchronous one; folding them together would hide a backfill storm inside the
// `get` series.
func TestBackfillIsCountedAsBackfill(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, echoServe)

	m := metrics.New()
	rec := m.Hashserv("acme", "widget")

	up, err := NewUpstream(UpstreamConfig{URL: f.url()}, rec, testLogger())
	if err != nil {
		t.Fatalf("NewUpstream: %v", err)
	}

	defer func() { _ = up.Close() }()

	store := &fakeBackfillStore{}

	b, err := NewBackfiller(up, store, rec, testLogger(), BackfillConfig{BackendID: 7})
	if err != nil {
		t.Fatalf("NewBackfiller: %v", err)
	}

	defer func() { _ = b.Close() }()

	if !b.Enqueue("m", strings.Repeat("c", 64)) {
		t.Fatal("Enqueue was dropped")
	}

	if _, err := b.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if n := upstreamTotal(t, m, metrics.UpstreamBackfill, metrics.UpstreamHit); n != 1 {
		t.Errorf("bakery_hashserv_upstream_total{op=backfill,result=hit} = %v, want 1", n)
	}

	if n := upstreamTotal(t, m, metrics.UpstreamGet, metrics.UpstreamHit); n != 0 {
		t.Errorf("a backfill was counted as a get: %v", n)
	}
}

// TestBackfillCloseReleasesWait. Shutdown abandons the queue -- a backfill is an
// optimization and holding shutdown open for one is not a trade worth making -- but it must
// not leave a caller blocked on a counter that can never reach zero.
func TestBackfillCloseReleasesWait(t *testing.T) {
	t.Parallel()

	ctx := testContext(t)
	f := newFakeUpstream(t, echoServe)
	up, _ := newTestUpstream(t, f, UpstreamConfig{})

	store := &fakeBackfillStore{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	b := newTestBackfiller(t, up, store, BackfillConfig{BackendID: 1, QueueSize: 8})

	for i := range 4 {
		if !b.Enqueue("m", fmt.Sprintf("%064x", i)) {
			t.Fatalf("Enqueue(%d) was dropped", i)
		}
	}

	<-store.entered

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	depth, err := b.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait after Close: %v -- it must not block on a counter that cannot drain", err)
	}

	if depth != 0 {
		t.Errorf("Wait depth after Close = %d, want 0", depth)
	}

	if b.Enqueue("m", "late") {
		t.Error("Enqueue was accepted after Close")
	}

	close(store.block)
}
