package hashserv

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedConn is a msgConn driven by a fixed list of inbound messages, recording every
// outbound one. It is the whole point of the msgConn seam: the framing -- the code whose
// failure mode is "bitbake hangs forever with no error" -- is proven here with no
// WebSocket, no network and no database.
type scriptedConn struct {
	in   []string
	next int
	out  []string

	readErr error // returned once in is exhausted; errClosed by default
}

func newScriptedConn(in ...string) *scriptedConn {
	return &scriptedConn{in: in, readErr: errClosed}
}

func (c *scriptedConn) Read(_ context.Context) (string, error) {
	if c.next >= len(c.in) {
		return "", c.readErr
	}

	msg := c.in[c.next]
	c.next++

	return msg, nil
}

func (c *scriptedConn) Write(_ context.Context, msg string) error {
	c.out = append(c.out, msg)

	return nil
}

func TestHandshake(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          []string
		wantHeaders map[string]string
		wantOut     []string
		wantErr     bool
	}{
		{
			name:        "the stock client: greeting, needs-headers false, empty message",
			in:          []string{"OEHASHEQUIV 1.1", "needs-headers: false", ""},
			wantHeaders: map[string]string{"needs-headers": "false"},
			wantOut:     nil,
		},
		{
			// The reply is a single EMPTY MESSAGE: hashserv's handle_headers() returns {},
			// so there are no header lines to send, only the terminator. A client that asked
			// for headers blocks until it arrives.
			name:        "needs-headers true is answered with the empty terminator",
			in:          []string{"OEHASHEQUIV 1.1", "needs-headers: true", ""},
			wantHeaders: map[string]string{"needs-headers": "true"},
			wantOut:     []string{""},
		},
		{
			name:        "header tags are lowercased and values trimmed",
			in:          []string{"OEHASHEQUIV 1.1", "Needs-Headers:   false  ", "X-Thing: v", ""},
			wantHeaders: map[string]string{"needs-headers": "false", "x-thing": "v"},
		},
		{
			name:        "no headers at all, just the terminator",
			in:          []string{"OEHASHEQUIV 1.1", ""},
			wantHeaders: map[string]string{},
		},
		{
			// THE TRAP. Over WebSocket the handshake is one message per line. A client that
			// sent it as a single newline-delimited blob would be a stream-transport client,
			// and we must not accept it -- accepting it would mean we had built the framing
			// to the wrong spec and would then mis-frame every reply.
			name:    "a newline-delimited blob is NOT a valid websocket handshake",
			in:      []string{"OEHASHEQUIV 1.1\nneeds-headers: false\n\n"},
			wantErr: true,
		},
		{
			name:    "wrong protocol name is refused",
			in:      []string{"OEHASHEQUIV2 1.1", ""},
			wantErr: true,
		},
		{
			name:    "protocol 1.0 is refused: upstream requires > (1,0)",
			in:      []string{"OEHASHEQUIV 1.0", ""},
			wantErr: true,
		},
		{
			name:    "protocol 2.0 is refused: upstream requires <= (1,1)",
			in:      []string{"OEHASHEQUIV 2.0", ""},
			wantErr: true,
		},
		{
			name:    "greeting with no version is refused",
			in:      []string{"OEHASHEQUIV", ""},
			wantErr: true,
		},
		{
			name:    "a header with no colon is refused",
			in:      []string{"OEHASHEQUIV 1.1", "garbage", ""},
			wantErr: true,
		},
		{
			name:    "a client that hangs up mid-handshake",
			in:      []string{"OEHASHEQUIV 1.1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newScriptedConn(tt.in...)

			got, err := handshake(context.Background(), c)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("handshake() = %v, want error", got)
				}

				return
			}

			if err != nil {
				t.Fatalf("handshake() error = %v", err)
			}

			if len(got) != len(tt.wantHeaders) {
				t.Fatalf("headers = %v, want %v", got, tt.wantHeaders)
			}

			for k, want := range tt.wantHeaders {
				if got[k] != want {
					t.Errorf("header %q = %q, want %q", k, got[k], want)
				}
			}

			if !equalStrings(c.out, tt.wantOut) {
				t.Errorf("wrote %q, want %q", c.out, tt.wantOut)
			}
		})
	}
}

// TestStreamOkEncodingsDiffer is the single most important assertion in this package.
//
// The server sends `"ok"` (JSON, quoted) to ENTER stream mode and `ok` (raw, unquoted) to
// LEAVE it. The client reads the first with a JSON decode and the second as raw bytes. Emit
// the same encoding for both and the connection desynchronizes SILENTLY -- no error, no
// close, just a build that hangs forever at "Checking sstate mirror object availability".
//
// If someone "cleans up" runStream to use one helper for both writes, this test is what
// stops them shipping it.
func TestStreamOkEncodingsDiffer(t *testing.T) {
	t.Parallel()

	c := newScriptedConn("END")

	err := runStream(context.Background(), c, func(context.Context, string) (string, error) {
		t.Fatal("handler must not run: END ends the stream immediately")

		return "", nil
	})
	if err != nil {
		t.Fatalf("runStream() error = %v", err)
	}

	want := []string{`"ok"`, "ok"}
	if !equalStrings(c.out, want) {
		t.Fatalf("wrote %q, want %q -- entry is JSON-quoted, exit is raw", c.out, want)
	}

	if c.out[0] == c.out[1] {
		t.Fatal("entry and exit encodings of ok are identical; they must not be")
	}
}

func TestRunStream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      []string
		handler streamHandler
		wantOut []string
	}{
		{
			name: "hits and misses keep strict order; a miss is an EMPTY MESSAGE",
			in:   []string{"m a", "m b", "m c", "END"},
			handler: func(_ context.Context, line string) (string, error) {
				// b is a miss.
				if strings.HasSuffix(line, "b") {
					return "", nil
				}

				return "uni-" + line, nil
			},
			wantOut: []string{`"ok"`, "uni-m a", "", "uni-m c", "ok"},
		},
		{
			// Upstream: `if not l: break`. An empty message ends the stream just as END
			// does -- and, critically, the raw `ok` is still sent afterwards.
			name:    "an empty message ends the stream, and still gets the trailing ok",
			in:      []string{"m a", ""},
			handler: func(context.Context, string) (string, error) { return "u", nil },
			wantOut: []string{`"ok"`, "u", "ok"},
		},
		{
			name:    "an immediately-empty stream is still well-formed",
			in:      []string{""},
			handler: func(context.Context, string) (string, error) { return "", nil },
			wantOut: []string{`"ok"`, "ok"},
		},
		{
			name: "exists-stream answers the literal strings true and false",
			in:   []string{"aaa", "bbb", "END"},
			handler: func(_ context.Context, line string) (string, error) {
				if line == "aaa" {
					return "true", nil
				}

				return "false", nil
			},
			wantOut: []string{`"ok"`, "true", "false", "ok"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newScriptedConn(tt.in...)

			if err := runStream(context.Background(), c, tt.handler); err != nil {
				t.Fatalf("runStream() error = %v", err)
			}

			if !equalStrings(c.out, tt.wantOut) {
				t.Fatalf("wrote %q, want %q", c.out, tt.wantOut)
			}
		})
	}
}

// TestRunStreamNeverDropsAResponse pins the invariant the client's Batch asserts on: it
// counts replies and requires exactly one per request it sent. One missing reply and the
// client's result list is misaligned -- every subsequent lookup returns the PREVIOUS task's
// unihash, which is a wrong sstate object, not an error.
func TestRunStreamNeverDropsAResponse(t *testing.T) {
	t.Parallel()

	const n = 500

	in := make([]string, 0, n+1)
	for i := range n {
		in = append(in, "method task"+string(rune('a'+i%26))+strings.Repeat("x", i%7))
	}

	in = append(in, "END")

	c := newScriptedConn(in...)

	calls := 0
	err := runStream(context.Background(), c, func(_ context.Context, line string) (string, error) {
		calls++

		// Every other line is a miss, so the empty-message replies are counted too.
		if calls%2 == 0 {
			return "", nil
		}

		return "u" + line, nil
	})
	if err != nil {
		t.Fatalf("runStream() error = %v", err)
	}

	if calls != n {
		t.Fatalf("handler ran %d times, want %d", calls, n)
	}

	// entry "ok" + one reply per request + exit ok.
	if want := n + 2; len(c.out) != want {
		t.Fatalf("wrote %d messages, want %d (one reply per request, plus both oks)", len(c.out), want)
	}
}

func TestRunStreamHandlerErrorAborts(t *testing.T) {
	t.Parallel()

	c := newScriptedConn("m a", "m b", "END")
	boom := errors.New("boom")

	err := runStream(context.Background(), c, func(context.Context, string) (string, error) {
		return "", boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("runStream() error = %v, want %v", err, boom)
	}

	// Only the entry "ok" was written. No half-answer, and no trailing ok that would tell
	// the client the stream ended cleanly when it did not.
	if !equalStrings(c.out, []string{`"ok"`}) {
		t.Fatalf("wrote %q, want only the entry ok", c.out)
	}
}

func TestParseGetStreamLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		line         string
		wantMethod   string
		wantTaskhash string
		wantErr      bool
	}{
		{
			name:         "the real thing",
			line:         "oe.sstatesig.OEOuthashBasic 8a1c0b2f",
			wantMethod:   "oe.sstatesig.OEOuthashBasic",
			wantTaskhash: "8a1c0b2f",
		},
		{
			name:         "extra whitespace is collapsed, as Python's split() does",
			line:         "  method   taskhash  ",
			wantMethod:   "method",
			wantTaskhash: "taskhash",
		},
		{
			// A silently-accepted malformed line would look up the WRONG taskhash and return
			// a wrong unihash -- and a wrong unihash names a wrong sstate object. Upstream
			// raises here; so do we.
			name:    "one field is refused, not guessed at",
			line:    "method",
			wantErr: true,
		},
		{
			name:    "three fields are refused",
			line:    "method taskhash extra",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			line:    "   ",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			method, taskhash, err := parseGetStreamLine(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseGetStreamLine(%q) = %q %q, want error", tt.line, method, taskhash)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseGetStreamLine(%q) error = %v", tt.line, err)
			}

			if method != tt.wantMethod || taskhash != tt.wantTaskhash {
				t.Errorf("parseGetStreamLine(%q) = %q %q, want %q %q",
					tt.line, method, taskhash, tt.wantMethod, tt.wantTaskhash)
			}
		})
	}
}

func TestDecodeRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		msg         string
		wantRPC     string
		wantPayload string
		wantErr     error
	}{
		{
			name:        "a normal single-key request",
			msg:         `{"get": {"taskhash": "abc", "method": "m", "all": false}}`,
			wantRPC:     rpcGet,
			wantPayload: `{"taskhash": "abc", "method": "m", "all": false}`,
		},
		{
			name:        "the streaming commands carry a null payload",
			msg:         `{"get-stream": null}`,
			wantRPC:     rpcGetStream,
			wantPayload: `null`,
		},
		{
			name:        "ping carries an empty object",
			msg:         `{"ping": {}}`,
			wantRPC:     rpcPing,
			wantPayload: `{}`,
		},
		{
			// Upstream ignores extra keys: it takes the first handler name present. The
			// dispatch ORDER is therefore part of the protocol, which is why rpcOrder is a
			// slice and not a map.
			name:        "an unknown extra key is ignored, not an error",
			wantRPC:     rpcPing,
			msg:         `{"ping": {}, "nonsense": 1}`,
			wantPayload: `{}`,
		},
		{
			name:    "a command we deliberately do not serve is unknown",
			msg:     `{"gc-sweep": {"mark": "m"}}`,
			wantErr: errUnknownRPC,
		},
		{
			name:    "the user-admin surface does not exist",
			msg:     `{"new-user": {"username": "x", "permissions": []}}`,
			wantErr: errUnknownRPC,
		},
		{
			name:    "an empty object names no command",
			msg:     `{}`,
			wantErr: errUnknownRPC,
		},
		{
			name:    "malformed json",
			msg:     `{"get":`,
			wantErr: nil, // some error; not errUnknownRPC
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rpc, payload, err := decodeRequest([]byte(tt.msg))

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("decodeRequest(%s) error = %v, want %v", tt.msg, err, tt.wantErr)
				}

				return
			}

			if tt.wantRPC == "" {
				if err == nil {
					t.Fatalf("decodeRequest(%s) = %q, want an error", tt.msg, rpc)
				}

				return
			}

			if err != nil {
				t.Fatalf("decodeRequest(%s) error = %v", tt.msg, err)
			}

			if rpc != tt.wantRPC {
				t.Errorf("rpc = %q, want %q", rpc, tt.wantRPC)
			}

			if string(payload) != tt.wantPayload {
				t.Errorf("payload = %s, want %s", payload, tt.wantPayload)
			}
		})
	}
}

func TestValidUnihash(t *testing.T) {
	t.Parallel()

	const good = "a69ec97f5af2e21e1a1f9cc8896965515d5559425666f734e245a3d40cee33d9"

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "64 lowercase hex, what a 2.10+ client sends", in: good, want: true},
		{
			// THE REGRESSION. bitbake 2.8 (Scarthgap -- our floor) does not validate unihashes at
			// all, and its own test suite reports this exact 40-hex value. Rejecting it refuses a
			// legitimate client and failed 11 of 17 tests in the conformance gate.
			name: "40 hex, which upstream's own 2.8 suite reports",
			in:   "f46d3fbb439bd9b921095da657a4de906510d2cd",
			want: true,
		},
		{name: "short hex", in: "abc123", want: true},
		{name: "empty", in: "", want: false},
		{name: "longer than 64 is rejected", in: good + "a", want: false},
		{name: "uppercase is rejected", in: strings.ToUpper(good), want: false},
		{name: "non-hex", in: strings.Repeat("g", 64), want: false},
		{
			// The unihash is embedded in the sstate object FILENAME, so a path separator in one
			// is not a hash, it is a traversal.
			name: "a path separator is not a hash",
			in:   "../../etc/passwd",
			want: false,
		},
		{name: "a slash is rejected", in: "abc/def", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := validUnihash(tt.in); got != tt.want {
				t.Errorf("validUnihash(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
