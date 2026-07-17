package bazel

import (
	"errors"
	"testing"
)

func TestParseResourceName(t *testing.T) {
	const h = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // any 64-hex

	tests := []struct {
		name         string
		in           string
		wantInstance string
		wantHash     string
		wantSize     int64
		wantErr      error
	}{
		{
			name:         "read sha256 (digest function omitted, 2-seg tail)",
			in:           "acme/widget/blobs/" + h + "/1234",
			wantInstance: "acme/widget",
			wantHash:     h,
			wantSize:     1234,
		},
		{
			name:         "read blake3 (digest function present, 3-seg tail)",
			in:           "acme/widget/blobs/blake3/" + h + "/42",
			wantInstance: "acme/widget",
			wantHash:     h,
			wantSize:     42,
		},
		{
			name:         "write sha256 with uploads/{uuid}",
			in:           "acme/widget/uploads/2a8b-uuid/blobs/" + h + "/9",
			wantInstance: "acme/widget",
			wantHash:     h,
			wantSize:     9,
		},
		{
			name:         "write with trailing metadata after size",
			in:           "acme/widget/uploads/uuid/blobs/" + h + "/9/some/metadata",
			wantInstance: "acme/widget",
			wantHash:     h,
			wantSize:     9,
		},
		{
			name:         "write blake3 (3-seg tail) with metadata",
			in:           "acme/widget/uploads/uuid/blobs/blake3/" + h + "/9/meta",
			wantInstance: "acme/widget",
			wantHash:     h,
			wantSize:     9,
		},
		{
			name:         "multi-segment instance name (deeper than org/project)",
			in:           "a/b/c/blobs/" + h + "/5",
			wantInstance: "a/b/c",
			wantHash:     h,
			wantSize:     5,
		},
		{
			name:         "instance segment CONTAINS the literal string blobs but is not it",
			in:           "acme/blobstore/blobs/" + h + "/7",
			wantInstance: "acme/blobstore",
			wantHash:     h,
			wantSize:     7,
		},
		{
			name:         "empty instance name",
			in:           "blobs/" + h + "/0",
			wantInstance: "",
			wantHash:     h,
			wantSize:     0,
		},
		{
			name:    "compressed-blobs read -> Unimplemented sentinel",
			in:      "acme/widget/compressed-blobs/zstd/" + h + "/100",
			wantErr: errCompressedResource,
		},
		{
			name:    "compressed-blobs write -> Unimplemented sentinel",
			in:      "acme/widget/uploads/uuid/compressed-blobs/zstd/" + h + "/100",
			wantErr: errCompressedResource,
		},
		{
			name:    "no marker at all",
			in:      "acme/widget/nonsense/" + h + "/5",
			wantErr: errInvalidResource,
		},
		{
			name:    "tail too short (hash but no size)",
			in:      "acme/widget/blobs/" + h,
			wantErr: errInvalidResource,
		},
		{
			name:    "size not a number",
			in:      "acme/widget/blobs/" + h + "/notanumber",
			wantErr: errInvalidResource,
		},
		{
			name:    "negative size",
			in:      "acme/widget/blobs/" + h + "/-3",
			wantErr: errInvalidResource,
		},
		{
			name:    "write missing blob marker after uuid",
			in:      "acme/widget/uploads/uuid",
			wantErr: errInvalidResource,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResourceName(tt.in)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}

			if got.instance != tt.wantInstance {
				t.Errorf("instance = %q, want %q", got.instance, tt.wantInstance)
			}

			if got.hash != tt.wantHash {
				t.Errorf("hash = %q, want %q", got.hash, tt.wantHash)
			}

			if got.size != tt.wantSize {
				t.Errorf("size = %d, want %d", got.size, tt.wantSize)
			}
		})
	}
}
