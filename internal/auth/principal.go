// Package auth owns identity and authorization: OIDC verification, browser
// sessions, the CLI device grant, API keys, and the Principal that every
// authorization decision in Bakery is made against.
//
// # The Principal invariant
//
// A Principal is a VERIFIED identity. Nothing outside this package may create
// one, and that is enforced by the compiler rather than by a comment:
//
//   - Principal is an INTERFACE, so `auth.Principal{}` is not a composite
//     literal any more -- it does not compile.
//   - It carries an unexported method (sealed), so no type declared in another
//     package can implement it.
//   - The only implementation, *principal, is unexported, and every function
//     that returns one is unexported. A caller outside this package obtains a
//     Principal from exactly two places: the request context (put there by this
//     package's middleware, after verification) and this package's own
//     authenticate methods.
//
// The one residual hole in an unexported-method interface is EMBEDDING: a
// foreign struct may embed auth.Principal and thereby satisfy it. That gains an
// attacker nothing. The embedded field is either nil -- in which case every
// method call panics, which is loud and fail-closed, never a silent "some valid
// user" -- or it holds a Principal this package already issued, which is not a
// forgery. There is no arrangement of exported identifiers that yields a
// Principal with roles this package did not verify. TestPrincipalIsUnforgeable
// drives the compiler at this and asserts it refuses.
//
// This matters beyond tidiness. Later milestones bet on it: the OCI upstream
// fetch takes a Principal, and if one could be forged from outside, Bakery would
// be an open relay serving Docker Hub with our rate-limit-bearing credentials.
package auth

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// The role vocabulary is ALIASED to the generated enums rather than redeclared.
// A redeclaration would need a conversion at every DB boundary, and a conversion
// is a place where "reader" can silently become "" -- an authorization downgrade
// or, worse, a comparison that quietly stops matching. An alias cannot drift from
// the schema because it IS the schema's type.
type (
	// SiteRole is the site-wide role, derived from OIDC group claims.
	SiteRole = repository.SiteRole
	// OrgRole is a role within an organization, derived from OIDC group claims.
	OrgRole = repository.OrgRole
	// ProjectRole is a role within a project. Managed in-app, never from claims.
	ProjectRole = repository.ProjectRole
	// Scope is an API key's read/write scope.
	Scope = repository.ApiKeyScope
)

// Role and scope constants, re-exported so callers need not import the generated
// package to make an authorization decision.
const (
	SiteRoleUser  = repository.SiteRoleUser
	SiteRoleAdmin = repository.SiteRoleAdmin

	OrgRoleMember = repository.OrgRoleMember
	OrgRoleAdmin  = repository.OrgRoleAdmin
	OrgRoleOwner  = repository.OrgRoleOwner

	ProjectRoleReader = repository.ProjectRoleReader
	ProjectRoleWriter = repository.ProjectRoleWriter
	ProjectRoleAdmin  = repository.ProjectRoleAdmin

	ScopeRead  = repository.ApiKeyScopeRead
	ScopeWrite = repository.ApiKeyScopeWrite
)

// Method is how the caller proved who they are.
type Method string

const (
	// MethodSession is the browser: an scs session cookie, established by the
	// OIDC authorization-code flow.
	MethodSession Method = "session"
	// MethodBearer is the CLI: an OIDC ID token from the device grant, verified
	// against the provider's JWKS on every request.
	MethodBearer Method = "bearer"
	// MethodAPIKey is machine traffic: a project-scoped `bkry_` key. This is the
	// only method that appears on the cache hot path.
	MethodAPIKey Method = "api_key"
	// MethodDev is DEV_LOGIN. It exists only when the env var / flag is set, and
	// there is no code path that can turn it on at runtime.
	MethodDev Method = "dev"
)

// Principal is a verified identity plus the roles it was verified to hold.
//
// It cannot be constructed outside this package. See the package doc for why
// that is load-bearing and how the compiler enforces it.
//
// The capability methods take BOTH the org id and the project id because the
// caller (route resolution) already has both, and a Principal that had to look up
// a project's org would need a database on the authorization path.
type Principal interface {
	// Identity.
	UserID() pgtype.UUID
	Issuer() string
	Subject() string
	Email() string
	DisplayName() string
	Method() Method

	// Roles. For an API-key principal these are deliberately empty: a key is a
	// DELEGATION of the user's authority, capped to one project and one scope,
	// and it must never carry the user's site or org powers with it.
	SiteRole() SiteRole
	IsSiteAdmin() bool
	OrgRole(orgID pgtype.UUID) (OrgRole, bool)
	ProjectRole(projectID pgtype.UUID) (ProjectRole, bool)

	// APIKey reports the key's grant, if this principal authenticated with one.
	APIKey() (KeyGrant, bool)

	// Capabilities. These are the only questions callers should ask.
	CanViewOrg(orgID pgtype.UUID) bool
	CanAdminOrg(orgID pgtype.UUID) bool
	CanOwnOrg(orgID pgtype.UUID) bool
	CanReadProject(orgID, projectID pgtype.UUID) bool
	CanWriteProject(orgID, projectID pgtype.UUID) bool
	CanAdminProject(orgID, projectID pgtype.UUID) bool

	// sealed is unexported, so no type outside this package can implement
	// Principal. This method is the enforcement mechanism; do not remove it, and
	// do not give it an exported name.
	sealed()
}

// KeyGrant is what an API key authorizes: exactly one project, at one scope.
type KeyGrant struct {
	KeyID     pgtype.UUID
	ProjectID pgtype.UUID
	Scope     Scope
}

// principal is the one and only implementation of Principal.
type principal struct {
	userID      pgtype.UUID
	issuer      string
	subject     string
	email       string
	displayName string
	method      Method

	siteRole SiteRole

	// pgtype.UUID is [16]byte plus a bool: comparable, so it is a legal map key
	// and no string formatting happens on an authorization decision.
	orgs     map[pgtype.UUID]OrgRole
	projects map[pgtype.UUID]ProjectRole

	// key is non-nil if and only if method == MethodAPIKey.
	key *KeyGrant
}

func (p *principal) sealed() {}

func (p *principal) UserID() pgtype.UUID { return p.userID }
func (p *principal) Issuer() string      { return p.issuer }
func (p *principal) Subject() string     { return p.subject }
func (p *principal) Email() string       { return p.email }
func (p *principal) DisplayName() string { return p.displayName }
func (p *principal) Method() Method      { return p.method }
func (p *principal) SiteRole() SiteRole  { return p.siteRole }

// IsSiteAdmin is false for an API key even when the owning user IS a site admin.
//
// A key is a delegation capped to one project and one scope. If it carried site
// admin with it, a read-scoped key minted for one project would silently be a
// master key for the whole installation -- and the schema deliberately cannot
// notice, because validation does not join anything (see api_keys.sql). The cap
// is applied here, at construction, and again on every question below.
func (p *principal) IsSiteAdmin() bool {
	return p.method != MethodAPIKey && p.siteRole == SiteRoleAdmin
}

func (p *principal) OrgRole(orgID pgtype.UUID) (OrgRole, bool) {
	role, ok := p.orgs[orgID]

	return role, ok
}

func (p *principal) ProjectRole(projectID pgtype.UUID) (ProjectRole, bool) {
	role, ok := p.projects[projectID]

	return role, ok
}

func (p *principal) APIKey() (KeyGrant, bool) {
	if p.key == nil {
		return KeyGrant{}, false
	}

	return *p.key, true
}

// CanViewOrg reports whether the principal may see the org at all.
func (p *principal) CanViewOrg(orgID pgtype.UUID) bool {
	if p.method == MethodAPIKey {
		return false
	}

	if p.IsSiteAdmin() {
		return true
	}

	_, ok := p.orgs[orgID]

	return ok
}

// CanAdminOrg reports whether the principal may administer the org: create and
// delete its projects, manage its project memberships.
func (p *principal) CanAdminOrg(orgID pgtype.UUID) bool {
	if p.method == MethodAPIKey {
		return false
	}

	if p.IsSiteAdmin() {
		return true
	}

	role, ok := p.orgs[orgID]

	return ok && (role == OrgRoleAdmin || role == OrgRoleOwner)
}

// CanOwnOrg reports whether the principal may perform owner-only acts: renaming
// or deleting the organization itself.
func (p *principal) CanOwnOrg(orgID pgtype.UUID) bool {
	if p.method == MethodAPIKey {
		return false
	}

	if p.IsSiteAdmin() {
		return true
	}

	role, ok := p.orgs[orgID]

	return ok && role == OrgRoleOwner
}

// CanReadProject reports whether the principal may read the project's cache and
// its console pages.
func (p *principal) CanReadProject(orgID, projectID pgtype.UUID) bool {
	if p.method == MethodAPIKey {
		return p.keyGrants(projectID, ScopeRead)
	}

	if p.IsSiteAdmin() {
		return true
	}

	// Any org membership implies read on every project in the org. Org roles are
	// claim-derived, so this is exactly "the IdP put you in this org's group".
	if _, ok := p.orgs[orgID]; ok {
		return true
	}

	_, ok := p.projects[projectID]

	return ok
}

// CanWriteProject reports whether the principal may write to the project's cache.
func (p *principal) CanWriteProject(orgID, projectID pgtype.UUID) bool {
	if p.method == MethodAPIKey {
		return p.keyGrants(projectID, ScopeWrite)
	}

	if p.CanAdminOrg(orgID) {
		return true
	}

	role, ok := p.projects[projectID]

	return ok && (role == ProjectRoleWriter || role == ProjectRoleAdmin)
}

// CanAdminProject reports whether the principal may manage the project's
// settings, memberships and API keys.
func (p *principal) CanAdminProject(orgID, projectID pgtype.UUID) bool {
	if p.method == MethodAPIKey {
		return false
	}

	if p.CanAdminOrg(orgID) {
		return true
	}

	role, ok := p.projects[projectID]

	return ok && role == ProjectRoleAdmin
}

// keyGrants is the whole authorization story for an API key: the routed project
// must be the key's project, and the scope must cover the operation. There is no
// escalation path -- not through the site role, not through org membership.
func (p *principal) keyGrants(projectID pgtype.UUID, want Scope) bool {
	if p.key == nil || p.key.ProjectID != projectID {
		return false
	}

	if want == ScopeWrite {
		return p.key.Scope == ScopeWrite
	}

	return true
}

// MaxScopeForRole caps an API key's scope at the authority of the role granting
// it. A key must never exceed the user's own authority in the project, and
// validation deliberately does not join project_memberships to check (that would
// put a second probe on the sstate HEAD storm) -- so the cap is applied here, at
// creation, and re-applied by RevokeAPIKeysForMembership on any role downgrade.
func MaxScopeForRole(role ProjectRole) Scope {
	if role == ProjectRoleWriter || role == ProjectRoleAdmin {
		return ScopeWrite
	}

	return ScopeRead
}

// ScopeWithinRole reports whether a key at scope `want` is within the authority
// of `role`.
func ScopeWithinRole(want Scope, role ProjectRole) bool {
	return want == ScopeRead || MaxScopeForRole(role) == ScopeWrite
}
