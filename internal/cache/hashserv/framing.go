package hashserv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// msgConn is one message-framed, ordered, bidirectional connection.
//
// It exists so the framing -- the part that hangs bitbake forever when it is wrong -- can
// be tested exhaustively against a scripted fake, with no WebSocket and no network. The
// real implementation wraps coder/websocket (conn.go).
//
// Read returns ONE whole message. Write sends ONE whole message. Neither adds nor strips a
// newline: on this transport there are none.
type msgConn interface {
	Read(ctx context.Context) (string, error)
	Write(ctx context.Context, msg string) error
}

// errClosed reports an orderly client disconnect. It is not an error condition -- a build
// finishing and going away is the normal end of every connection.
var errClosed = errors.New("hashserv: connection closed")

// handshake performs the OEHASHEQUIV opening exchange and returns the client's headers.
//
// # The trap
//
// Over WebSocket the handshake is ONE MESSAGE PER LINE, WITH NO NEWLINES ANYWHERE. It is
// not a newline-delimited block, and docs/design/protocols/yocto.md §2.2 -- which shows it
// as "each line \n-terminated" -- is describing the STREAM transports only.
//
// The client sends the handshake through its transport-polymorphic send()
// (bb/asyncrpc/client.py setup_connection). StreamConnection.send appends "\n";
// WebsocketConnection.send does not -- it emits the string as one WebSocket message. So
// over WebSocket the client sends, as three separate messages:
//
//	"OEHASHEQUIV 1.1"
//	"needs-headers: false"
//	""                      <- an EMPTY MESSAGE terminates the headers
//
// A server that waits for "OEHASHEQUIV 1.1\nneeds-headers: false\n\n" in one frame waits
// forever, and so does the build.
//
// A protocol name or version we do not accept is refused BY CLOSING, with no response.
// That is upstream's behavior (asyncrpc/serv.py process_requests simply returns) and the
// client is written to expect it -- a reply here would be read as a response to a request
// that was never sent, desynchronizing the connection.
func handshake(ctx context.Context, c msgConn) (map[string]string, error) {
	greeting, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}

	name, version, ok := strings.Cut(greeting, " ")
	if !ok || name != protoName {
		return nil, fmt.Errorf("hashserv: bad protocol %q", greeting)
	}

	if !acceptProtoVersion(version) {
		return nil, fmt.Errorf("hashserv: unsupported protocol version %q", version)
	}

	headers := map[string]string{}

	for {
		line, err := c.Read(ctx)
		if err != nil {
			return nil, err
		}

		// The empty message, not an empty line. End of headers.
		if line == "" {
			break
		}

		tag, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("hashserv: bad header %q", line)
		}

		headers[strings.ToLower(strings.TrimSpace(tag))] = strings.TrimSpace(value)
	}

	// hashserv's handle_headers() returns {}, so the reply is just the terminating empty
	// message. The mechanism is used by other asyncrpc services (the PR server); we honor
	// it so a client that asks does not block waiting for a reply that never comes.
	if headers["needs-headers"] == "true" {
		if err := c.Write(ctx, ""); err != nil {
			return nil, err
		}
	}

	return headers, nil
}

// acceptProtoVersion mirrors hashserv's validate_proto_version: strictly greater than
// (1,0) and at most (1,1). The protocol string has been "OEHASHEQUIV 1.1" since Dunfell --
// it is the METHOD SET that has grown, not the version -- so this is not a capability
// negotiation and must not be treated as one.
func acceptProtoVersion(v string) bool { return v == protoVersion }

// sendJSON writes a JSON-encoded value: upstream's send_message.
func sendJSON(ctx context.Context, c msgConn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("hashserv: encode response: %w", err)
	}

	return c.Write(ctx, string(b))
}

// streamHandler answers one line of a stream. An empty return is a MISS, and on the wire
// that is an empty message -- which is exactly how the client reads it.
type streamHandler func(ctx context.Context, line string) (string, error)

// runStream implements stream mode, the performance-critical path.
//
// # The two encodings of "ok"
//
// The server sends `"ok"` -- JSON, WITH QUOTES -- to ENTER stream mode, and `ok` -- raw,
// WITHOUT QUOTES -- to LEAVE it. This is not a typo in the reference implementation and it
// is not something either side is lenient about: the client enters via invoke() (which
// JSON-decodes and compares to the string "ok") and leaves via a raw recv() compared
// against the bytes "ok". Send the wrong one and the connection desynchronizes silently.
//
// # Why the client pipelines
//
// The client (hashserv/client.py, class Batch) fires every query as fast as it can while
// concurrently reading the replies, so an entire setscene graph costs ~1 RTT instead of N.
// A full `bitbake core-image-minimal` issues tens of thousands of get-stream lookups; at
// 100ms RTT, un-pipelined that is about an hour. On reconnect the client RESENDS its
// in-flight messages and asserts it got back exactly as many results as it sent -- so a
// server that drops a request without answering, or answers out of order, breaks it. Hence
// one goroutine, strictly in order, no concurrency inside this loop.
//
// Both "END" and an empty message end the stream, and BOTH are followed by the raw `ok`.
func runStream(ctx context.Context, c msgConn, h streamHandler) error {
	if err := sendJSON(ctx, c, "ok"); err != nil {
		return err
	}

	for {
		line, err := c.Read(ctx)
		if err != nil {
			return err
		}

		if line == "" || line == "END" {
			break
		}

		resp, err := h(ctx, line)
		if err != nil {
			return err
		}

		if err := c.Write(ctx, resp); err != nil {
			return err
		}
	}

	return c.Write(ctx, "ok")
}

// parseGetStreamLine splits a get-stream request line into its method and taskhash.
//
// Upstream is `(method, taskhash) = l.split()` -- Python's whitespace split, which requires
// EXACTLY two fields and raises otherwise. A method never contains whitespace (it is the
// value of SSTATE_HASHEQUIV_METHOD, a dotted Python path), so this is safe, but the arity
// must be enforced: a malformed line that silently produced a lookup for the wrong taskhash
// would return a WRONG unihash, and a wrong unihash is a wrong sstate object.
func parseGetStreamLine(line string) (method, taskhash string, err error) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", "", fmt.Errorf("hashserv: bad get-stream line %q", line)
	}

	return fields[0], fields[1], nil
}
