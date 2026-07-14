package hashserv

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/coder/websocket"
)

// wsConn adapts a WebSocket to msgConn: one message in, one message out, no newlines.
//
// It is the ONLY place a *websocket.Conn is touched, and exactly one goroutine ever holds
// one -- see serve. That is not a style choice. The protocol has no request ids, so responses
// must be strictly ordered, one per request; two goroutines writing to this connection would
// interleave two responses and desynchronize it permanently and SILENTLY. bitbake would then
// hang forever with no error, four hours into a build. If a future change makes it possible
// for a second goroutine to reach a wsConn, the design is already wrong.
type wsConn struct{ c *websocket.Conn }

func (w wsConn) Read(ctx context.Context) (string, error) {
	typ, data, err := w.c.Read(ctx)
	if err != nil {
		if isClosed(err) {
			return "", errClosed
		}

		return "", fmt.Errorf("hashserv: read: %w", err)
	}

	// bitbake's client speaks TEXT frames exclusively (json.dumps -> socket.send(str)). A
	// binary frame is not something the protocol can express, so it is a protocol error, not
	// something to coerce.
	if typ != websocket.MessageText {
		return "", fmt.Errorf("hashserv: unexpected %v frame", typ)
	}

	return string(data), nil
}

func (w wsConn) Write(ctx context.Context, msg string) error {
	if err := w.c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
		if isClosed(err) {
			return errClosed
		}

		return fmt.Errorf("hashserv: write: %w", err)
	}

	return nil
}

func isClosed(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, websocket.CloseError{}) ||
		websocket.CloseStatus(err) != -1
}

// serve runs one connection to completion, in ONE goroutine.
//
// The shape is: handshake, then a loop of (read one request, write exactly one response).
// Nothing in here is concurrent, and nothing it calls may become concurrent.
//
// # How a connection ends
//
// Three ways, and the difference between them is the difference between a build that fails
// loudly and a build that silently ships a poisoned cache:
//
//   - The client sends nothing more and hangs up. Normal. Every build ends this way.
//   - A request is denied or malformed -> we write {"invoke-error": ...} and CLOSE. The client
//     raises InvokeError, which is in no retry tuple and is caught nowhere on the build path,
//     so it propagates and HALTS the build. This is the loud path, and every auth denial takes
//     it.
//   - An unrecognized command, or a broken handshake -> we close with NO reply. That is
//     upstream's behavior (its ClientError is logged, never sent), and a reply here would be
//     read as an answer to a request the client never sent.
func (s *session) serve(ctx context.Context) error {
	if _, err := handshake(ctx, s.conn); err != nil {
		return err
	}

	for {
		msg, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}

		if err := s.dispatch(ctx, []byte(msg)); err != nil {
			return s.fail(ctx, err)
		}
	}
}

// fail renders a terminal error and reports whether it was orderly.
//
// An invokeError is the one error the client understands, so it is written before the close.
// Anything else -- an unrecognized command, a broken frame, a dead database -- closes in
// silence, which is what upstream does.
func (s *session) fail(ctx context.Context, err error) error {
	var ie invokeError
	if !errors.As(err, &ie) {
		return err
	}

	b, encErr := encodeInvokeError(ie)
	if encErr != nil {
		return errors.Join(err, encErr)
	}

	if writeErr := s.conn.Write(ctx, string(b)); writeErr != nil {
		return errors.Join(err, writeErr)
	}

	return err
}

// accept upgrades an HTTP request to a WebSocket.
//
// SetReadLimit is MANDATORY and it is the reason this helper exists rather than callers
// calling websocket.Accept directly. coder/websocket defaults to a 32768-byte read limit;
// upstream's own test_huge_message reports a 131072-byte outhash_siginfo, and WebSocket
// messages are not chunked, so it arrives as ONE frame and the connection dies with
// StatusMessageTooBig. The default LOOKS safe because it happens to equal bitbake's
// DEFAULT_MAX_CHUNK -- which applies only to the stream transports, not this one.
func accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// bitbake sets ping_interval=None on both ends: there are no WebSocket pings on this
		// protocol, in either direction. Nothing here may depend on them for liveness.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("hashserv: websocket upgrade: %w", err)
	}

	c.SetReadLimit(MaxMessageBytes)

	return c, nil
}
