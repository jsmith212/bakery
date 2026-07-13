package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateStackOrdering(t *testing.T) {
	var order []string

	tag := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	handler := CreateStack(tag("first"), tag("second"), tag("third"))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			order = append(order, "handler")
		}),
	)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "third", "handler"}

	if len(order) != len(want) {
		t.Fatalf("got call order %v, want %v", order, want)
	}

	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("got call order %v, want %v", order, want)
		}
	}
}

func TestCreateStackEmpty(t *testing.T) {
	handler := CreateStack()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("got status %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestRequestLoggerPassesResponseThrough(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "ok", status: http.StatusOK, body: "ok\n"},
		{name: "created", status: http.StatusCreated, body: "made"},
		{name: "not found", status: http.StatusNotFound, body: "missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := RequestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)

				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("write: %v", err)
				}
			}))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != tt.status {
				t.Errorf("got status %d, want %d", rec.Code, tt.status)
			}

			if got := rec.Body.String(); got != tt.body {
				t.Errorf("got body %q, want %q", got, tt.body)
			}
		})
	}
}

// readerFromWriter models what net/http's *response gives a /cache handler: a
// ResponseWriter that also implements io.ReaderFrom for the zero-copy sendfile
// path. http.ServeContent's io.CopyN only takes that path when the destination
// it is handed exposes ReadFrom, so the recorder must forward to it.
type readerFromWriter struct {
	http.ResponseWriter

	readFromCalled bool
	body           bytes.Buffer
}

func (w *readerFromWriter) ReadFrom(src io.Reader) (int64, error) {
	w.readFromCalled = true

	return io.Copy(&w.body, src)
}

// The recorder must expose io.ReaderFrom and delegate to the wrapped writer's
// ReadFrom, or every multi-GB /cache download loses the sendfile fast path and
// falls back to a 32 KiB userspace copy. Before the ReadFrom forwarder existed,
// responseRecorder embedded only the http.ResponseWriter interface (whose method
// set has no ReadFrom), so this assertion failed and the delegation never ran.
func TestResponseRecorderForwardsReaderFrom(t *testing.T) {
	underlying := &readerFromWriter{ResponseWriter: httptest.NewRecorder()}
	rr := &responseRecorder{ResponseWriter: underlying, status: http.StatusOK}

	rf, ok := any(rr).(io.ReaderFrom)
	if !ok {
		t.Fatal("responseRecorder must implement io.ReaderFrom to preserve the sendfile fast path")
	}

	const payload = "a multi-gig sstate tarball, in spirit"

	n, err := rf.ReadFrom(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	if !underlying.readFromCalled {
		t.Error("ReadFrom did not delegate to the wrapped writer; sendfile fast path is defeated")
	}

	if n != int64(len(payload)) {
		t.Errorf("got n=%d, want %d", n, len(payload))
	}

	if got := underlying.body.String(); got != payload {
		t.Errorf("got forwarded body %q, want %q", got, payload)
	}

	if rr.bytes != len(payload) {
		t.Errorf("got %d bytes accounted, want %d", rr.bytes, len(payload))
	}
}

// When the wrapped writer has no ReadFrom (e.g. the S3-deferred non-seekable
// path, or a plain recorder), ReadFrom must still copy the bytes through and
// keep byte accounting intact -- without recursing back into itself.
func TestResponseRecorderReadFromFallback(t *testing.T) {
	rec := httptest.NewRecorder()

	if _, ok := any(rec).(io.ReaderFrom); ok {
		t.Skip("httptest.ResponseRecorder unexpectedly implements io.ReaderFrom; fallback path not exercised")
	}

	rr := &responseRecorder{ResponseWriter: rec, status: http.StatusOK}

	const payload = "buffered copy still accounts every byte"

	n, err := rr.ReadFrom(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}

	if n != int64(len(payload)) {
		t.Errorf("got n=%d, want %d", n, len(payload))
	}

	if got := rec.Body.String(); got != payload {
		t.Errorf("got written body %q, want %q", got, payload)
	}

	if rr.bytes != len(payload) {
		t.Errorf("got %d bytes accounted, want %d", rr.bytes, len(payload))
	}
}

// A handler that never calls WriteHeader still sends a 200, and the logged line
// has to report it as such rather than as a zero status.
func TestRequestLoggerDefaultsToOK(t *testing.T) {
	var buf bytes.Buffer

	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	handler := RequestLogger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("implicit")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var line struct {
		Status int `json:"status"`
		Bytes  int `json:"bytes"`
	}

	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("parse log line %q: %v", buf.String(), err)
	}

	if line.Status != http.StatusOK {
		t.Errorf("got logged status %d, want %d", line.Status, http.StatusOK)
	}

	if line.Bytes != len("implicit") {
		t.Errorf("got %d bytes logged, want %d", line.Bytes, len("implicit"))
	}
}
