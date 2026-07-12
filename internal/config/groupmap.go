package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"

	"github.com/jsmith212/bakery/internal/slug"
)

// The group -> org mapping is JSON, not YAML, and that is a considered choice
// rather than laziness.
//
// The KEYS IN THIS FILE ARE OIDC GROUP NAMES FROM AN EXTERNAL IdP. YAML's implicit
// typing coerces a group literally named `no`, `on`, `off` or `yes` into a BOOLEAN,
// and `1.10` into a float. That is a silent auth-mapping bug in the one file where
// a silent auth-mapping bug is least acceptable, and it costs an unnecessary
// dependency to introduce. JSON's only real loss is comments, which the optional
// per-org `note` field covers.

// OrgRole is a role within an organization. It mirrors the org_role enum in the
// schema; a value outside this set is a config error, not a runtime surprise.
type OrgRole string

const (
	OrgRoleMember OrgRole = "member"
	OrgRoleAdmin  OrgRole = "admin"
	OrgRoleOwner  OrgRole = "owner"
)

// rank orders the roles so that a user in several groups mapping to the same org
// gets the HIGHEST of them rather than whichever the map iterated last.
func (r OrgRole) rank() int {
	switch r {
	case OrgRoleMember:
		return 1
	case OrgRoleAdmin:
		return 2
	case OrgRoleOwner:
		return 3
	}

	return 0
}

func (r OrgRole) valid() bool { return r.rank() > 0 }

// SiteRole is the site-wide role. It mirrors the site_role enum.
type SiteRole string

const (
	SiteRoleUser  SiteRole = "user"
	SiteRoleAdmin SiteRole = "admin"
)

// Errors the caller must branch on.
var (
	// ErrNoGroups means the ID token carried no group claim at all.
	//
	// FAIL CLOSED. Azure AD truncates a >200-group claim into a `_claim_names`
	// overage rather than sending the groups, so "no groups" is a thing that
	// happens to real, correctly-configured users. Reconciliation must REJECT the
	// login, never proceed with an empty keep-set: an empty keep-set makes
	// ReconcileOrgMembershipsRemove delete every org membership the user has, which
	// cascades to their project memberships and every API key they hold in those
	// orgs. That is irreversible -- re-adding the org membership does NOT bring the
	// keys back.
	ErrNoGroups = errors.New("the ID token carried no group claim")

	// ErrNoMappedOrgs means the user's groups map to zero orgs: they are not
	// entitled to anything here. Same fail-closed rule -- reject the login.
	ErrNoMappedOrgs = errors.New("none of the user's groups map to an organization")

	// ErrInvalidGroupMap means the mapping file is malformed. Boot must refuse.
	ErrInvalidGroupMap = errors.New("invalid group mapping file")
)

// GroupMap is the parsed, validated group -> org mapping.
//
// Org roles and the site role are 100% derived from OIDC group claims and
// reconciled on EVERY login. PROJECT roles are managed in-app and this file must
// never mention them: a manually granted org role would be silently deleted by the
// user's next login, which is why the schema has no `source` column and makes that
// state unrepresentable.
type GroupMap struct {
	// Note is the comment field JSON does not have. Ignored.
	Note string `json:"note,omitempty"`

	// SiteAdminGroups: membership in any of these makes the user a site admin.
	SiteAdminGroups []string `json:"site_admin_groups"`

	// Orgs maps each org slug to its group -> role table.
	Orgs []GroupMapOrg `json:"orgs"`
}

// GroupMapOrg is one organization's entry.
type GroupMapOrg struct {
	// Slug is the org slug. It must satisfy the same grammar and reserved-word
	// denylist the database enforces, or the org it names could never be created.
	Slug string `json:"slug"`

	// Note is the comment field JSON does not have. Ignored.
	Note string `json:"note,omitempty"`

	// Groups maps an OIDC group name to the org role it grants.
	Groups map[string]OrgRole `json:"groups"`
}

// Resolution is the authorization state a login reconciles to.
type Resolution struct {
	SiteRole SiteRole

	// Orgs maps org slug -> role. Never empty: Resolve returns an error instead.
	Orgs map[string]OrgRole
}

// LoadGroupMap reads and validates the mapping file at path.
//
// It is deliberately strict -- unknown fields are an error, not a shrug. A typo'd
// key in an authorization file that parses silently and grants nothing is exactly
// the failure this refuses to have.
func LoadGroupMap(path string) (*GroupMap, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read group mapping file: %w", err)
	}

	return ParseGroupMap(raw)
}

// ParseGroupMap validates an in-memory mapping document. LoadGroupMap is this plus
// a file read; tests drive this directly.
func ParseGroupMap(raw []byte) (*GroupMap, error) {
	var gm GroupMap

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&gm); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidGroupMap, err)
	}

	if err := gm.validate(); err != nil {
		return nil, err
	}

	return &gm, nil
}

func (g *GroupMap) validate() error {
	if len(g.Orgs) == 0 {
		return fmt.Errorf("%w: no orgs are mapped, so no user could ever log in", ErrInvalidGroupMap)
	}

	seen := make(map[string]struct{}, len(g.Orgs))

	for i, org := range g.Orgs {
		if err := slug.Check(org.Slug); err != nil {
			return fmt.Errorf("%w: orgs[%d]: %w", ErrInvalidGroupMap, i, err)
		}

		if _, dup := seen[org.Slug]; dup {
			return fmt.Errorf("%w: orgs[%d]: org %q is listed twice; merge the two `groups` tables",
				ErrInvalidGroupMap, i, org.Slug)
		}

		seen[org.Slug] = struct{}{}

		if len(org.Groups) == 0 {
			return fmt.Errorf("%w: orgs[%d] (%s): no groups are mapped, so nobody can reach this org",
				ErrInvalidGroupMap, i, org.Slug)
		}

		for group, role := range org.Groups {
			if group == "" {
				return fmt.Errorf("%w: orgs[%d] (%s): a group name is empty",
					ErrInvalidGroupMap, i, org.Slug)
			}

			if !role.valid() {
				return fmt.Errorf(
					"%w: orgs[%d] (%s): group %q has role %q; want one of member, admin, owner",
					ErrInvalidGroupMap, i, org.Slug, group, role)
			}
		}
	}

	if slices.Contains(g.SiteAdminGroups, "") {
		return fmt.Errorf("%w: site_admin_groups contains an empty group name", ErrInvalidGroupMap)
	}

	return nil
}

// Resolve turns a user's OIDC group claim into the authorization state to
// reconcile.
//
// It FAILS CLOSED, and the caller MUST honor that: an error here means REJECT THE
// LOGIN, never "proceed with no orgs". See ErrNoGroups.
//
// A user in several groups that map to the same org gets the HIGHEST role, not the
// last one the map happened to iterate. A group the file does not mention is
// ignored -- users are routinely in dozens of unrelated IdP groups.
func (g *GroupMap) Resolve(groups []string) (Resolution, error) {
	if len(groups) == 0 {
		return Resolution{SiteRole: "", Orgs: nil}, ErrNoGroups
	}

	claimed := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		claimed[group] = struct{}{}
	}

	orgs := make(map[string]OrgRole)

	for _, org := range g.Orgs {
		for group, role := range org.Groups {
			if _, ok := claimed[group]; !ok {
				continue
			}

			if role.rank() > orgs[org.Slug].rank() {
				orgs[org.Slug] = role
			}
		}
	}

	if len(orgs) == 0 {
		return Resolution{SiteRole: "", Orgs: nil}, ErrNoMappedOrgs
	}

	site := SiteRoleUser

	for _, group := range g.SiteAdminGroups {
		if _, ok := claimed[group]; ok {
			site = SiteRoleAdmin

			break
		}
	}

	return Resolution{SiteRole: site, Orgs: orgs}, nil
}

// OrgSlugs returns every org slug the file mentions, sorted. Boot logs it, so an
// operator can see what the file actually says rather than what they meant.
func (g *GroupMap) OrgSlugs() []string {
	out := make([]string, 0, len(g.Orgs))
	for _, org := range g.Orgs {
		out = append(out, org.Slug)
	}

	sort.Strings(out)

	return out
}
