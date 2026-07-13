package config

import (
	"errors"
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

	res, err := gm.Resolve(GroupsClaim{
		Groups:  []string{"bakery-users", "bakery-admins", "acme-leads"},
		Present: true,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if res.SiteRole != SiteRoleAdmin {
		t.Errorf("SiteRole = %q, want admin", res.SiteRole)
	}

	if res.Orgs["acme"] != OrgRoleAdmin {
		t.Errorf("acme role = %q, want admin", res.Orgs["acme"])
	}

	// The example configures a login gate, so it must actually gate: a user outside
	// login_groups is refused even though acme-leads maps to an org. If the example
	// ships a gate that does not gate, the operator who copies it believes they have
	// one.
	if _, err := gm.Resolve(GroupsClaim{Groups: []string{"acme-leads"}, Present: true}); !errors.Is(err, ErrNotInLoginGroup) {
		t.Errorf("Resolve() outside the login gate = %v, want ErrNotInLoginGroup", err)
	}
}
