package middleware

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

type responseRecorder struct {
	http.ResponseWriter

	status int
	bytes  int
}

func (rr *responseRecorder) WriteHeader(code int) {
	rr.status = code
	rr.ResponseWriter.WriteHeader(code)
}

func (rr *responseRecorder) Write(b []byte) (int, error) {
	n, err := rr.ResponseWriter.Write(b)
	rr.bytes += n

	return n, err //nolint:wrapcheck // transparent pass-through of the wrapped writer
}

// writerOnly hides every method of the wrapped value except Write, so that
// io.Copy cannot rediscover ReadFrom on it and recurse back into the recorder.
type writerOnly struct{ io.Writer }

// ReadFrom forwards to the wrapped writer's io.ReaderFrom when it has one. This
// is what keeps the sendfile fast path alive on /cache GETs: net/http's
// *response implements ReadFrom (zero-copy from an *os.File), and http.ServeContent
// streams via io.CopyN, which only takes that path when the destination it is
// handed exposes ReadFrom. Because we wrap the ResponseWriter, we must re-expose
// it, or every multi-GB sstate download degrades to a 32 KiB userspace copy.
func (rr *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := rr.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		rr.bytes += int(n)

		return n, err //nolint:wrapcheck // transparent pass-through of the wrapped writer
	}

	// No fast path available: copy through Write so byte accounting still holds.
	// writerOnly prevents io.Copy from finding this recorder's own ReadFrom.
	n, err := io.Copy(writerOnly{rr}, src)

	return n, err //nolint:wrapcheck // transparent pass-through of the wrapped writer
}

// Flush forwards to the wrapped writer when it is an http.Flusher.
func (rr *responseRecorder) Flush() {
	if f, ok := rr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the wrapped writer when it is an http.Hijacker (the
// hashserv wss upgrade will need this once it lands behind this middleware).
func (rr *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := rr.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack() //nolint:wrapcheck // transparent pass-through of the wrapped writer
	}

	return nil, nil, http.ErrNotSupported
}

// RequestLogger logs one structured line per completed request.
func RequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rr := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rr, r)

		slog.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rr.status,
			"bytes", rr.bytes,
			"remote", r.RemoteAddr,
			"userAgent", r.UserAgent(),
			"duration", time.Since(start),
		)
	})
}
