package hashserv

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/metrics"
)

// Authenticator turns the token carried by the in-band `auth` RPC into a Principal.
//
// It is NOT AuthenticateCache: there is no *http.Request at this point in the protocol, and
// there cannot be -- credentials arrive inside a WebSocket frame, long after the upgrade.
// *auth.Service's AuthenticateToken satisfies this (through the thin widening adapter in the
// server wiring), and it runs the same constant-time, zero-join, index-only key probe.
type Authenticator interface {
	AuthenticateToken(ctx context.Context, token string) (Principal, error)
}

// backfiller is the write-behind queue for upstream hits found on the streaming hot path.
type backfiller interface {
	// Enqueue never blocks; it reports false when the queue was full and the item was dropped.
	// A full queue DROPS on purpose: losing a backfill costs one future cache lookup, whereas
	// blocking here would stall a build mid-setscene.
	Enqueue(method, taskhash string) bool

	// Wait drains the queue and reports the depth seen on entry. It is what makes a
	// conformance run deterministic.
	Wait(ctx context.Context) (int, error)
}

// session is ONE connection. Everything in it is owned by ONE goroutine -- the serve loop in
// conn.go -- which is what makes the protocol's strict response ordering true by
// construction rather than by discipline. Nothing here may spawn a goroutine that writes.
type session struct {
	conn  msgConn
	store store
	up    upstreamLookup
	bf    backfiller
	authn Authenticator
	route cache.Route
	rec   *metrics.HashservRecorder
	log   *slog.Logger

	// perms is mutable: it starts at whatever the connection is entitled to anonymously and
	// is REPLACED by a successful `auth`. Upstream's client re-authenticates after every
	// reconnect for exactly this reason.
	perms perms
}

// dispatch routes one request and writes exactly one response.
//
// An error returned from here is TERMINAL for the connection. If it is an invokeError it is
// rendered to the client first -- {"invoke-error": ...} -- and then the connection closes.
// That pair is the only loud failure the protocol has, and it is why every auth denial is an
// invokeError: see the package doc for what happens if you 401 the upgrade instead.
func (s *session) dispatch(ctx context.Context, msg []byte) error {
	rpc, payload, err := decodeRequest(msg)
	if err != nil {
		// An unrecognized command is logged and the connection dropped WITHOUT a reply --
		// upstream's ClientError is never sent to the client. Replying would desynchronize a
		// client that is not expecting a response it did not ask for.
		s.rec.RPC(metrics.RPCOther, metrics.RPCError)
		s.log.WarnContext(ctx, "hashserv: dropping connection",
			slog.String("org", s.route.Org), slog.String("project", s.route.Project),
			slog.Any("error", err))

		return err
	}

	err = s.handle(ctx, rpc, payload)

	switch {
	case err == nil:
		s.rec.RPC(rpcLabel(rpc), metrics.RPCOK)
	case isInvokeError(err):
		s.rec.RPC(rpcLabel(rpc), metrics.RPCDenied)
	default:
		s.rec.RPC(rpcLabel(rpc), metrics.RPCError)
	}

	return err
}

func (s *session) handle(ctx context.Context, rpc string, payload json.RawMessage) error {
	switch rpc {
	case rpcPing:
		return sendJSON(ctx, s.conn, map[string]any{"alive": true})

	case rpcAuth:
		return s.handleAuth(ctx, payload)

	case rpcGet:
		return s.handleGet(ctx, payload)

	case rpcGetOuthash:
		return s.handleGetOuthash(ctx, payload)

	case rpcGetStream:
		return s.handleGetStream(ctx)

	case rpcExistsStream:
		return s.handleExistsStream(ctx)

	case rpcReport:
		return s.handleReport(ctx, payload)

	case rpcReportEquiv:
		return s.handleReportEquiv(ctx, payload)

	case rpcRemove:
		return s.handleRemove(ctx, payload)

	case rpcBackfillWait:
		return s.handleBackfillWait(ctx)

	default:
		return errUnknownRPC
	}
}

// require enforces a permission, IN BAND.
//
// The denial is an invokeError, which halts the build. It is emphatically NOT a 401 at the
// WebSocket upgrade: bb.siggen catches the ConnectionError a 401 produces, warns, and carries
// on with unihash = taskhash -- so the build finishes with a silently poisoned cache instead
// of stopping. Loud beats silent here, always.
func (s *session) require(want perms) error {
	if s.perms.has(want) {
		return nil
	}

	if s.perms == 0 {
		return newInvokeError("not authenticated: this cache requires credentials")
	}

	return newInvokeError("not authorized: missing %s permission", permName(want))
}

// handleAuth authenticates the connection and REPLACES its permissions.
//
// A Bakery cache credential is ONE OPAQUE bkry_ TOKEN, not an id:secret pair -- so there is
// no secret half to split off, and we accept the token from EITHER field. That mirrors
// AuthenticateCache's Basic password-then-username fallback, and it is what lets a client
// that was configured for a username/password server work unchanged.
func (s *session) handleAuth(ctx context.Context, payload json.RawMessage) error {
	var req authRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return newInvokeError("auth: malformed request")
	}

	principal, err := s.authenticate(ctx, req)
	if err != nil {
		// Deliberately coarse: unknown, revoked, expired and malformed are indistinguishable.
		// A precise message here is an oracle.
		//
		// And the message carries NEITHER field back. A Bakery credential is one opaque bkry_
		// token that the snippet generator puts in the USERNAME field, so echoing req.Username --
		// as upstream does, where the username is not a secret -- would leak the token into the
		// InvokeError bitbake surfaces and may write to a build log.
		s.log.InfoContext(ctx, "hashserv: auth failed",
			slog.String("org", s.route.Org), slog.String("project", s.route.Project))

		return newInvokeError("unable to authenticate")
	}

	s.perms = grant(principal, s.route)

	if !s.perms.has(permRead) {
		return newInvokeError("not authorized: no access to %s/%s", s.route.Org, s.route.Project)
	}

	return sendJSON(ctx, s.conn, map[string]any{
		"result":      true,
		"username":    req.Username,
		"permissions": s.perms.names(),
	})
}

// authenticate probes the token field, then the username field. The shape check keeps a
// username that is plainly not a token from costing a database round trip.
func (s *session) authenticate(ctx context.Context, req authRequest) (Principal, error) {
	candidates := make([]string, 0, 2)
	for _, f := range []string{req.Token, req.Username} {
		if strings.HasPrefix(f, auth.TokenPrefix) {
			candidates = append(candidates, f)
		}
	}

	if len(candidates) == 0 {
		return nil, errors.New("hashserv: no credential presented")
	}

	var err error

	for _, token := range candidates {
		var p Principal

		if p, err = s.authn.AuthenticateToken(ctx, token); err == nil {
			return p, nil
		}
	}

	return nil, err
}

func (s *session) handleGet(ctx context.Context, payload json.RawMessage) error {
	if err := s.require(permRead); err != nil {
		return err
	}

	var req getRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return newInvokeError("get: malformed request")
	}

	if req.All {
		row, ok, err := s.store.getUnihashFull(ctx, req.Method, req.Taskhash)
		if err != nil {
			return err
		}

		if !ok {
			return sendJSON(ctx, s.conn, nil)
		}

		return sendJSON(ctx, s.conn, row)
	}

	unihash, ok, err := s.store.getUnihash(ctx, req.Method, req.Taskhash)
	if err != nil {
		return err
	}

	if !ok {
		// A miss goes upstream and is WRITTEN THROUGH -- this is the non-streaming path, so
		// there is no hot-path budget to protect and the write is worth doing inline.
		var found bool
		if unihash, found = s.upstreamUnihash(ctx, req.Method, req.Taskhash, true); !found {
			return sendJSON(ctx, s.conn, nil)
		}
	}

	return sendJSON(ctx, s.conn, unihashResponse{
		Taskhash: req.Taskhash, Method: req.Method, Unihash: unihash,
	})
}

// upstreamUnihash asks upstream for a unihash we do not have. writeThrough decides whether
// the row is persisted inline (the `get` path) or handed to the backfill queue (the
// `get-stream` hot path, where an inline write would stall a setscene burst).
func (s *session) upstreamUnihash(ctx context.Context, method, taskhash string, writeThrough bool) (string, bool) {
	if s.up == nil {
		return "", false
	}

	// The pool records the op (hit/miss/error) itself, so this helper does not -- recording here
	// too would double-count every chained query.
	unihash, ok, err := s.up.GetUnihash(ctx, method, taskhash)
	switch {
	case err != nil:
		return "", false
	case !ok:
		return "", false
	case !validUnihash(unihash):
		// Never persist or serve a malformed unihash from a third party. The pool already counted
		// this as a hit; a garbage answer from upstream is rare and not worth a separate series.
		return "", false
	}

	if writeThrough {
		if _, err := s.store.insertUnihash(ctx, method, taskhash, unihash); err != nil {
			s.log.WarnContext(ctx, "hashserv: upstream write-through failed", slog.Any("error", err))
		}
	} else if s.bf != nil {
		// A dropped backfill costs one future lookup. Blocking the hot path to avoid it would
		// cost a stalled build. The queue depth gauge is the operator's signal.
		s.bf.Enqueue(method, taskhash)
	}

	return unihash, true
}

func (s *session) handleGetOuthash(ctx context.Context, payload json.RawMessage) error {
	if err := s.require(permRead); err != nil {
		return err
	}

	var req getOuthashRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return newInvokeError("get-outhash: malformed request")
	}

	// with_unihash defaults to TRUE when the key is absent (upstream:
	// request.get("with_unihash", True)). Defaulting it to false would silently strip the
	// unihash out of every reply to a client that did not name the field.
	withUnihash := req.WithUnihash == nil || *req.WithUnihash

	if !withUnihash {
		row, ok, err := s.store.getOuthashRaw(ctx, req.Method, req.Outhash)
		if err != nil {
			return err
		}

		if !ok {
			return sendJSON(ctx, s.conn, nil)
		}

		return sendJSON(ctx, s.conn, row)
	}

	row, ok, err := s.store.getOuthash(ctx, req.Method, req.Outhash)
	if err != nil {
		return err
	}

	if !ok {
		if s.up == nil {
			return sendJSON(ctx, s.conn, nil)
		}

		// The pool records the upstream op (hit/miss/error) itself, so this path does not.
		up, found, err := s.up.GetOuthash(ctx, req.Method, req.Outhash, req.Taskhash)
		if err != nil || !found {
			return sendJSON(ctx, s.conn, nil)
		}

		// Write through the UPSTREAM row -- binding upstream's own taskhash, not the caller's.
		// Binding req.Taskhash here would be a cache-poisoning primitive; see store.persistUpstream.
		if err := s.store.persistUpstream(ctx, *up); err != nil {
			s.log.WarnContext(ctx, "hashserv: upstream write-through failed", slog.Any("error", err))
		}

		return sendJSON(ctx, s.conn, up)
	}

	return sendJSON(ctx, s.conn, row)
}

// handleGetStream is THE hot path. A full `bitbake core-image-minimal` issues tens of
// thousands of these, pipelined, in a burst at the start of every build.
func (s *session) handleGetStream(ctx context.Context) error {
	if err := s.require(permRead); err != nil {
		return err
	}

	return runStream(ctx, s.conn, func(ctx context.Context, line string) (string, error) {
		s.rec.StreamLine(metrics.StreamGet)

		method, taskhash, err := parseGetStreamLine(line)
		if err != nil {
			return "", err
		}

		unihash, ok, err := s.store.getUnihash(ctx, method, taskhash)
		if err != nil {
			return "", err
		}

		if ok {
			return unihash, nil
		}

		// Miss. Ask upstream, hand the answer back IMMEDIATELY, and let the backfill worker
		// do the write behind us. An empty string is the miss reply, and on this transport
		// that is an empty message.
		if unihash, found := s.upstreamUnihash(ctx, method, taskhash, false); found {
			return unihash, nil
		}

		return "", nil
	})
}

func (s *session) handleExistsStream(ctx context.Context) error {
	if err := s.require(permRead); err != nil {
		return err
	}

	return runStream(ctx, s.conn, func(ctx context.Context, line string) (string, error) {
		s.rec.StreamLine(metrics.StreamExists)

		ok, err := s.store.unihashExists(ctx, line)
		if err != nil {
			return "", err
		}

		// No backfill on this path -- upstream does not do one either. We are answering "could
		// an sstate object named this exist", not minting anything. The pool records the op.
		if !ok && s.up != nil {
			up, err := s.up.UnihashExists(ctx, line)
			ok = err == nil && up
		}

		if ok {
			return "true", nil
		}

		return "false", nil
	})
}

func (s *session) handleReport(ctx context.Context, payload json.RawMessage) error {
	if err := s.require(permRead); err != nil {
		return err
	}

	var req reportRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return newInvokeError("report: malformed request")
	}

	if !validUnihash(req.Unihash) {
		s.rec.ReportDropped(metrics.DropInvalidUnihash)

		return newInvokeError("invalid unihash %q", req.Unihash)
	}

	// permRead but not permReport: the read-only path. Look up, answer, NEVER write. This is
	// upstream's own read-only mode and the right behavior for an open mirror -- but it is a
	// silent non-write, so it is metered. A build whose every report is dropped is a build
	// getting no equivalence at all, and that belongs on a dashboard.
	if !s.perms.has(permReport) {
		s.rec.ReportDropped(metrics.DropReadOnly)

		resp, err := s.store.reportReadOnly(ctx, req)
		if err != nil {
			return err
		}

		return sendJSON(ctx, s.conn, resp)
	}

	resp, equivalent, err := s.store.report(ctx, req, s.up)
	if err != nil {
		s.rec.ReportDropped(metrics.DropError)

		return err
	}

	// The moment the whole system earns its keep: a task whose inputs changed but whose
	// output did not just inherited an existing unihash, so its downstream tasks keep theirs
	// and the rebuild stops propagating.
	if equivalent {
		s.rec.Equivalence()
	}

	return sendJSON(ctx, s.conn, resp)
}

func (s *session) handleReportEquiv(ctx context.Context, payload json.RawMessage) error {
	if err := s.require(permRead); err != nil {
		return err
	}

	var req reportEquivRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return newInvokeError("report-equiv: malformed request")
	}

	if !validUnihash(req.Unihash) {
		s.rec.ReportDropped(metrics.DropInvalidUnihash)

		return newInvokeError("invalid unihash %q", req.Unihash)
	}

	if !s.perms.has(permReport) {
		s.rec.ReportDropped(metrics.DropReadOnly)

		unihash, ok, err := s.store.getUnihash(ctx, req.Method, req.Taskhash)
		if err != nil {
			return err
		}

		if !ok {
			unihash = req.Unihash
		}

		return sendJSON(ctx, s.conn, unihashResponse{
			Taskhash: req.Taskhash, Method: req.Method, Unihash: unihash,
		})
	}

	resp, err := s.store.reportEquiv(ctx, req)
	if err != nil {
		return err
	}

	return sendJSON(ctx, s.conn, resp)
}

// handleRemove purges hash rows. It is the ONE db-admin RPC Bakery serves; the GC RPCs are
// deliberately absent because Bakery garbage-collects in-process (see protocol.go).
//
// It deletes GC ROOTS -- the unihash is what roots every sstate object -- so it demands a
// write-scoped key and refuses any filter column it does not recognize, rather than letting
// an unrecognized filter widen into "delete everything".
func (s *session) handleRemove(ctx context.Context, payload json.RawMessage) error {
	if err := s.require(permDBAdmin); err != nil {
		return err
	}

	var req removeRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return newInvokeError("remove: malformed request")
	}

	count, err := s.store.remove(ctx, req.Where)
	if err != nil {
		return err
	}

	s.log.InfoContext(ctx, "hashserv: remove",
		slog.String("org", s.route.Org), slog.String("project", s.route.Project),
		slog.Int64("count", count))

	return sendJSON(ctx, s.conn, map[string]any{"count": count})
}

// handleBackfillWait blocks until the backfill queue drains, returning the depth it saw on
// entry. Its only purpose is determinism: without it a conformance run races the worker.
func (s *session) handleBackfillWait(ctx context.Context) error {
	if err := s.require(permRead); err != nil {
		return err
	}

	depth := 0

	if s.bf != nil {
		var err error
		if depth, err = s.bf.Wait(ctx); err != nil {
			return err
		}
	}

	return sendJSON(ctx, s.conn, map[string]any{"tasks": depth})
}

// names renders the permission set in upstream's vocabulary, for the `auth` reply.
func (p perms) names() []string {
	out := []string{}

	if p.has(permRead) {
		out = append(out, "@read")
	}

	if p.has(permReport) {
		out = append(out, "@report")
	}

	if p.has(permDBAdmin) {
		out = append(out, "@db-admin")
	}

	return out
}

func permName(p perms) string {
	switch p {
	case permRead:
		return "@read"
	case permReport:
		return "@report"
	case permDBAdmin:
		return "@db-admin"
	default:
		return "@none"
	}
}

func isInvokeError(err error) bool {
	var ie invokeError

	return errors.As(err, &ie)
}

// rpcLabel maps a wire command onto the CLOSED metrics label set. It exists so that the
// wire's `method` field -- an opaque, client-controlled string (it is the value of
// SSTATE_HASHEQUIV_METHOD) -- can never be mistaken for the RPC name and mint one Prometheus
// series per value.
func rpcLabel(rpc string) metrics.HashservRPC {
	switch rpc {
	case rpcPing:
		return metrics.RPCPing
	case rpcAuth:
		return metrics.RPCAuth
	case rpcGet:
		return metrics.RPCGet
	case rpcGetOuthash:
		return metrics.RPCGetOuthash
	case rpcGetStream:
		return metrics.RPCGetStream
	case rpcExistsStream:
		return metrics.RPCExistsStream
	case rpcReport:
		return metrics.RPCReport
	case rpcReportEquiv:
		return metrics.RPCReportEquiv
	case rpcRemove:
		return metrics.RPCRemove
	case rpcBackfillWait:
		return metrics.RPCBackfillWait
	default:
		return metrics.RPCOther
	}
}
