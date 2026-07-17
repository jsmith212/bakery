package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"

	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/cache/bazel"
	"github.com/jsmith212/bakery/internal/cache/httpblob"
	"github.com/jsmith212/bakery/internal/metrics"
)

// A raw []byte gRPC codec, registered globally so a test can drive a hand-rolled
// server-streaming service without generated proto code. grpc selects it by
// content-subtype, which the client sets with grpc.CallContentSubtype.
const rawCodecName = "bakery-raw-test"

type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec.Marshal: want *[]byte, got %T", v)
	}

	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec.Unmarshal: want *[]byte, got %T", v)
	}

	*b = append((*b)[:0], data...)

	return nil
}

func (rawCodec) Name() string { return rawCodecName }

func init() { encoding.RegisterCodec(rawCodec{}) }

const (
	drainServiceName = "bakery.test.Drainer"
	drainMethod      = "/bakery.test.Drainer/Drain"
)

// TestGRPCListenerBindsAndDrains proves the two load-bearing facts about the third
// listener at once:
//
//  1. BIND BEFORE SERVE. Run binds public + gRPC + metrics before serving any, so Ready
//     fires with three non-nil addresses. A port clash on any of them is a startup error,
//     not a half-started server -- and if any listener were skipped, one of these would be
//     nil.
//  2. GracefulStop DRAINS an in-flight stream. The gRPC server runs on its OWN
//     net.Listener, which is the only reason GracefulStop is safe: that path builds
//     grpc-go's real http2Server (Drain implemented). On the shared ServeHTTP transport
//     Drain is `panic("Drain() is not implemented")` and GracefulStop panics -- but only
//     with an RPC in flight, so a naive test passes. This test holds a stream open ACROSS
//     shutdown and asserts the server delivers the remaining frame and closes the stream
//     cleanly (io.EOF, not an Unavailable hard-cut), which a hard Stop() would not do.
func TestGRPCListenerBindsAndDrains(t *testing.T) {
	// release lets the streaming handler finish its second send. Kept blocked until the
	// test has a frame in flight AND shutdown has begun, so the drain window is real.
	release := make(chan struct{})

	grpcSrv := grpc.NewServer()
	grpcSrv.RegisterService(&grpc.ServiceDesc{
		ServiceName: drainServiceName,
		HandlerType: (*any)(nil),
		Streams: []grpc.StreamDesc{{
			StreamName:    "Drain",
			ServerStreams: true,
			Handler: func(_ any, stream grpc.ServerStream) error {
				first := []byte("frame-1")
				if err := stream.SendMsg(&first); err != nil {
					return err
				}

				<-release // the RPC is now in flight, spanning shutdown

				second := []byte("frame-2")

				return stream.SendMsg(&second)
			},
		}},
	}, nil)

	srv := New(Config{
		Addr:        "127.0.0.1:0",
		Version:     "test",
		GRPC:        grpcSrv,
		GRPCAddr:    "127.0.0.1:0",
		Metrics:     metrics.New(),
		MetricsAddr: "127.0.0.1:0",
		Dist:        testDist(),
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	type addrs struct{ public, grpc, metrics net.Addr }

	ready := make(chan addrs, 1)
	srv.ready = func(public, grpcAddr, metricsAddr net.Addr) {
		ready <- addrs{public, grpcAddr, metricsAddr}
	}

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	var got addrs

	select {
	case got = <-ready:
	case err := <-done:
		t.Fatalf("Run returned before binding: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("server never became ready")
	}

	// 1. All three listeners were bound before serving. A skipped listener is a nil addr.
	if got.grpc == nil {
		t.Fatal("gRPC listener address is nil -- the third listener never bound")
	}

	if got.metrics == nil {
		t.Fatal("metrics listener address is nil")
	}

	// The public and metrics listeners really serve.
	if body := get(t, "http://"+got.public.String()+"/healthz"); body != "ok\n" {
		t.Errorf("public /healthz: got %q", body)
	}

	// The gRPC listener really serves: open a stream and receive the first frame, so an
	// RPC is genuinely in flight when shutdown begins.
	cc, err := grpc.NewClient("passthrough:///"+got.grpc.String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gRPC: %v", err)
	}

	defer cc.Close()

	// A SEPARATE context for the client: the stream must outlive the shutdown cancel()
	// below, or "the server drained it" and "the client cancelled it" are
	// indistinguishable. This is what proves the drain came from GracefulStop.
	clientCtx, clientCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer clientCancel()

	stream, err := cc.NewStream(clientCtx, &grpc.StreamDesc{ServerStreams: true}, drainMethod,
		grpc.CallContentSubtype(rawCodecName))
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}

	var buf []byte
	if err := stream.RecvMsg(&buf); err != nil {
		t.Fatalf("recv first frame: %v", err)
	}

	if string(buf) != "frame-1" {
		t.Fatalf("first frame = %q, want frame-1", buf)
	}

	// 2. Begin shutdown WITH the stream in flight, then let the handler finish. A hard
	// Stop() would sever the stream; GracefulStop must let it complete.
	cancel()
	close(release)

	if err := stream.RecvMsg(&buf); err != nil {
		t.Fatalf("recv second frame: the stream was cut mid-flight instead of drained: %v", err)
	}

	if string(buf) != "frame-2" {
		t.Fatalf("second frame = %q, want frame-2", buf)
	}

	if err := stream.RecvMsg(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("stream close: got %v, want a clean io.EOF -- GracefulStop did not drain", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

// TestBazelAndSccacheMountInBothModes proves the M4 boot wiring: the bazel backend
// (its /ac and /cas HTTP mounts) and the sibling sccache backend register beside the SPA,
// /api/v1 and the M2/M3 backends in BOTH console and headless mode -- "no console" is not
// "no cache" -- and only the bazel backend gets its gRPC services registered.
//
// The backend slice is built exactly as Boot builds it (bazel.New OWNS its /ac + /cas
// mounts; sccache is a sibling httpblob backend), so a regression in that construction is
// caught here without a database.
func TestBazelAndSccacheMountInBothModes(t *testing.T) {
	deps := cache.Deps{Metrics: metrics.New()}

	// An unconfigured route: every /cache request 404s BEFORE auth or blob access, so nil
	// authenticators and a nil blob service are never reached. That is precisely the path
	// the invariant demands ("a project/kind with no cache_backends row 404s"), and it is
	// what lets this test run without a DB.
	routes := stubRoutes{ok: false}

	backends := []cache.Backend{
		bazel.New(deps, routes, nil, nil),
		httpblob.NewSccache(deps, routes, nil),
	}

	// The M4 routes the two backends must own. Each 404s (unconfigured) but MUST reach the
	// backend -- not the SPA catch-all, whose tell is a 200 + the console shell.
	targets := []struct {
		name   string
		method string
		target string
	}{
		{"ac reaches the bazel backend", http.MethodGet, "/cache/acme/widget/ac/deadbeef"},
		{"cas reaches the bazel backend", http.MethodGet, "/cache/acme/widget/cas/deadbeef"},
		{"cas HEAD reaches the bazel backend", http.MethodHead, "/cache/acme/widget/cas/deadbeef"},
		{"sccache reaches its backend", http.MethodGet, "/cache/acme/widget/sccache/aa/bb/deadbeef"},
	}

	for _, headless := range []bool{false, true} {
		handler := NewHandler(Config{
			Dist:          testDist(),
			Headless:      headless,
			API:           http.NotFoundHandler(),
			CacheBackends: backends,
		})

		for _, tc := range targets {
			t.Run(fmt.Sprintf("headless=%v/%s", headless, tc.name), func(t *testing.T) {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))

				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s = %d, want 404 (unconfigured, but ROUTED to the backend)",
						tc.method, tc.target, rec.Code)
				}

				if body := rec.Body.String(); strings.Contains(body, "<title>bakery</title>") {
					t.Errorf("%s %s returned the console shell -- the SPA catch-all swallowed the "+
						"route, so the backend was never mounted", tc.method, tc.target)
				}

				if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
					t.Errorf("%s %s served Content-Type %q -- a cache client would ingest HTML",
						tc.method, tc.target, ct)
				}
			})
		}
	}

	// Only the bazel backend speaks gRPC. The boot loop registers exactly the backends
	// that implement cache.GRPCBackend, and a registration failure must be an error, never
	// a panic.
	grpcSrv := grpc.NewServer()

	registered := 0

	for _, b := range backends {
		gb, ok := b.(cache.GRPCBackend)
		if !ok {
			continue
		}

		if err := gb.RegisterGRPC(grpcSrv); err != nil {
			t.Fatalf("RegisterGRPC(%s): %v", b.Kind(), err)
		}

		registered++
	}

	if registered != 1 {
		t.Fatalf("%d backends registered gRPC services, want exactly 1 (bazel)", registered)
	}

	if _, ok := backends[1].(cache.GRPCBackend); ok {
		t.Error("the sccache backend implements cache.GRPCBackend -- it must not; it is HTTP-only")
	}
}
