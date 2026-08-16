package server

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jsmith212/bakery/internal/config"
	"github.com/jsmith212/bakery/internal/metrics"
)

// TestBuildStorageSelectsTheDriver locks the two properties of the
// --storage-driver switch that a silent regression would hide.
//
//  1. An UNKNOWN driver is an ERROR, never a fallback to local. A deployment
//     that asked for s3 and quietly got a local directory would serve a cold
//     cache off an empty disk forever while every probe stayed green.
//  2. The ZERO VALUE is local. Kong stamps `default:"local"` on every parsed
//     ServeCmd, so "" reaches buildStorage only from a hand-built struct -- and
//     turning that into an error would make TestBootRejectsAnUnusableStorageDir
//     pass on this function's error string instead of on the storage directory
//     it claims to be testing.
//
// No database and no network: buildStorage is reachable on its own.
func TestBuildStorageSelectsTheDriver(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	tests := []struct {
		name    string
		driver  string
		bucket  string
		wantErr string // "" means it must succeed
	}{
		{name: "zero value is local", driver: "", bucket: "", wantErr: ""},
		{name: "local", driver: metrics.DriverLocal, bucket: "", wantErr: ""},
		// The s3 arm without a bucket fails in NewS3's own validation, before
		// any network call -- so this case proves the arm is WIRED without
		// needing a bucket to exist.
		{name: "s3 without a bucket", driver: metrics.DriverS3, bucket: "", wantErr: "s3 bucket is empty"},
		{name: "unknown never falls back", driver: "gcs", bucket: "", wantErr: "unknown storage driver"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := config.ServeCmd{
				StorageDriver: tt.driver,
				StorageDir:    t.TempDir(),
				S3Bucket:      tt.bucket,
			}

			got, err := buildStorage(t.Context(), cmd, metrics.New(), log)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("buildStorage(%q) error = %v, want nil", tt.driver, err)
				}

				if got == nil {
					t.Fatal("buildStorage returned a nil store with a nil error")
				}

				return
			}

			if err == nil {
				t.Fatalf("buildStorage(%q) returned nil error, want %q", tt.driver, tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("buildStorage(%q) error = %v, want it to mention %q", tt.driver, err, tt.wantErr)
			}
		})
	}
}
