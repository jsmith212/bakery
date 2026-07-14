package hashserv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jsmith212/bakery/internal/metrics"
)

// This file is the MIRROR IMAGE of framing.go: there, Bakery reads a handshake and writes
// responses; here it writes a handshake and reads them. Every trap in the package doc
// applies in reverse, and one of them is fatal in a way that is hard to see:
//
//	over WebSocket the handshake is ONE MESSAGE PER LINE, WITH NO NEWLINES.
//
// A client that sends "OEHASHEQUIV 1.1\nneeds-headers: false\n\n" as one message is not
// refused -- upstream's Cut on " " succeeds, the version parses, and then it BLOCKS
// FOREVER waiting for the header lines it already "received". No error, no timeout on the
// reference server, and the miss that sent us upstream never returns. See clientHandshake.
//
// The other half of this file is the pool. The reference implementation opens ONE UPSTREAM
// CONNECTION PER DOWNSTREAM CLIENT (server.py ServerClient.process_requests), which under a
// BB_NUMBER_THREADS-wide build is a connection storm aimed at a third party we do not own.
// Bakery keeps a small bounded pool instead.

// Upstream defaults. All overridable through UpstreamConfig.
const (
	// defaultUpstreamMaxConns bounds the pool. Upstream queries are COLD-PATH ONLY --
	// they happen on a local miss -- so a handful of connections absorbs a build, and
	// the bound is what stops a first build against an empty cache from opening one
	// socket per BB_NUMBER_THREADS worker on hashserv.yoctoproject.org.
	defaultUpstreamMaxConns = 4

	// defaultUpstreamDialTimeout bounds ONE dial, not the whole operation.
	defaultUpstreamDialTimeout = 10 * time.Second

	// defaultUpstreamOpTimeout bounds one pooled operation end to end, and it is not
	// optional: the caller's context on the hot path is the DOWNSTREAM CONNECTION's, and
	// that lives for the entire build. Without a timeout of its own, one hung upstream
	// query holds a pool permit for hours and starves every other caller behind it.
	defaultUpstreamOpTimeout = 15 * time.Second
)

// ErrUpstreamClosed is returned once the pool has been closed. It is not retryable.
var ErrUpstreamClosed = errors.New("hashserv: upstream pool is closed")

// errUpstreamBadUnihash is upstream answering with something that is not a unihash.
//
// It is an ERROR, never a value: a unihash from upstream is written through into our
// database and handed to a build, where it becomes an sstate object filename. A third
// party is not trusted to produce one, and "we could not read the answer" must never be
// laundered into "here is the answer".
var errUpstreamBadUnihash = errors.New("hashserv: upstream returned a malformed unihash")

// UpstreamInvokeError is upstream's {"invoke-error": {"message": ...}} response.
//
// It is a DISTINCT type because it must NOT be retried, and everything about it is
// different from a network blip:
//
//   - The server has ALREADY CLOSED the connection it arrived on (serv.py breaks the loop
//     right after sending it). There is nothing left to retry on.
//   - The condition is DETERMINISTIC -- a rejected credential, a malformed request, a
//     permission denial. Re-dialing, re-authenticating and re-asking produces the same
//     answer, so a retry converts one clean error into a reconnect loop pointed at a third
//     party.
//
// Python's own client draws the same line: InvokeError is in no retry tuple, while
// OSError/ConnectionError/ConnectionClosedError are (client.py:161-178).
type UpstreamInvokeError struct{ Message string }

func (e UpstreamInvokeError) Error() string {
	return "hashserv: upstream invoke-error: " + e.Message
}

// UpstreamConfig configures the chained upstream hashserv. It comes from
// cache_backends.config ({"upstream": "wss://hashserv.yoctoproject.org/ws"}), so URL is
// the only field an operator must set.
type UpstreamConfig struct {
	// URL is a ws:// or wss:// address. Credentials may be embedded as userinfo
	// (wss://user:token@host/ws) -- they are stripped before dialing and never logged.
	URL string

	// Username and Token drive the in-band `auth` RPC on every pooled connection, and
	// override any userinfo in URL. Upstream auth is IN-BAND, never a header: a stock
	// hashserv sends no Authorization on the upgrade (see spec §2.2).
	Username string
	Token    string

	MaxConns    int
	DialTimeout time.Duration
	OpTimeout   time.Duration

	// HTTPClient dials the upgrade. Optional; http.DefaultClient is used when nil.
	HTTPClient *http.Client
}

// normalize validates the config and fills its defaults. It also SPLITS CREDENTIALS OUT OF
// THE URL, so the URL that reaches websocket.Dial and every log line is credential-free.
func (c UpstreamConfig) normalize() (UpstreamConfig, error) {
	if c.URL == "" {
		return c, errors.New("hashserv: upstream URL is required")
	}

	u, err := url.Parse(c.URL)
	if err != nil {
		return c, fmt.Errorf("hashserv: upstream URL: %w", err)
	}

	if u.Scheme != "ws" && u.Scheme != "wss" {
		return c, fmt.Errorf("hashserv: upstream URL scheme %q: want ws or wss", u.Scheme)
	}

	if u.User != nil {
		if c.Username == "" {
			c.Username = u.User.Username()
		}

		if pw, ok := u.User.Password(); ok && c.Token == "" {
			c.Token = pw
		}

		u.User = nil
	}

	c.URL = u.String()

	if c.MaxConns <= 0 {
		c.MaxConns = defaultUpstreamMaxConns
	}

	if c.DialTimeout <= 0 {
		c.DialTimeout = defaultUpstreamDialTimeout
	}

	if c.OpTimeout <= 0 {
		c.OpTimeout = defaultUpstreamOpTimeout
	}

	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}

	return c, nil
}

// ---------------------------------------------------------------------------
// UpstreamClient -- ONE connection.
// ---------------------------------------------------------------------------

// UpstreamClient is a hashserv client on ONE WebSocket connection.
//
// IT IS NOT SAFE FOR CONCURRENT USE, and that is a property of the protocol, not an
// implementation shortcut. There are NO REQUEST IDS: responses come back strictly in
// order, one per request. Two goroutines interleaving requests on one connection is the
// same class of bug as two writer goroutines on a downstream connection -- the responses
// pair up with the wrong requests and the stream is desynchronized silently, forever.
//
// Use *Upstream, which makes that impossible by construction: a connection is checked out
// of the pool, used by exactly one caller, and checked back in.
type UpstreamClient struct {
	ws *websocket.Conn

	// broken marks the connection UNUSABLE. It is set on any transport or protocol
	// failure -- including an invoke-error, after which upstream has already closed the
	// socket -- and a broken connection is destroyed rather than returned to the pool.
	// After a framing failure the stream POSITION is unknown, and a connection whose
	// position is unknown answers the next caller's question with the previous caller's
	// answer.
	broken bool
}

// DialUpstream opens and hands back one authenticated connection.
//
// It performs the WebSocket upgrade, sets the read limit, sends the handshake and -- when
// the config carries credentials -- the in-band `auth` RPC. A connection this returns is
// ready to serve RPCs.
func DialUpstream(ctx context.Context, cfg UpstreamConfig) (*UpstreamClient, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	// The handshake response has no body to close, in either direction: on success
	// coder/websocket takes the body (it IS the connection) and NILS resp.Body -- closing
	// it would panic -- and on failure it has already drained and closed it and left behind
	// a NopCloser over the first 1 KiB, for debugging. "You never need to close resp.Body
	// yourself" (dial.go:110).
	ws, _, err := websocket.Dial(ctx, cfg.URL, //nolint:bodyclose // resp.Body is nil on success and already closed on failure
		&websocket.DialOptions{HTTPClient: cfg.HTTPClient})
	if err != nil {
		return nil, fmt.Errorf("hashserv: dial upstream: %w", err)
	}

	// MANDATORY, in this direction too. coder/websocket defaults to a 32 KiB read limit
	// and a get-outhash response carries outhash_siginfo, which upstream's own suite
	// sends at 128 KiB. WebSocket messages are not chunked, so it arrives as ONE frame
	// and the connection dies with StatusMessageTooBig. See MaxMessageBytes.
	ws.SetReadLimit(MaxMessageBytes)

	c := &UpstreamClient{ws: ws}

	if err := c.clientHandshake(ctx); err != nil {
		c.discard()

		return nil, err
	}

	if cfg.Username != "" || cfg.Token != "" {
		if err := c.Auth(ctx, cfg.Username, cfg.Token); err != nil {
			c.discard()

			return nil, err
		}
	}

	return c, nil
}

// clientHandshake sends the OEHASHEQUIV opening exchange.
//
// THE TRAP. Over WebSocket this is THREE SEPARATE MESSAGES with NO NEWLINES:
//
//	msg 1: "OEHASHEQUIV 1.1"
//	msg 2: "needs-headers: false"
//	msg 3: ""                       <- an EMPTY MESSAGE terminates the headers
//
// It is not a newline-delimited block. The client's send() is transport-polymorphic
// (bb/asyncrpc/client.py setup_connection): StreamConnection.send appends "\n",
// WebsocketConnection.send does not -- it emits the string as one WebSocket message. Send
// the blob and the server blocks forever on the header read, with no error on either side.
// framing.go's handshake() is the server half of exactly this, and its test asserts the
// same three messages.
//
// needs-headers is FALSE, so there is NO REPLY to read. hashserv's handle_headers() returns
// {} and the reply is only the terminating empty message, which upstream sends only when
// asked; reading for one here would consume the response to the first real RPC and
// desynchronize the connection on message four.
func (c *UpstreamClient) clientHandshake(ctx context.Context) error {
	for _, msg := range []string{
		protoName + " " + protoVersion,
		"needs-headers: false",
		"",
	} {
		if err := c.writeMsg(ctx, msg); err != nil {
			return fmt.Errorf("hashserv: upstream handshake: %w", err)
		}
	}

	return nil
}

// Ping checks the connection is alive. Upstream answers {"alive": true}; we care only that
// it answered at all, so the payload is not decoded.
func (c *UpstreamClient) Ping(ctx context.Context) error {
	return c.invoke(ctx, rpcPing, map[string]any{}, nil)
}

// Auth performs the in-band `auth` RPC.
//
// Upstream denies IN-BAND, with an invoke-error and a close -- never a 401 on the upgrade
// (see the package doc). So a rejected credential surfaces here as an UpstreamInvokeError,
// which is exactly what must not be retried.
func (c *UpstreamClient) Auth(ctx context.Context, username, token string) error {
	return c.invoke(ctx, rpcAuth, authRequest{Username: username, Token: token}, nil)
}

// GetUnihash asks upstream for (method, taskhash) -> unihash. ok=false is a MISS: upstream
// answered `null`, which is an answer, not a failure.
//
// This is the plain `get` RPC, deliberately NOT `get-stream`. Our upstream calls are
// one-off misses, not batches, and entering stream mode would put a MODE SWITCH on a shared
// pooled connection -- a connection left in stream mode answers the next checkout's JSON
// request with a raw line.
//
// It returns what upstream said, faithfully, INCLUDING a malformed unihash. The pool
// (*Upstream) is where that is rejected -- see Upstream.GetUnihash.
func (c *UpstreamClient) GetUnihash(ctx context.Context, method, taskhash string) (string, bool, error) {
	var row *unihashResponse

	req := getRequest{Method: method, Taskhash: taskhash, All: false}
	if err := c.invoke(ctx, rpcGet, req, &row); err != nil {
		return "", false, err
	}

	if row == nil {
		return "", false, nil
	}

	return row.Unihash, true, nil
}

// GetOuthash asks upstream for the joined outhash row: every outhash column plus the
// unihash. It is the primitive `report` uses to avoid minting a unihash that diverges from
// an upstream that already knows this output.
//
// with_unihash is true: without it upstream returns the raw outhash row and no unihash,
// which is the one column the caller needs.
func (c *UpstreamClient) GetOuthash(ctx context.Context, method, outhash, taskhash string) (*outhashResponse, bool, error) {
	withUnihash := true

	var row *outhashResponse

	req := getOuthashRequest{
		Method:      method,
		Outhash:     outhash,
		Taskhash:    taskhash,
		WithUnihash: &withUnihash,
	}

	if err := c.invoke(ctx, rpcGetOuthash, req, &row); err != nil {
		return nil, false, err
	}

	if row == nil {
		return nil, false, nil
	}

	return row, true, nil
}

// UnihashExists asks upstream whether a unihash is known.
//
// THERE IS NO NON-STREAMING EXISTS RPC. `exists-stream` is the only one the protocol has
// (docs/design/protocols/yocto.md §2.4 -- the complete method table), so unlike get and
// get-outhash this one MUST enter stream mode, and the whole mode switch is therefore
// contained in a single call:
//
//	-> {"exists-stream": null}
//	<- "ok"        JSON, WITH QUOTES     -- enter stream mode
//	-> <unihash>   a bare line
//	<- true|false  a bare line
//	-> END
//	<- ok          RAW, WITHOUT QUOTES   -- leave stream mode
//
// The two encodings of "ok" are not a typo in the reference implementation and neither side
// is lenient about them (see runStream). The connection is back in normal mode when this
// returns, so it is safe to check back into the pool -- and if ANY step fails it is marked
// broken and destroyed instead, because a connection stuck in stream mode would answer the
// next caller's JSON request with a bare line.
func (c *UpstreamClient) UnihashExists(ctx context.Context, unihash string) (bool, error) {
	if err := c.writeJSON(ctx, map[string]any{rpcExistsStream: nil}); err != nil {
		return false, err
	}

	enter, err := c.readMsg(ctx)
	if err != nil {
		return false, err
	}

	if invErr, ok := decodeUpstreamInvokeError(enter); ok {
		c.broken = true

		return false, invErr
	}

	// Entering is send_message("ok") -- a JSON string. Decode it rather than comparing
	// bytes, which is what the Python client does (invoke() -> JSON -> == "ok").
	var okMsg string
	if err := json.Unmarshal([]byte(enter), &okMsg); err != nil || okMsg != "ok" {
		c.broken = true

		return false, fmt.Errorf("hashserv: upstream refused exists-stream: %q", enter)
	}

	if err := c.writeMsg(ctx, unihash); err != nil {
		return false, err
	}

	answer, err := c.readMsg(ctx)
	if err != nil {
		return false, err
	}

	// Leaving is send("ok") -- RAW, no quotes. A server that sends "ok" (quoted) here has
	// not left stream mode, and the connection must not be reused.
	if err := c.writeMsg(ctx, "END"); err != nil {
		return false, err
	}

	leave, err := c.readMsg(ctx)
	if err != nil {
		return false, err
	}

	if leave != "ok" {
		c.broken = true

		return false, fmt.Errorf("hashserv: upstream did not leave exists-stream: %q", leave)
	}

	return answer == "true", nil
}

// Close shuts the connection down. A broken connection is aborted rather than closed
// politely: after an invoke-error upstream has already gone, and a close handshake with a
// corpse only costs a timeout.
func (c *UpstreamClient) Close() error {
	if c.broken {
		c.discard()

		return nil
	}

	if err := c.ws.Close(websocket.StatusNormalClosure, ""); err != nil {
		return fmt.Errorf("hashserv: close upstream: %w", err)
	}

	return nil
}

// discard aborts the connection without a close handshake.
func (c *UpstreamClient) discard() {
	c.broken = true

	_ = c.ws.CloseNow()
}

// invoke sends ONE request and reads ONE response.
//
// A request is a single-key JSON object, {"<method>": <payload>}; a response is a BARE JSON
// value -- an object, or null. There is no id, no envelope, and no way to tell two responses
// apart, which is the whole reason a connection may serve exactly one caller at a time.
//
// out may be nil when the response is not needed; the response is still read, because
// leaving it in the socket would hand it to the next request as its own.
func (c *UpstreamClient) invoke(ctx context.Context, name string, req any, out any) error {
	if err := c.writeJSON(ctx, map[string]any{name: req}); err != nil {
		return err
	}

	resp, err := c.readMsg(ctx)
	if err != nil {
		return err
	}

	if invErr, ok := decodeUpstreamInvokeError(resp); ok {
		// Upstream has already closed the socket behind this message.
		c.broken = true

		return invErr
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal([]byte(resp), out); err != nil {
		// The bytes were framed correctly but are not what this RPC returns, so our model
		// of the server is wrong and the stream cannot be trusted to be where we think.
		c.broken = true

		return fmt.Errorf("hashserv: upstream %s response: %w", name, err)
	}

	return nil
}

func (c *UpstreamClient) writeJSON(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("hashserv: encode upstream request: %w", err)
	}

	return c.writeMsg(ctx, string(b))
}

// writeMsg sends ONE whole message. No newline is added: on this transport there are none.
func (c *UpstreamClient) writeMsg(ctx context.Context, msg string) error {
	if err := c.ws.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		c.broken = true

		return fmt.Errorf("hashserv: write upstream: %w", err)
	}

	return nil
}

// readMsg reads ONE whole message.
func (c *UpstreamClient) readMsg(ctx context.Context) (string, error) {
	_, b, err := c.ws.Read(ctx)
	if err != nil {
		c.broken = true

		return "", fmt.Errorf("hashserv: read upstream: %w", err)
	}

	return string(b), nil
}

// decodeUpstreamInvokeError recognizes {"invoke-error": {"message": "..."}}.
//
// No normal response can be mistaken for one: get and get-outhash return either null or an
// object of known columns, and neither carries an invoke-error key.
func decodeUpstreamInvokeError(msg string) (UpstreamInvokeError, bool) {
	var raw map[string]json.RawMessage

	if err := json.Unmarshal([]byte(msg), &raw); err != nil {
		return UpstreamInvokeError{}, false
	}

	payload, ok := raw["invoke-error"]
	if !ok {
		return UpstreamInvokeError{}, false
	}

	var body struct {
		Message string `json:"message"`
	}

	if err := json.Unmarshal(payload, &body); err != nil {
		// It IS an invoke-error; we just could not read its message. Surfacing the raw
		// payload beats surfacing an empty string.
		return UpstreamInvokeError{Message: string(payload)}, true
	}

	return UpstreamInvokeError{Message: body.Message}, true
}

// ---------------------------------------------------------------------------
// Upstream -- the bounded connection pool.
// ---------------------------------------------------------------------------

// Upstream is a chained upstream hashserv, fronted by a small bounded connection pool. It
// is safe for concurrent use; UpstreamClient is not, and this is what makes the difference.
//
// EXCLUSIVITY IS STRUCTURAL. A connection is either in the idle list or checked out --
// never both -- so exactly one caller can hold it, and the protocol's strict response
// ordering holds. The permit channel bounds the pool: a caller with no permit waits (or its
// context expires) rather than opening a fifth socket on a server we do not own.
//
// Dialing is LAZY. A dead upstream must not stall boot: nothing connects until the first
// miss reaches it, and a failure there degrades a lookup to a local miss.
type Upstream struct {
	cfg UpstreamConfig
	rec *metrics.HashservRecorder
	log *slog.Logger

	// sem is the permit channel: cap(sem) == MaxConns. Holding a permit for the whole
	// checkout is what bounds the pool, and popping the connection OUT of idle is what
	// makes it exclusive.
	sem chan struct{}

	mu     sync.Mutex
	idle   []*UpstreamClient
	closed bool
}

// NewUpstream builds the pool. It does not dial.
func NewUpstream(cfg UpstreamConfig, rec *metrics.HashservRecorder, log *slog.Logger) (*Upstream, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}

	if rec == nil {
		return nil, errors.New("hashserv: upstream requires a metrics recorder")
	}

	if log == nil {
		log = slog.Default()
	}

	return &Upstream{
		cfg: cfg,
		rec: rec,
		log: log.With("upstream", cfg.URL),
		sem: make(chan struct{}, cfg.MaxConns),
	}, nil
}

// Ping checks upstream is reachable. bitbake's cooker does exactly this at startup before
// it trusts an upstream, and so should any operator-facing health check we grow.
func (u *Upstream) Ping(ctx context.Context) error {
	return u.do(ctx, func(ctx context.Context, c *UpstreamClient) error {
		return c.Ping(ctx)
	})
}

// GetUnihash asks upstream for (method, taskhash) -> unihash, on the `get` miss path.
//
// It VALIDATES the answer. A unihash from upstream is written through into our database and
// handed to a build, where it becomes an sstate object filename -- so a malformed one is an
// upstream ERROR, never a value. That check lives here, on the pooled type every handler
// uses, rather than in each caller.
func (u *Upstream) GetUnihash(ctx context.Context, method, taskhash string) (string, bool, error) {
	return u.getUnihash(ctx, metrics.UpstreamGet, method, taskhash)
}

// getUnihash is GetUnihash with the metrics op parameterized: the backfill worker issues
// the same RPC but must count as `backfill`, not `get`.
func (u *Upstream) getUnihash(
	ctx context.Context,
	op metrics.HashservUpstreamOp,
	method, taskhash string,
) (string, bool, error) {
	var (
		unihash string
		found   bool
	)

	err := u.do(ctx, func(ctx context.Context, c *UpstreamClient) error {
		var err error

		unihash, found, err = c.GetUnihash(ctx, method, taskhash)
		if err != nil {
			return err
		}

		if found && !validUnihash(unihash) {
			// Not a transport failure: the stream is where we think it is, so the
			// connection is NOT marked broken and goes back to the pool.
			return fmt.Errorf("%w: %q", errUpstreamBadUnihash, unihash)
		}

		return nil
	})
	if err != nil {
		u.rec.Upstream(op, metrics.UpstreamError)

		return "", false, err
	}

	if !found {
		u.rec.Upstream(op, metrics.UpstreamMiss)

		return "", false, nil
	}

	u.rec.Upstream(op, metrics.UpstreamHit)

	return unihash, true, nil
}

// GetOuthash asks upstream for the joined outhash row. Its unihash column is validated for
// the same reason GetUnihash's is: it is adopted by `report` and becomes an object name.
func (u *Upstream) GetOuthash(ctx context.Context, method, outhash, taskhash string) (*outhashResponse, bool, error) {
	var (
		row   *outhashResponse
		found bool
	)

	err := u.do(ctx, func(ctx context.Context, c *UpstreamClient) error {
		var err error

		row, found, err = c.GetOuthash(ctx, method, outhash, taskhash)
		if err != nil {
			return err
		}

		if found && row.Unihash != "" && !validUnihash(row.Unihash) {
			return fmt.Errorf("%w: %q", errUpstreamBadUnihash, row.Unihash)
		}

		return nil
	})
	if err != nil {
		u.rec.Upstream(metrics.UpstreamGetOuthash, metrics.UpstreamError)

		return nil, false, err
	}

	if !found {
		u.rec.Upstream(metrics.UpstreamGetOuthash, metrics.UpstreamMiss)

		return nil, false, nil
	}

	u.rec.Upstream(metrics.UpstreamGetOuthash, metrics.UpstreamHit)

	return row, true, nil
}

// UnihashExists asks upstream whether a unihash is known, on the `exists-stream` miss path.
// No backfill: knowing that upstream has a unihash we cannot name teaches us nothing to
// store.
func (u *Upstream) UnihashExists(ctx context.Context, unihash string) (bool, error) {
	var exists bool

	err := u.do(ctx, func(ctx context.Context, c *UpstreamClient) error {
		var err error

		exists, err = c.UnihashExists(ctx, unihash)

		return err
	})
	if err != nil {
		u.rec.Upstream(metrics.UpstreamExists, metrics.UpstreamError)

		return false, err
	}

	if !exists {
		u.rec.Upstream(metrics.UpstreamExists, metrics.UpstreamMiss)

		return false, nil
	}

	u.rec.Upstream(metrics.UpstreamExists, metrics.UpstreamHit)

	return true, nil
}

// Close destroys every pooled connection. Connections currently checked out are destroyed
// when they are checked back in.
func (u *Upstream) Close() error {
	u.mu.Lock()

	if u.closed {
		u.mu.Unlock()

		return nil
	}

	u.closed = true
	idle := u.idle
	u.idle = nil

	u.mu.Unlock()

	for _, c := range idle {
		c.discard()
	}

	return nil
}

// do runs fn on a pooled connection, with a timeout of its own.
//
// THE TIMEOUT IS NOT OPTIONAL. On the hot path the caller's context belongs to the
// DOWNSTREAM CONNECTION and lives for the whole build, so a hung upstream would hold its
// pool permit for hours and starve every caller behind it.
//
// It retries ONCE, on a fresh connection, when a REUSED one broke at the transport level --
// the classic pooled-connection race, where the peer closed an idle connection we had no way
// to know was dead. The retry is safe because EVERY RPC BAKERY ISSUES UPSTREAM IS A READ
// (ping, auth, get, get-outhash, exists-stream); Bakery never writes to its upstream. It is
// NOT retried on an UpstreamInvokeError, which is a deterministic answer and not a blip.
func (u *Upstream) do(ctx context.Context, fn func(context.Context, *UpstreamClient) error) error {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.OpTimeout)
	defer cancel()

	retry, err := u.try(ctx, fn, false)
	if !retry {
		return err
	}

	u.log.Debug("hashserv: pooled upstream connection was stale, retrying on a fresh one",
		"error", err)

	_, err = u.try(ctx, fn, true)

	return err
}

// try checks out one connection, runs fn on it, and checks it back in. retry reports
// whether the failure was the stale-pooled-connection race and is worth one fresh attempt.
func (u *Upstream) try(
	ctx context.Context,
	fn func(context.Context, *UpstreamClient) error,
	fresh bool,
) (retry bool, err error) {
	c, reused, err := u.checkout(ctx, fresh)
	if err != nil {
		return false, err
	}

	err = fn(ctx, c)
	broken := c.broken

	u.checkin(c)

	if err == nil {
		return false, nil
	}

	var invErr UpstreamInvokeError
	if errors.As(err, &invErr) {
		return false, err
	}

	return reused && broken && ctx.Err() == nil, err
}

// checkout takes a permit and then a connection: an idle one if there is one, else a fresh
// dial. reused reports which. fresh forces a dial, for the stale-connection retry.
//
// It BLOCKS when the pool is exhausted, and honors ctx while it does -- a build waiting on
// a permit is waiting on a cold miss, and it would rather wait than see a spurious error.
func (u *Upstream) checkout(ctx context.Context, fresh bool) (*UpstreamClient, bool, error) {
	select {
	case u.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, false, fmt.Errorf("hashserv: waiting for an upstream connection: %w", ctx.Err())
	}

	u.mu.Lock()

	if u.closed {
		u.mu.Unlock()
		<-u.sem

		return nil, false, ErrUpstreamClosed
	}

	if n := len(u.idle); !fresh && n > 0 {
		c := u.idle[n-1]
		u.idle[n-1] = nil
		u.idle = u.idle[:n-1]

		u.mu.Unlock()

		return c, true, nil
	}

	u.mu.Unlock()

	dialCtx, cancel := context.WithTimeout(ctx, u.cfg.DialTimeout)
	defer cancel()

	c, err := DialUpstream(dialCtx, u.cfg)
	if err != nil {
		<-u.sem

		return nil, false, err
	}

	return c, false, nil
}

// checkin returns a connection to the pool -- unless it is broken, in which case it is
// DESTROYED. A broken connection has an unknown stream position (or, after an invoke-error,
// no peer at all), and reusing it hands the next caller the previous caller's answer.
func (u *Upstream) checkin(c *UpstreamClient) {
	defer func() { <-u.sem }()

	if c.broken {
		c.discard()

		return
	}

	u.mu.Lock()

	if u.closed {
		u.mu.Unlock()
		c.discard()

		return
	}

	u.idle = append(u.idle, c)

	u.mu.Unlock()
}

// ---------------------------------------------------------------------------
// The backfill worker.
// ---------------------------------------------------------------------------

// backfillStore is the CONSUMER-SIDE database surface the backfill worker needs: one
// write-once insert and nothing else. The hashserv store satisfies it. Declaring it here
// keeps the worker testable against a hand-written fake and makes its blast radius legible
// (see httpblob.RouteStore for the same idiom).
type backfillStore interface {
	InsertUnihash(ctx context.Context, backendID int64, method, taskhash, unihash string) error
}

// Backfill worker defaults.
const (
	// defaultBackfillQueue bounds the queue. It is BOUNDED because the alternative is
	// worse in exactly the case that matters: a first build against an empty cache misses
	// every lookup, and an unbounded queue would grow one entry per setscene task while
	// the worker drains at one upstream RTT apiece.
	defaultBackfillQueue = 1024

	// defaultBackfillTimeout bounds one backfill (the upstream query plus the insert).
	defaultBackfillTimeout = 30 * time.Second
)

// BackfillConfig configures the write-behind worker.
type BackfillConfig struct {
	// BackendID scopes every insert. It is the multi-tenancy boundary.
	BackendID int64

	QueueSize int
	Workers   int
	Timeout   time.Duration
}

// backfillItem is one queued write-behind. It carries (method, taskhash) and NOT the
// unihash the hot path already saw, matching the reference implementation: the worker
// re-asks upstream. That costs an extra round trip on a path where nothing is waiting, and
// it buys the guarantee that what we PERSIST is what upstream says now, resolved through
// the same validating door as every other upstream answer.
type backfillItem struct {
	method   string
	taskhash string
}

// Backfiller is the write-behind path for `get-stream` misses that upstream can answer.
//
// THE HOT PATH MUST NEVER BLOCK ON IT. `get-stream` is a BB_NUMBER_THREADS-parallel burst of
// tens of thousands of lookups in one connection; the handler returns upstream's unihash to
// the build IMMEDIATELY and drops (method, taskhash) here. So Enqueue never blocks, and when
// the queue is FULL it DROPS and counts. A dropped backfill is a missed optimization -- the
// next build re-asks upstream and gets the same answer. A blocked backfill is a stalled
// build.
type Backfiller struct {
	up        *Upstream
	store     backfillStore
	rec       *metrics.HashservRecorder
	log       *slog.Logger
	backendID int64
	timeout   time.Duration

	queue chan backfillItem

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	dropped atomic.Uint64

	mu sync.Mutex
	// pending is the number of items enqueued and not yet finished -- queued AND
	// in-flight. It is what backfill-wait waits on and what the gauge publishes. Counting
	// only the channel's length would let backfill-wait return while the item currently
	// being written is still in the air, which is precisely the race the RPC exists to
	// close.
	pending int
	// drained is closed when pending reaches zero, and replaced when it leaves zero. It is
	// what lets Wait honor a context, which sync.Cond cannot.
	drained chan struct{}
	closed  bool
}

// NewBackfiller starts the worker. It does not dial: the pool is lazy, so a dead upstream
// costs nothing until something is queued.
func NewBackfiller(
	up *Upstream,
	store backfillStore,
	rec *metrics.HashservRecorder,
	log *slog.Logger,
	cfg BackfillConfig,
) (*Backfiller, error) {
	switch {
	case up == nil:
		return nil, errors.New("hashserv: backfill requires an upstream")
	case store == nil:
		return nil, errors.New("hashserv: backfill requires a store")
	case rec == nil:
		return nil, errors.New("hashserv: backfill requires a metrics recorder")
	}

	if log == nil {
		log = slog.Default()
	}

	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultBackfillQueue
	}

	if cfg.Workers <= 0 {
		cfg.Workers = 1
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultBackfillTimeout
	}

	ctx, cancel := context.WithCancel(context.Background())

	drained := make(chan struct{})
	close(drained)

	b := &Backfiller{
		up:        up,
		store:     store,
		rec:       rec,
		log:       log,
		backendID: cfg.BackendID,
		timeout:   cfg.Timeout,
		queue:     make(chan backfillItem, cfg.QueueSize),
		ctx:       ctx,
		cancel:    cancel,
		drained:   drained,
	}

	rec.SetBackfillQueue(0)

	b.wg.Add(cfg.Workers)

	for range cfg.Workers {
		go b.run()
	}

	return b, nil
}

// Enqueue offers one write-behind. It NEVER BLOCKS. It reports whether the item was
// accepted; false means the queue was full and the backfill was dropped and counted.
func (b *Backfiller) Enqueue(method, taskhash string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return false
	}

	// A non-blocking send, under the lock. It cannot block, so it cannot deadlock the
	// worker's done() -- and it is what keeps pending and the channel in lockstep.
	select {
	case b.queue <- backfillItem{method: method, taskhash: taskhash}:
	default:
		b.dropped.Add(1)
		b.log.Warn("hashserv: backfill queue full, dropping a unihash learned from upstream",
			"method", method, "dropped_total", b.dropped.Load())

		return false
	}

	if b.pending == 0 {
		b.drained = make(chan struct{})
	}

	b.pending++
	b.rec.SetBackfillQueue(b.pending)

	return true
}

// Wait implements `backfill-wait`: it blocks until the queue has drained and returns the
// depth it saw AT ENTRY.
//
// The depth is read and the drain signal captured under ONE lock, so the pair is exact:
// this is the RPC that makes the conformance run deterministic, and a Wait that returns
// while a write is still in the air makes the very assertion it exists to support flaky.
func (b *Backfiller) Wait(ctx context.Context) (int, error) {
	b.mu.Lock()
	depth := b.pending
	drained := b.drained
	b.mu.Unlock()

	select {
	case <-drained:
		return depth, nil
	case <-ctx.Done():
		return depth, fmt.Errorf("hashserv: backfill-wait: %w", ctx.Err())
	}
}

// Dropped is the number of backfills the queue refused. The Prometheus signal for the same
// condition is bakery_hashserv_backfill_queue pinned at its cap.
func (b *Backfiller) Dropped() uint64 { return b.dropped.Load() }

// Close stops the workers and releases anyone in Wait. Queued items are abandoned -- a
// backfill is an optimization, and holding shutdown open for one is not a trade worth
// making.
func (b *Backfiller) Close() error {
	b.mu.Lock()

	if b.closed {
		b.mu.Unlock()

		return nil
	}

	b.closed = true
	b.mu.Unlock()

	b.cancel()
	b.wg.Wait()

	// Whatever is still queued will never be written, so it must not leave a Wait blocked
	// forever on a counter that can no longer reach zero.
	for {
		select {
		case <-b.queue:
			b.done()
		default:
			return nil
		}
	}
}

func (b *Backfiller) run() {
	defer b.wg.Done()

	for {
		select {
		case <-b.ctx.Done():
			return
		case item := <-b.queue:
			b.process(item)
		}
	}
}

// process performs one write-behind: re-ask upstream, validate, insert.
//
// Every exit path goes through done(), including every failure. A backfill that errors and
// forgets to decrement leaves `backfill-wait` blocked forever -- on the conformance run,
// that is a hang, not a failure.
func (b *Backfiller) process(item backfillItem) {
	defer b.done()

	ctx, cancel := context.WithTimeout(b.ctx, b.timeout)
	defer cancel()

	unihash, ok, err := b.up.getUnihash(ctx, metrics.UpstreamBackfill, item.method, item.taskhash)
	if err != nil {
		b.log.Warn("hashserv: backfill upstream lookup failed",
			"method", item.method, "taskhash", item.taskhash, "error", err)

		return
	}

	if !ok {
		// Upstream knew it when the hot path asked and does not now. Nothing to write.
		return
	}

	// Belt and braces: getUnihash already rejects a malformed unihash as an upstream
	// error, and this is the last statement before the DB. A unihash is a third party's
	// string that becomes an sstate object filename and the GC root for it; the cost of
	// checking twice is one regexp on a cold path.
	if !validUnihash(unihash) {
		b.log.Warn("hashserv: refusing to backfill a malformed unihash",
			"method", item.method, "taskhash", item.taskhash)

		return
	}

	if err := b.store.InsertUnihash(ctx, b.backendID, item.method, item.taskhash, unihash); err != nil {
		b.log.Error("hashserv: backfill insert failed",
			"method", item.method, "taskhash", item.taskhash, "error", err)

		return
	}

	b.log.Debug("hashserv: backfilled a unihash from upstream",
		"method", item.method, "taskhash", item.taskhash)
}

// done retires one item, whatever its outcome, and releases Wait when the last one lands.
//
// The zero guard is not defensive dressing: done() and close(b.drained) are the same event,
// so one spurious decrement would close an already-closed channel and panic the worker.
// Every item a successful Enqueue counted is retired exactly once -- by a worker, or by
// Close draining what the workers abandoned -- and this makes that arithmetic unfalsifiable.
func (b *Backfiller) done() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.pending == 0 {
		return
	}

	b.pending--

	if b.pending == 0 {
		close(b.drained)
	}

	b.rec.SetBackfillQueue(b.pending)
}
