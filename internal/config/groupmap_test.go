package config

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

const validGroupMap = `{
  "site_admin_groups": ["bakery-admins"],
  "orgs": [
    {
      "slug": "acme",
      "note": "the primary tenant",
      "groups": {
        "acme-engineering": "member",
        "acme-leads": "admin",
        "acme-directors": "owner"
      }
    },
    {
      "slug": "initech",
      "groups": { "initech-all": "member" }
    }
  ]
}`

func TestParseGroupMap(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		wantErr bool
	}{
		{name: "valid", doc: validGroupMap, wantErr: false},
		{
			name:    "no orgs means nobody can ever log in",
			doc:     `{"orgs": []}`,
			wantErr: true,
		},
		{
			name:    "an org with no groups is unreachable",
			doc:     `{"orgs": [{"slug": "acme", "groups": {}}]}`,
			wantErr: true,
		},
		{
			name:    "unknown role",
			doc:     `{"orgs": [{"slug": "acme", "groups": {"g": "superuser"}}]}`,
			wantErr: true,
		},
		{
			name:    "project roles are not claim-derived and must not appear here",
			doc:     `{"orgs": [{"slug": "acme", "groups": {"g": "writer"}}]}`,
			wantErr: true,
		},
		{
			name:    "reserved slug can never name a real org",
			doc:     `{"orgs": [{"slug": "cache", "groups": {"g": "member"}}]}`,
			wantErr: true,
		},
		{
			name:    "malformed slug",
			doc:     `{"orgs": [{"slug": "Acme Corp", "groups": {"g": "member"}}]}`,
			wantErr: true,
		},
		{
			name: "duplicate org",
			doc: `{"orgs": [
			        {"slug": "acme", "groups": {"a": "member"}},
			        {"slug": "acme", "groups": {"b": "admin"}}
			      ]}`,
			wantErr: true,
		},
		{
			name:    "empty group name",
			doc:     `{"orgs": [{"slug": "acme", "groups": {"": "member"}}]}`,
			wantErr: true,
		},
		{
			name:    "empty site admin group name",
			doc:     `{"site_admin_groups": [""], "orgs": [{"slug": "acme", "groups": {"g": "member"}}]}`,
			wantErr: true,
		},
		{
			// A typo'd key in an authorization file that parses silently and grants
			// nothing is exactly the failure this refuses to have.
			name:    "unknown field is a typo, not a shrug",
			doc:     `{"site_admin_group": ["x"], "orgs": [{"slug": "acme", "groups": {"g": "member"}}]}`,
			wantErr: true,
		},
		{name: "not json", doc: `orgs: [acme]`, wantErr: true},
		{name: "empty document", doc: ``, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseGroupMap([]byte(tt.doc))

			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseGroupMap() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr && err != nil && !errors.Is(err, ErrInvalidGroupMap) {
				t.Errorf("error %v does not wrap ErrInvalidGroupMap", err)
			}
		})
	}
}

// TestGroupNamesThatYAMLWouldEatParseFine. This is why the file is JSON. A YAML
// parser coerces a group literally named `no`, `on`, `off` or `yes` into a BOOLEAN
// and `1.10` into a float -- and these are OIDC group names from an external IdP, so
// we do not get to choose them. That is a silent auth-mapping bug in the one file
// where a silent auth-mapping bug is least acceptable.
func TestGroupNamesThatYAMLWouldEatParseFine(t *testing.T) {
	doc := `{"orgs": [{"slug": "acme", "groups": {
	  "no": "member", "on": "admin", "off": "member", "yes": "owner", "1.10": "member"
	}}]}`

	gm, err := ParseGroupMap([]byte(doc))
	if err != nil {
		t.Fatalf("ParseGroupMap: %v", err)
	}

	got, err := gm.Resolve([]string{"no", "1.10"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.Orgs["acme"] != OrgRoleMember {
		t.Errorf("acme role = %q, want member -- a group name survived parsing but not resolution", got.Orgs["acme"])
	}
}

func TestGroupMapResolve(t *testing.T) {
	gm, err := ParseGroupMap([]byte(validGroupMap))
	if err != nil {
		t.Fatalf("ParseGroupMap: %v", err)
	}

	tests := []struct {
		name     string
		groups   []string
		wantErr  error
		wantSite SiteRole
		wantOrgs map[string]OrgRole
	}{
		{
			name:     "one group, one org",
			groups:   []string{"acme-engineering"},
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleMember},
		},
		{
			// The highest role wins, NOT whichever the map happened to iterate last.
			// Getting this wrong makes an owner's role flap between runs.
			name:     "highest role wins",
			groups:   []string{"acme-engineering", "acme-directors", "acme-leads"},
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleOwner},
		},
		{
			name:     "order does not matter",
			groups:   []string{"acme-directors", "acme-engineering"},
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleOwner},
		},
		{
			name:     "several orgs",
			groups:   []string{"acme-leads", "initech-all"},
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleAdmin, "initech": OrgRoleMember},
		},
		{
			name:     "site admin",
			groups:   []string{"bakery-admins", "acme-engineering"},
			wantSite: SiteRoleAdmin,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleMember},
		},
		{
			// Users are routinely in dozens of unrelated IdP groups. Those must be
			// ignored, not rejected.
			name:     "unmapped groups are ignored",
			groups:   []string{"vpn-users", "printer-admins", "initech-all"},
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"initech": OrgRoleMember},
		},
		{
			// FAIL CLOSED. Azure AD truncates a >200-group claim into a _claim_names
			// overage, so "no groups" happens to real users. Proceeding with an empty
			// keep-set would make ReconcileOrgMembershipsRemove delete every org
			// membership the user has -- cascading to their project roles and every API
			// key they hold in those orgs, irreversibly.
			name:    "no groups at all is an error, never an empty result",
			groups:  nil,
			wantErr: ErrNoGroups,
		},
		{
			name:    "empty slice is an error",
			groups:  []string{},
			wantErr: ErrNoGroups,
		},
		{
			name:    "groups that map to nothing is an error",
			groups:  []string{"vpn-users"},
			wantErr: ErrNoMappedOrgs,
		},
		{
			// A site admin with no org is still not entitled to anything, so the login
			// is still rejected. The site-admin group is not a backdoor around the org
			// mapping.
			name:    "site admin with no org membership is still rejected",
			groups:  []string{"bakery-admins"},
			wantErr: ErrNoMappedOrgs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gm.Resolve(tt.groups)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve(%v) error = %v, want %v", tt.groups, err, tt.wantErr)
				}

				if len(got.Orgs) != 0 {
					t.Errorf("Resolve returned %d orgs alongside an error; a caller that "+
						"ignored the error would reconcile against them", len(got.Orgs))
				}

				return
			}

			if err != nil {
				t.Fatalf("Resolve(%v): %v", tt.groups, err)
			}

			if got.SiteRole != tt.wantSite {
				t.Errorf("SiteRole = %q, want %q", got.SiteRole, tt.wantSite)
			}

			if !maps.Equal(got.Orgs, tt.wantOrgs) {
				t.Errorf("Orgs = %v, want %v", got.Orgs, tt.wantOrgs)
			}
		})
	}
}

func TestLoadGroupMap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "groups.json")

	if err := os.WriteFile(path, []byte(validGroupMap), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	gm, err := LoadGroupMap(path)
	if err != nil {
		t.Fatalf("LoadGroupMap: %v", err)
	}

	want := []string{"acme", "initech"}
	if got := gm.OrgSlugs(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("OrgSlugs() = %v, want %v", got, want)
	}

	if _, err := LoadGroupMap(filepath.Join(dir, "nope.json")); err == nil {
		t.Error("LoadGroupMap succeeded on a missing file")
	}
}
