package config

import (
	"path/filepath"
	"testing"
)

// TestExampleGroupMapParses. groups.example.json is the file operators copy. If it
// does not parse, the first thing everybody does is hit a boot failure, and the
// documentation is a lie.
func TestExampleGroupMapParses(t *testing.T) {
	gm, err := LoadGroupMap(filepath.Join("..", "..", "groups.example.json"))
	if err != nil {
		t.Fatalf("groups.example.json does not parse: %v", err)
	}

	res, err := gm.Resolve([]string{"bakery-admins", "acme-leads"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if res.SiteRole != SiteRoleAdmin {
		t.Errorf("SiteRole = %q, want admin", res.SiteRole)
	}

	if res.Orgs["acme"] != OrgRoleAdmin {
		t.Errorf("acme role = %q, want admin", res.Orgs["acme"])
	}
}
