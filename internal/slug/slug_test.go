package slug

import (
	"errors"
	"testing"
)

// cases is this package's grammar table. internal/db's TestSlugMirrorsDatabase
// replays an equivalent table against a real Postgres, and additionally drives
// slug.Reserved() itself through bakery_slug_ok -- so the denylist, the part most
// likely to drift, is checked against the live function rather than a copy.
var cases = []struct {
	Name  string
	Slug  string
	Valid bool
}{
	{Name: "simple", Slug: "acme", Valid: true},
	{Name: "digits", Slug: "acme2", Valid: true},
	{Name: "hyphenated", Slug: "acme-widgets", Valid: true},
	{Name: "starts with digit", Slug: "2acme", Valid: true},
	{Name: "single character", Slug: "a", Valid: true},
	{Name: "single digit", Slug: "9", Valid: true},
	{Name: "63 characters", Slug: "a12345678901234567890123456789012345678901234567890123456789012", Valid: true},

	{Name: "empty", Slug: "", Valid: false},
	{Name: "64 characters", Slug: "a123456789012345678901234567890123456789012345678901234567890123", Valid: false},
	{Name: "uppercase", Slug: "Acme", Valid: false},
	{Name: "leading hyphen", Slug: "-acme", Valid: false},
	{Name: "trailing hyphen", Slug: "acme-", Valid: false},
	{Name: "underscore", Slug: "acme_widgets", Valid: false},
	{Name: "slash", Slug: "acme/widgets", Valid: false},
	{Name: "dot", Slug: "acme.widgets", Valid: false},
	{Name: "space", Slug: "acme widgets", Valid: false},
	{Name: "single hyphen", Slug: "-", Valid: false},

	{Name: "reserved blobs", Slug: "blobs", Valid: false},
	{Name: "reserved uploads", Slug: "uploads", Valid: false},
	{Name: "reserved actions", Slug: "actions", Valid: false},
	{Name: "reserved actionresults", Slug: "actionresults", Valid: false},
	{Name: "reserved operations", Slug: "operations", Valid: false},
	{Name: "reserved capabilities", Slug: "capabilities", Valid: false},
	{Name: "reserved compressed-blobs", Slug: "compressed-blobs", Valid: false},
	{Name: "reserved ac", Slug: "ac", Valid: false},
	{Name: "reserved cas", Slug: "cas", Valid: false},
	{Name: "reserved v2", Slug: "v2", Valid: false},
	{Name: "reserved api", Slug: "api", Valid: false},
	{Name: "reserved cache", Slug: "cache", Valid: false},

	// The camelCase spelling in the spec is unrepresentable under the grammar,
	// which is exactly why the lowercase form has to be on the denylist.
	{Name: "camelCase actionResults is invalid by grammar", Slug: "actionResults", Valid: false},
	// Reserved words are only reserved verbatim.
	{Name: "reserved word as a prefix is fine", Slug: "cache-server", Valid: true},
	{Name: "reserved word as a suffix is fine", Slug: "my-cache", Valid: true},
}

func TestValid(t *testing.T) {
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			if got := Valid(tt.Slug); got != tt.Valid {
				t.Errorf("Valid(%q) = %v, want %v", tt.Slug, got, tt.Valid)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		slug    string
		wantErr error
	}{
		{name: "valid", slug: "acme", wantErr: nil},
		{name: "malformed", slug: "Acme", wantErr: ErrInvalid},
		{name: "reserved", slug: "cache", wantErr: ErrReserved},
		{name: "empty", slug: "", wantErr: ErrInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Check(tt.slug)

			switch {
			case tt.wantErr == nil && err != nil:
				t.Errorf("Check(%q) = %v, want nil", tt.slug, err)
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Errorf("Check(%q) = %v, want %v", tt.slug, err, tt.wantErr)
			}
		})
	}
}

func TestReservedIsACopy(t *testing.T) {
	got := Reserved()
	if len(got) == 0 {
		t.Fatal("Reserved() is empty")
	}

	got[0] = "clobbered"

	if IsReserved("clobbered") {
		t.Error("mutating the slice returned by Reserved() changed the denylist")
	}

	if !IsReserved("blobs") {
		t.Error(`mutating the slice returned by Reserved() removed "blobs" from the denylist`)
	}
}
