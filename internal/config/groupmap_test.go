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
			// INVERTED, DELIBERATELY. This case used to be "no orgs means nobody can
			// ever log in" and asserted an error, because org membership WAS the login
			// gate. It is not any more: the gate is login_groups, and a deployment can
			// run entirely on local org grants. An org-free file is now exactly right,
			// and refusing it would refuse the very deployment this milestone exists to
			// enable.
			name:    "no orgs is fine now: a deployment can run entirely on local grants",
			doc:     `{"orgs": []}`,
			wantErr: false,
		},
		{
			name:    "orgs may be omitted entirely",
			doc:     `{"login_groups": ["bakery-users"]}`,
			wantErr: false,
		},
		{
			name:    "a login gate with no orgs and no site admins is a valid policy",
			doc:     `{}`,
			wantErr: false,
		},
		{
			// An empty LIST admits anyone; an empty STRING gates on a group nobody can
			// be in, which locks everybody out. That is what a stray "" looks like.
			name:    "empty login group name locks everyone out",
			doc:     `{"login_groups": [""], "orgs": []}`,
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
		{
			// The login gate has the worst failure mode of the lot: a typo'd
			// `login_group` parses to an EMPTY LoginGroups, which admits everyone who
			// can authenticate. DisallowUnknownFields is what stops it.
			name:    "a typo'd login_groups must not silently open the gate",
			doc:     `{"login_group": ["bakery-users"], "orgs": []}`,
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

	got, err := gm.Resolve(GroupsClaim{Groups: []string{"no", "1.10"}, Present: true})
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
		claim    GroupsClaim
		wantErr  error
		wantSite SiteRole
		wantOrgs map[string]OrgRole
	}{
		{
			name:     "one group, one org",
			claim:    present("acme-engineering"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleMember},
		},
		{
			// The highest role wins, NOT whichever the map happened to iterate last.
			// Getting this wrong makes an owner's role flap between runs.
			name:     "highest role wins",
			claim:    present("acme-engineering", "acme-directors", "acme-leads"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleOwner},
		},
		{
			name:     "order does not matter",
			claim:    present("acme-directors", "acme-engineering"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleOwner},
		},
		{
			name:     "several orgs",
			claim:    present("acme-leads", "initech-all"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleAdmin, "initech": OrgRoleMember},
		},
		{
			name:     "site admin",
			claim:    present("bakery-admins", "acme-engineering"),
			wantSite: SiteRoleAdmin,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleMember},
		},
		{
			// Users are routinely in dozens of unrelated IdP groups. Those must be
			// ignored, not rejected.
			name:     "unmapped groups are ignored",
			claim:    present("vpn-users", "printer-admins", "initech-all"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"initech": OrgRoleMember},
		},
		{
			// UNCHANGED, AND THE POINT OF THE WHOLE EXERCISE. An unreadable claim is
			// still refused: Azure AD replaces a >200-group claim with a `_claim_names`
			// overage, so the `groups` claim is simply ABSENT for real,
			// correctly-configured users. Reconciling it as "zero groups" would NULL
			// every oidc_role, delete every membership row with no local grant, and
			// cascade away the user's project roles and every API key they hold in
			// those orgs. Irreversible.
			name:    "an unreadable claim is refused, never treated as an empty result",
			claim:   GroupsClaim{Present: false},
			wantErr: ErrGroupsUnreadable,
		},
		{
			// The zero value fails closed: a caller who forgets to set Present refuses
			// the login instead of wiping the user's memberships.
			name:    "the zero-value claim fails closed",
			claim:   GroupsClaim{},
			wantErr: ErrGroupsUnreadable,
		},
		{
			// Belt and braces: even a non-empty group list is refused if we were told
			// the claim was not readable. Present is the authority, not len(Groups).
			name:    "groups alongside Present=false are not trusted",
			claim:   GroupsClaim{Groups: []string{"acme-directors"}, Present: false},
			wantErr: ErrGroupsUnreadable,
		},
		{
			// INVERTED, DELIBERATELY. This case used to be "empty slice is an error"
			// (ErrNoGroups). A GENUINELY EMPTY claim is an ordinary state now: it means
			// "this user has only local memberships". Admit them, and reconcile the OIDC
			// half to nothing. Conflating this with the case above is the whole trap.
			name:     "a genuinely empty claim is admitted with zero orgs",
			claim:    GroupsClaim{Groups: []string{}, Present: true},
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{},
		},
		{
			// INVERTED, DELIBERATELY. Was "groups that map to nothing is an error"
			// (ErrNoMappedOrgs). Org membership is no longer the login gate, so
			// resolving to zero orgs is not a refusal -- the user may hold local
			// memberships this file cannot see.
			name:     "groups that map to no org resolve to zero orgs, not an error",
			claim:    present("vpn-users"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{},
		},
		{
			// INVERTED, DELIBERATELY. Was "site admin with no org membership is still
			// rejected". A site admin who is in no org group is exactly the operator who
			// has to create the first org, and refusing them was the dead-end that
			// started this milestone.
			name:     "site admin with no org membership is admitted",
			claim:    present("bakery-admins"),
			wantSite: SiteRoleAdmin,
			wantOrgs: map[string]OrgRole{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gm.Resolve(tt.claim)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve(%v) error = %v, want %v", tt.claim, err, tt.wantErr)
				}

				if len(got.Orgs) != 0 || got.SiteRole != "" {
					t.Errorf("Resolve returned site %q and %d orgs alongside an error; a caller "+
						"that ignored the error would reconcile against them", got.SiteRole, len(got.Orgs))
				}

				return
			}

			if err != nil {
				t.Fatalf("Resolve(%v): %v", tt.claim, err)
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

// present is a readable groups claim. The bare-slice form is deliberately not
// available: a test must say which of the two states it means.
func present(groups ...string) GroupsClaim {
	return GroupsClaim{Groups: groups, Present: true}
}

// TestLoginGate. The gate answers "may this human use Bakery at all?", which is a
// DIFFERENT question from "which orgs are they in" -- answering the second to
// answer the first is what forced a group per org into the directory forever.
func TestLoginGate(t *testing.T) {
	const gated = `{
	  "login_groups": ["bakery-users", "bakery-contractors"],
	  "site_admin_groups": ["bakery-admins"],
	  "orgs": [{"slug": "acme", "groups": {"acme-engineering": "member"}}]
	}`

	tests := []struct {
		name     string
		doc      string
		claim    GroupsClaim
		wantErr  error
		wantSite SiteRole
		wantOrgs map[string]OrgRole
	}{
		{
			name:     "in a login group, admitted",
			doc:      gated,
			claim:    present("bakery-users", "acme-engineering"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{"acme": OrgRoleMember},
		},
		{
			name:     "any one login group is enough",
			doc:      gated,
			claim:    present("bakery-contractors"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{},
		},
		{
			// The gate is the gate. An org group does not smuggle you past it.
			name:    "in no login group, refused even with a mapped org group",
			doc:     gated,
			claim:   present("acme-engineering"),
			wantErr: ErrNotInLoginGroup,
		},
		{
			// Nor does the site-admin group. If the gate says no, it is no.
			name:    "in no login group, refused even as a site admin",
			doc:     gated,
			claim:   present("bakery-admins"),
			wantErr: ErrNotInLoginGroup,
		},
		{
			name:    "an empty claim is refused when the gate is configured",
			doc:     gated,
			claim:   GroupsClaim{Groups: []string{}, Present: true},
			wantErr: ErrNotInLoginGroup,
		},
		{
			// EMPTY login_groups ADMITS ANY SUCCESSFUL OIDC AUTH. The IdP has already
			// decided who may authenticate; a deployment whose tenant is its user base
			// should not have to restate that as a group.
			name:     "an unset gate admits anyone who authenticated",
			doc:      `{"orgs": [{"slug": "acme", "groups": {"acme-engineering": "member"}}]}`,
			claim:    present("some-unrelated-group"),
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{},
		},
		{
			name:     "an explicitly empty gate admits anyone who authenticated",
			doc:      `{"login_groups": [], "orgs": []}`,
			claim:    GroupsClaim{Groups: []string{}, Present: true},
			wantSite: SiteRoleUser,
			wantOrgs: map[string]OrgRole{},
		},
		{
			// THE TRAP DETECTION IS NOT PART OF THE GATE, AND MUST NOT DEPEND ON IT.
			// The gate and the reconciler consume the same claim, so an unreadable claim
			// must refuse with ErrGroupsUnreadable whether the gate is on...
			name:    "an unreadable claim is refused as unreadable, gate on",
			doc:     gated,
			claim:   GroupsClaim{Present: false},
			wantErr: ErrGroupsUnreadable,
		},
		{
			// ...or off. If this ever returns nil, an absent claim reconciles as "zero
			// groups" on every deployment that did not configure a gate.
			name:    "an unreadable claim is refused as unreadable, gate off",
			doc:     `{"orgs": []}`,
			claim:   GroupsClaim{Present: false},
			wantErr: ErrGroupsUnreadable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gm, err := ParseGroupMap([]byte(tt.doc))
			if err != nil {
				t.Fatalf("ParseGroupMap: %v", err)
			}

			got, err := gm.Resolve(tt.claim)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve(%v) error = %v, want %v", tt.claim, err, tt.wantErr)
				}

				if len(got.Orgs) != 0 || got.SiteRole != "" {
					t.Errorf("Resolve returned site %q and %d orgs alongside an error; a caller "+
						"that ignored the error would reconcile against them", got.SiteRole, len(got.Orgs))
				}

				return
			}

			if err != nil {
				t.Fatalf("Resolve(%v): %v", tt.claim, err)
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

// TestOrgFreeGroupMapResolves: the file that exists ONLY to gate logins and name
// site admins. Every org membership in such a deployment is granted in-app. This
// whole document used to be a boot failure.
func TestOrgFreeGroupMapResolves(t *testing.T) {
	gm, err := ParseGroupMap([]byte(`{
	  "login_groups": ["bakery-users"],
	  "site_admin_groups": ["bakery-admins"]
	}`))
	if err != nil {
		t.Fatalf("an org-free group map must parse: %v", err)
	}

	if got := gm.OrgSlugs(); len(got) != 0 {
		t.Errorf("OrgSlugs() = %v, want none", got)
	}

	got, err := gm.Resolve(present("bakery-users", "bakery-admins"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got.SiteRole != SiteRoleAdmin {
		t.Errorf("SiteRole = %q, want admin", got.SiteRole)
	}

	if len(got.Orgs) != 0 {
		t.Errorf("Orgs = %v, want none -- this file maps no orgs", got.Orgs)
	}
}

// TestResolveNamesTheGroupThatConferredSiteAdmin.
//
// The site-admin listing has to say `ldap: platform-admins`, not merely "the claims
// said so" -- a hybrid site role is only safe while a LOCAL grant that outlived its
// LDAP revocation is DISTINGUISHABLE from a live directory membership on the screen.
// That distinction starts here: if Resolve cannot name the group, nothing downstream
// can, because site_oidc_group is what it writes.
//
// It is also DETERMINISTIC, unlike OrgGroups: site_admin_groups is a slice, so the
// first group the user holds wins in file order. Two site-admin groups must not make
// the recorded provenance a coin toss.
func TestResolveNamesTheGroupThatConferredSiteAdmin(t *testing.T) {
	gm, err := ParseGroupMap([]byte(`{
	  "site_admin_groups": ["platform-admins", "sre-oncall"]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		name     string
		claim    GroupsClaim
		wantRole SiteRole
		wantFrom string
	}{
		{"one site-admin group", present("platform-admins"), SiteRoleAdmin, "platform-admins"},
		{"the other one", present("sre-oncall"), SiteRoleAdmin, "sre-oncall"},
		{
			"BOTH: the first in FILE order wins, deterministically",
			present("sre-oncall", "platform-admins"), SiteRoleAdmin, "platform-admins",
		},
		{
			"an ordinary user names no group: 'user' is the ABSENCE of a grant, not a claim of one",
			present("engineering"), SiteRoleUser, "",
		},
		{"no groups at all", present(), SiteRoleUser, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gm.Resolve(tt.claim)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if got.SiteRole != tt.wantRole {
				t.Errorf("SiteRole = %q, want %q", got.SiteRole, tt.wantRole)
			}

			if got.SiteGroup != tt.wantFrom {
				t.Errorf("SiteGroup = %q, want %q. The site-admin listing cannot report "+
					"`ldap: <group>` for an admin whose group it was never told.",
					got.SiteGroup, tt.wantFrom)
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
