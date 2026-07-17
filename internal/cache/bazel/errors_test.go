package bazel

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/jsmith212/bakery/internal/blob"
	"github.com/jsmith212/bakery/internal/cache"
)

func TestGrpcErrorMapping(t *testing.T) {
	b := &Backend{deps: cache.Deps{Logger: testLogger(), Metrics: testMetrics()}}

	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"compressed -> Unimplemented", errCompressedResource, codes.Unimplemented},
		{"unparseable resource -> InvalidArgument", errInvalidResource, codes.InvalidArgument},
		{"size mismatch -> InvalidArgument", errSizeMismatch, codes.InvalidArgument},
		{"digest mismatch -> InvalidArgument", blob.ErrDigestMismatch, codes.InvalidArgument},
		{"invalid key -> InvalidArgument", blob.ErrInvalidKey, codes.InvalidArgument},
		{"wrapped size mismatch still InvalidArgument", fmt.Errorf("x: %w", errSizeMismatch), codes.InvalidArgument},
		{"context canceled -> Canceled (client hung up; not a server fault)", context.Canceled, codes.Canceled},
		{"context deadline -> DeadlineExceeded", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"wrapped cancel still Canceled", fmt.Errorf("get: %w", context.Canceled), codes.Canceled},
		{"anything else -> Internal", errors.New("boom"), codes.Internal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := grpcstatus.Code(b.grpcError(context.Background(), "T", tt.err))
			if got != tt.want {
				t.Fatalf("code = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCredentialCandidates(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
		want    []string
	}{
		{"bearer token", []string{"Bearer bkry_abc"}, []string{"bkry_abc"}},
		{"bearer lowercase scheme", []string{"bearer bkry_abc"}, []string{"bkry_abc"}},
		{"bearer non-token dropped", []string{"Bearer notatoken"}, nil},
		{
			name:    "basic offers password FIRST then username",
			headers: []string{"Basic " + basicB64("bkry_user", "bkry_pass")},
			want:    []string{"bkry_pass", "bkry_user"},
		},
		{
			name:    "basic token in password only",
			headers: []string{"Basic " + basicB64("x", "bkry_tok")},
			want:    []string{"bkry_tok"},
		},
		{
			name:    "basic token in username only (empty password)",
			headers: []string{"Basic " + basicB64("bkry_tok", "")},
			want:    []string{"bkry_tok"},
		},
		{"schemeless bare token still authenticates", []string{"bkry_abc"}, []string{"bkry_abc"}},
		{"garbage header ignored", []string{"nonsense"}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := credentialCandidates(tt.headers)
			if !equalStrings(got, tt.want) {
				t.Fatalf("candidates = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitInstance(t *testing.T) {
	tests := []struct {
		in       string
		wantOrg  string
		wantProj string
		wantOK   bool
	}{
		{"acme/widget", "acme", "widget", true},
		{"acme", "", "", false},
		{"acme/widget/extra", "", "", false},
		{"/widget", "", "", false},
		{"acme/", "", "", false},
		{"", "", "", false},
	}

	for _, tt := range tests {
		org, proj, ok := splitInstance(tt.in)
		if ok != tt.wantOK || org != tt.wantOrg || proj != tt.wantProj {
			t.Errorf("splitInstance(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tt.in, org, proj, ok, tt.wantOrg, tt.wantProj, tt.wantOK)
		}
	}
}
