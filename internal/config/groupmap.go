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
	// ErrGroupsUnreadable means we could not read the user's groups AT ALL: the
	// claim was absent, or Azure AD replaced it with a `_claim_names` /
	// `_claim_sources` overage pointing at Graph.
	//
	// FAIL CLOSED, AND MIND THE DIFFERENCE. "The IdP says this user is in zero
	// groups" and "we could not read this user's groups" are CATEGORICALLY
	// DIFFERENT, and only the first is safe to act on:
	//
	//   - groups: []  -> an ordinary state. It means "this user has only local
	//     memberships". ADMIT them, and reconcile the OIDC half away.
	//   - unreadable  -> we do not know. REFUSE the login and reconcile NOTHING.
	//
	// Treating unreadable as empty NULLs every oidc_role, deletes every org
	// membership row with no local grant, and cascades away the user's project
	// roles and every API key they hold in those orgs. That is irreversible --
	// re-adding the org membership does NOT bring the keys back. And it happens to
	// real, correctly-configured users, not to hypothetical ones.
	//
	// This holds WHETHER OR NOT the login gate is configured: the gate and the
	// reconciler consume the same claim, so disabling the gate must not disable the
	// trap detection. Resolve therefore checks readability BEFORE the gate.
	ErrGroupsUnreadable = errors.New("the user's groups claim is unreadable (absent, or an overage reference)")

	// ErrNotInLoginGroup means login_groups is configured and the user is in none
	// of them: they may not use Bakery at all. Reject the login.
	//
	// This is the LOGIN GATE, and it is a separate question from "which orgs are
	// they in". Answering the second to answer the first is what forced a group per
	// org into the directory, forever. A user in a login group with zero mapped
	// orgs is admitted, and holds only local memberships.
	ErrNotInLoginGroup = errors.New("none of the user's groups is a login group")

	// ErrInvalidGroupMap means the mapping file is malformed. Boot must refuse.
	ErrInvalidGroupMap = errors.New("invalid group mapping file")
)

// GroupMap is the parsed, validated group -> org mapping.
//
// It answers three DIFFERENT questions from three different fields, and keeping
// them apart is the whole point of the file:
//
//   - LoginGroups: may this human use Bakery at all?
//   - SiteAdminGroups: may they run the platform? (the OIDC half of a hybrid role)
//   - Orgs: which orgs are they in, and how? (the OIDC half of a hybrid membership)
//
// The claim-derived roles here are only ever ONE HALF of the effective role: an org
// membership and a site role may ALSO be granted in-app, and the database computes
// the effective role as greatest(oidc, local). The reconciler writes the oidc_*
// columns and nothing else, so nothing in this file can clobber a local grant.
//
// PROJECT roles are in-app only and must never appear here. Group-mapping them is
// the group explosion this design exists to escape.
type GroupMap struct {
	// Note is the comment field JSON does not have. Ignored.
	Note string `json:"note,omitempty"`

	// LoginGroups is the login gate: membership in any one of these admits the user.
	//
	// EMPTY OR UNSET ADMITS ANY SUCCESSFUL OIDC AUTH. That is deliberate, and it is
	// not a fail-open: the IdP has already decided who may authenticate, and a
	// deployment whose IdP tenant IS its user base should not have to restate that
	// as a group. It is also what lets a deployment run with no `orgs` at all and
	// hand out every membership locally.
	LoginGroups []string `json:"login_groups"`

	// SiteAdminGroups: membership in any of these makes the user a site admin.
	SiteAdminGroups []string `json:"site_admin_groups"`

	// Orgs maps each org slug to its group -> role table.
	//
	// MAY BE EMPTY. A deployment can run entirely on local grants, in which case
	// this file exists only to answer the login-gate and site-admin questions -- or
	// need not exist at all.
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

// Resolution is the authorization state a login reconciles to. It is the OIDC HALF
// of the user's authorization, never the whole of it: the local half lives in the
// database and this type cannot see it.
type Resolution struct {
	SiteRole SiteRole

	// SiteGroup is the group that JUSTIFIED SiteRole, empty unless SiteRole is admin.
	//
	// It is written to users.site_oidc_group, and it is audit, not authorization --
	// exactly like OrgGroups below. The site-admin listing has to be able to say
	// `ldap: platform-admins` rather than a bare "the claims said so", because the one
	// real risk in a hybrid site role is a LOCAL grant outliving the LDAP revocation
	// that was meant to remove it, and the defence against that is being able to see
	// which half holds each admin up.
	//
	// Unlike OrgGroups, this is DETERMINISTIC: site_admin_groups is a slice, so the
	// first one the user holds wins, in file order.
	SiteGroup string

	// Orgs maps org slug -> role.
	//
	// LEGITIMATELY EMPTY. It used to be "never empty: Resolve returns an error
	// instead", because org membership was the login gate. It is not any more: a
	// user whose orgs are all granted in-app resolves to zero claim-derived orgs and
	// must be ADMITTED. An empty map means "reconcile the OIDC half to nothing",
	// which is a real instruction, not a missing answer -- Resolve returns
	// ErrGroupsUnreadable when the answer is missing.
	Orgs map[string]OrgRole

	// OrgGroups maps org slug -> the group that JUSTIFIED the winning role in Orgs.
	//
	// It is written to org_memberships.oidc_group and it is audit, not authorization:
	// when a membership survives an LDAP change, or when an admin asks why someone is
	// in an org, "acme-leads" is the answer and "the claims said so" is not. Same key
	// set as Orgs, always.
	OrgGroups map[string]string
}

// GroupsClaim is the `groups` claim as read from the ID token, plus the one bit
// that a plain []string cannot carry: whether we could read it at all.
//
// The two states a bare slice conflates are the trap this whole model fails closed
// around (see ErrGroupsUnreadable), so the distinction is a field, not a nil check
// that any append, copy or round-trip through a mock could quietly erase.
//
// The ZERO VALUE IS "UNREADABLE", so a caller that forgets to set Present refuses
// the login rather than wiping the user's memberships.
type GroupsClaim struct {
	// Groups is the group names the claim carried. Meaningless unless Present.
	Groups []string

	// Present is true iff the ID token actually carried a readable groups claim --
	// including a genuinely empty one. It is FALSE when the claim was absent or
	// replaced by an Azure AD `_claim_names` overage.
	Present bool
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
	// There is deliberately NO `len(g.Orgs) == 0` rule here. It used to say "no orgs
	// are mapped, so no user could ever log in", and that was true when org
	// membership was the login gate. It is not any more: a deployment can run
	// entirely on local grants, and an org-free file is then exactly right.

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

	// An empty string in login_groups would gate on a group nobody can be in, which
	// locks everybody out -- and it is what a trailing comma or a stray "" looks
	// like. An empty LIST is fine and means something else entirely (admit anyone).
	if slices.Contains(g.LoginGroups, "") {
		return fmt.Errorf("%w: login_groups contains an empty group name", ErrInvalidGroupMap)
	}

	return nil
}

// Resolve turns a user's OIDC groups claim into the OIDC half of their
// authorization state.
//
// It FAILS CLOSED, and the caller MUST honor that: an error here means REJECT THE
// LOGIN AND RECONCILE NOTHING -- not one write. It does NOT mean "proceed with no
// orgs", and on an error the returned Resolution is the zero value precisely so
// that a caller which ignored the error reconciles against nothing rather than
// against a plausible-looking empty answer.
//
// It refuses on exactly two things, in this order:
//
//  1. ErrGroupsUnreadable -- we could not read the claim, so we do not know this
//     user's groups. Checked FIRST, and checked whether or not the login gate is
//     configured: the gate and the reconciler consume the same claim, so an
//     unconfigured gate must not disable the trap detection.
//  2. ErrNotInLoginGroup -- login_groups is configured and the user is in none of
//     them.
//
// Resolving to ZERO ORGS IS NOT AN ERROR. It used to be, when org membership was
// the login gate; a locally-granted user legitimately resolves to zero claim-derived
// orgs and refusing them is the bug. Zero orgs means "reconcile the OIDC half away",
// and the row survives whenever a local grant justifies it.
//
// A user in several groups that map to the same org gets the HIGHEST role, not the
// last one the map happened to iterate. A group the file does not mention is
// ignored -- users are routinely in dozens of unrelated IdP groups.
func (g *GroupMap) Resolve(claim GroupsClaim) (Resolution, error) {
	if !claim.Present {
		return Resolution{}, ErrGroupsUnreadable
	}

	claimed := make(map[string]struct{}, len(claim.Groups))
	for _, group := range claim.Groups {
		claimed[group] = struct{}{}
	}

	// The gate. An EMPTY login_groups admits anyone who authenticated.
	if len(g.LoginGroups) > 0 && !anyClaimed(claimed, g.LoginGroups) {
		return Resolution{}, ErrNotInLoginGroup
	}

	orgs := make(map[string]OrgRole)
	groups := make(map[string]string)

	for _, org := range g.Orgs {
		for group, role := range org.Groups {
			if _, ok := claimed[group]; !ok {
				continue
			}

			// STRICTLY greater, so the winner is the highest role -- and, among equal
			// roles, the first the map yields. Map iteration is randomised, so two groups
			// granting the SAME role leave oidc_group non-deterministic. That is honest:
			// either group justifies the membership, and the effective role is identical
			// whichever we record.
			if role.rank() > orgs[org.Slug].rank() {
				orgs[org.Slug] = role
				groups[org.Slug] = group
			}
		}
	}

	site := SiteRoleUser

	// firstClaimed, not anyClaimed: the site-admin listing must be able to NAME the
	// group, so which one won has to be an answer and not merely a boolean.
	siteGroup := firstClaimed(claimed, g.SiteAdminGroups)
	if siteGroup != "" {
		site = SiteRoleAdmin
	}

	return Resolution{SiteRole: site, SiteGroup: siteGroup, Orgs: orgs, OrgGroups: groups}, nil
}

// anyClaimed reports whether the user holds any of want.
func anyClaimed(claimed map[string]struct{}, want []string) bool {
	return firstClaimed(claimed, want) != ""
}

// firstClaimed returns the first of want the user holds, in the order the file lists
// them -- so two site-admin groups do not make the recorded provenance a coin toss.
// An empty group name cannot reach here: validate() refuses one.
func firstClaimed(claimed map[string]struct{}, want []string) string {
	for _, group := range want {
		if _, ok := claimed[group]; ok {
			return group
		}
	}

	return ""
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
