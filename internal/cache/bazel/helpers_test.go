package bazel

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"

	"google.golang.org/grpc/metadata"

	"github.com/jsmith212/bakery/internal/metrics"
)

func testMetrics() *metrics.Metrics { return metrics.New() }

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// bearerCtx returns a context carrying an incoming gRPC "authorization: Bearer <tok>".
func bearerCtx(token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)

	return metadata.NewIncomingContext(context.Background(), md)
}

// basicCtx returns a context carrying "authorization: Basic base64(user:pass)".
func basicCtx(user, pass string) context.Context {
	md := metadata.Pairs("authorization", "Basic "+basicB64(user, pass))

	return metadata.NewIncomingContext(context.Background(), md)
}

// basicB64 encodes "user:pass" the way an HTTP Basic credential does.
func basicB64(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// equalStrings compares two string slices, treating nil and empty as equal.
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
