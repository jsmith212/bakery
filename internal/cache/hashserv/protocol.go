// Package hashserv implements the Yocto hash-equivalence server (bitbake's `hashserv`)
// as a WebSocket cache backend.
//
// # Why this package is written the way it is
//
// The protocol has NO REQUEST IDS. Responses are strictly in order, one per request. A
// single dropped or reordered response desynchronizes the connection permanently and
// SILENTLY -- bitbake does not error, it hangs forever at "Checking sstate mirror object
// availability", four hours into someone's build. That is why a connection is served by
// exactly one goroutine (conn.go), and why no handler may spawn something that writes.
//
// The second trap is that AUTH IS DENIED IN-BAND, never with a 401 at the WebSocket
// upgrade. A 401 surfaces in the Python client as InvalidStatus -> ConnectionError, and
// bb.siggen CATCHES ConnectionError, warns, and sets unihash = taskhash -- so the build
// COMPLETES with a silently degraded cache (the sstate object filename embeds the unihash,
// so every task whose unihash we would have remapped now misses its stored object). The
// loud path -- the one that halts the build -- is {"invoke-error": ...} followed by
// closing the connection: InvokeError is in no retry tuple and is caught nowhere on the
// build path. See docs/design/specs/2026-07-13-m3-hashserv.md §2.1.
//
// Only WebSocket is served. The chunked stream framing (the `chunk-stream` sentinel) is a
// property of the raw-TCP/unix transports, which Bakery does not expose; over WebSocket a
// message is one JSON document, whole.
package hashserv

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// The handshake. The client opens with "OEHASHEQUIV 1.1"; upstream accepts any version
// > (1,0) and <= (1,1), i.e. exactly 1.1 today (hashserv server.py validate_proto_version).
// A version we do not accept is refused by CLOSING THE CONNECTION WITH NO RESPONSE --
// that is upstream's behavior and the client is written to expect it.
const (
	protoName    = "OEHASHEQUIV"
	protoVersion = "1.1"
)

// MaxMessageBytes bounds one WebSocket message.
//
// It is not a nicety: coder/websocket defaults to a 32768-byte read limit, and upstream's
// own test_huge_message reports an outhash_siginfo of 131072 bytes. WebSocket messages are
// NOT chunked (chunking exists only on the stream transports), so that arrives as ONE
// frame and the connection dies with StatusMessageTooBig. The collision with bitbake's
// DEFAULT_MAX_CHUNK -- also 32 KiB -- is a coincidence that makes the default look safe.
//
// 1 MiB mirrors Python websockets' own default max_size, deliberately, and in both
// directions: a response larger than that is rejected by the CLIENT, so accepting a larger
// request would let us store a siginfo we could never serve back.
const MaxMessageBytes = 1 << 20

// The RPC names Bakery serves. This is a CLOSED SET and it is deliberately smaller than
// upstream's.
//
// Grepping bitbake's lib/bb (the build path) for every hashserv client method shows a real
// build calls only: ping, auth, get, get-stream, exists-stream, report, report-equiv. The
// GC RPCs (gc-mark, gc-sweep, gc-status, clean-unused, get-db-usage, ...) and the
// user-admin RPCs (new-user, set-user-perms, become-user, refresh-token, delete-user) have
// ZERO call sites there -- they exist for the bitbake-hashclient operator CLI and for
// upstream's own tests.
//
// So they are not implemented, and that is a design position, not an omission:
//
//   - Bakery garbage-collects IN-PROCESS in M6, against the database, under a write barrier
//     with two halves. Exposing upstream's gc_mark/gc-sweep would put a SECOND, COMPETING
//     GC mechanism on the same table, reachable by any cache client, for no production
//     benefit -- a footgun aimed at M6.
//   - Credentials are minted by the Bakery API. A cache client never mints a credential.
//
// An unrecognized command is logged and the connection dropped, which is exactly what
// upstream does (ClientError is never sent to the client).
//
// `remove` IS served: it is project-scoped, needs a write-scoped key, and an operator
// legitimately wants "purge this project's hash equivalence".
const (
	rpcPing         = "ping"
	rpcAuth         = "auth"
	rpcGet          = "get"
	rpcGetOuthash   = "get-outhash"
	rpcGetStream    = "get-stream"
	rpcExistsStream = "exists-stream"
	rpcReport       = "report"
	rpcReportEquiv  = "report-equiv"
	rpcRemove       = "remove"
	rpcBackfillWait = "backfill-wait"
)

// rpcOrder is the dispatch order. Upstream iterates its handler dict and takes the FIRST
// key present in the message, ignoring any others (server.py dispatch_message), so a
// message carrying two known keys is resolved by order, not rejected. Matching that means
// pinning an order rather than ranging over a map.
var rpcOrder = []string{
	rpcPing,
	rpcAuth,
	rpcGet,
	rpcGetOuthash,
	rpcGetStream,
	rpcExistsStream,
	rpcReport,
	rpcReportEquiv,
	rpcRemove,
	rpcBackfillWait,
}

// unihashRE bounds what may be stored as a unihash.
//
// # It is NOT ^[0-9a-f]{64}$, and the difference is load-bearing
//
// The 64-hex UNIHASH_REGEX / is_valid_unihash that the protocol notes describe is a POST-2.8
// addition (see protocols/yocto.md §2.10 -- "unihash validation regex" lands in bitbake
// 2.10+). Our floor is Scarthgap, bitbake 2.8, and there:
//
//   - handle_report does NO validation whatsoever -- there is no regex in lib/hashserv at all;
//   - upstream's OWN test suite reports 40-hex unihashes (tests.py create_test_hash reports
//     "f46d3fbb439bd9b921095da657a4de906510d2cd").
//
// So rejecting anything but 64 hex characters refuses a legitimate Scarthgap client outright.
// It is not a theoretical concern: it failed 11 of the 17 tests in the conformance gate, which
// is precisely the divergence that gate exists to catch. A newer client (2.10+) validates its
// own unihashes to exactly 64 hex before sending them, so accepting the union costs nothing
// there.
//
// Hex-only and length-bounded is retained, rather than accepting anything the way 2.8 does,
// because the unihash is embedded in the sstate OBJECT FILENAME. A unihash carrying a path
// separator or a ".." is not a hash, it is a path, and no supported client has ever emitted
// one -- bitbake's unihash is always a hexdigest.
var unihashRE = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// validUnihash reports whether s is a well-formed unihash.
func validUnihash(s string) bool { return unihashRE.MatchString(s) }

// errUnknownRPC means the message carried no key this server serves. Upstream logs it and
// drops the connection WITHOUT replying; a reply would tell a scanner which build of the
// server it is talking to, and the client is not written to read one.
var errUnknownRPC = errors.New("hashserv: unrecognized command")

// invokeError is the ONE error shape the client understands: {"invoke-error": {"message":
// "..."}} followed by the server closing the connection. It raises InvokeError in the
// client, which is in no retry tuple and is caught nowhere on the build path -- so it
// propagates and HALTS the build.
//
// That loudness is the entire point. Every auth denial is an invokeError. See the package
// doc for what a 401 does instead.
type invokeError struct{ msg string }

func (e invokeError) Error() string { return e.msg }

// newInvokeError builds a client-visible, build-halting error.
func newInvokeError(format string, args ...any) invokeError {
	return invokeError{msg: fmt.Sprintf(format, args...)}
}

// encodeInvokeError renders the wire form.
func encodeInvokeError(e invokeError) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"invoke-error": map[string]any{"message": e.msg},
	})
	if err != nil {
		return nil, fmt.Errorf("hashserv: encode invoke-error: %w", err)
	}

	return b, nil
}

// decodeRequest splits one wire message into its RPC name and raw payload.
//
// A request is a SINGLE-KEY JSON object, {"<method>": <payload>}. It is not JSON-RPC:
// there is no id, no jsonrpc field, no method/params envelope. The payload is whatever
// that method takes -- an object, or null for the streaming commands.
func decodeRequest(msg []byte) (string, json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(msg, &raw); err != nil {
		return "", nil, fmt.Errorf("hashserv: malformed request: %w", err)
	}

	for _, name := range rpcOrder {
		if payload, ok := raw[name]; ok {
			return name, payload, nil
		}
	}

	return "", nil, errUnknownRPC
}

// Request payloads. Only the fields Bakery reads are declared; unknown fields are ignored,
// which is what upstream does and what keeps us forward-compatible with a newer client.

type getRequest struct {
	Method   string `json:"method"`
	Taskhash string `json:"taskhash"`
	All      bool   `json:"all"`
}

// getOuthashRequest carries with_unihash as a POINTER because its default is TRUE, not false
// (upstream: request.get("with_unihash", True)). A plain bool would silently default the
// field off and strip the unihash from every reply to a client that did not name it.
type getOuthashRequest struct {
	Method      string `json:"method"`
	Outhash     string `json:"outhash"`
	Taskhash    string `json:"taskhash"`
	WithUnihash *bool  `json:"with_unihash"`
}

// reportRequest is the payload of `report`, the RPC that carries the whole point of the
// system. outhash_siginfo is the large one -- upstream's own suite sends 128 KiB of it.
type reportRequest struct {
	Method         string `json:"method"`
	Taskhash       string `json:"taskhash"`
	Outhash        string `json:"outhash"`
	Unihash        string `json:"unihash"`
	Owner          string `json:"owner"`
	PN             string `json:"PN"`
	PV             string `json:"PV"`
	PR             string `json:"PR"`
	Task           string `json:"task"`
	OuthashSiginfo string `json:"outhash_siginfo"`
}

// reportEquivRequest directly asserts (method, taskhash) -> unihash with no outhash in
// play. bitbake uses it to retro-map a taskhash onto a unihash it already knows.
type reportEquivRequest struct {
	Method   string `json:"method"`
	Taskhash string `json:"taskhash"`
	Unihash  string `json:"unihash"`
}

type authRequest struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

// removeRequest carries an arbitrary single-column filter: {"where": {"<col>": "<val>"}}.
// Only the columns upstream's suite actually filters on are served; anything else is an
// invoke-error rather than a silently empty delete, because a delete that quietly matches
// nothing is indistinguishable from one that worked.
type removeRequest struct {
	Where map[string]string `json:"where"`
}

// unihashResponse is the reply to get, report and report-equiv alike.
type unihashResponse struct {
	Taskhash string `json:"taskhash"`
	Method   string `json:"method"`
	Unihash  string `json:"unihash"`
}

// outhashResponse is the joined outhash row: every column of the outhash table plus the
// unihash. Returned by get-outhash, and by get with "all": true.
type outhashResponse struct {
	Method         string `json:"method"`
	Taskhash       string `json:"taskhash"`
	Outhash        string `json:"outhash"`
	Created        string `json:"created"`
	Owner          string `json:"owner,omitempty"`
	PN             string `json:"PN,omitempty"`
	PV             string `json:"PV,omitempty"`
	PR             string `json:"PR,omitempty"`
	Task           string `json:"task,omitempty"`
	OuthashSiginfo string `json:"outhash_siginfo,omitempty"`
	Unihash        string `json:"unihash,omitempty"`
}
