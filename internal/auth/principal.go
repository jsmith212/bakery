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
	// SiteRole is the site-wide role: the EFFECTIVE one, greatest(oidc, local).
	SiteRole = repository.SiteRole
	// OrgRole is a role within an organization: the EFFECTIVE one, greatest(oidc,
	// local). The database generates it; nothing here recomputes it.
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
	// MethodUserToken is a personal access token: a `bkru_` credential that acts as
	// one human, without a browser. Non-interactive, so every capability guard
	// below refuses it until an arm is deliberately opened for it.
	MethodUserToken Method = "user_token"
	// MethodOrgToken is a robot: a `bkro_` credential owned by an ORG, not a human.
	// (The design memo calls this MethodRobot; the constant is named for the token
	// kind so that the Method, the tokenKind and the Prometheus `method` label are
	// all the same word.) Non-interactive.
	MethodOrgToken Method = "org_token"
)

// isInteractive reports whether this principal was obtained by a HUMAN proving
// who they are, right now, through a browser session or a freshly verified OIDC
// token.
//
// It is an ALLOWLIST, and that is the whole point. Every capability guard below
// used to ask `p.method == MethodAPIKey` -- a DENYLIST -- which meant that the
// moment a new Method constant was declared, a principal carrying it inherited
// full human authority, site admin included, until seven separate guards were
// remembered and edited. There is no compiler error for forgetting one and no
// test that fails; the credential simply works too well.
//
// Inverted, a new Method defaults CLOSED: it is refused everywhere until someone
// writes the arm that admits it. TestEveryMethodIsExplicitlyClassified holds the
// line by parsing this file for Method constants and refusing to pass until each
// one is classified.
func (p *principal) isInteractive() bool {
	return p.method == MethodSession || p.method == MethodBearer || p.method == MethodDev
}

// actsAsUser reports whether this principal's orgs/projects maps are the LIVE
// authority of the human it belongs to, and may therefore be consulted.
//
// It is a strict superset of isInteractive: every interactive method acts as its
// user, and so does MethodUserToken -- a personal access token IS its owner, on
// purpose, resolved against the live role tables on every request rather than
// against a snapshot taken at mint time.
//
// It is a SECOND allowlist rather than a `!= MethodUserToken` clause bolted onto
// the first, and that shape is not decoration. The guards below used to be a
// denylist keyed on MethodAPIKey, which meant a new Method inherited full human
// authority by default; writing the opening for user tokens as an exception to
// isInteractive would re-create exactly that, one Method later. Here the switch
// has no default arm and falls through to `false`, so MethodOrgToken -- and every
// Method anyone declares after it -- reads nothing from these maps until somebody
// deliberately adds it to this list and updates
// TestEveryMethodIsExplicitlyClassified's table.
//
// Acting as the user is NOT the same as being the user. IsSiteAdmin stays keyed
// on isInteractive, so a leaked personal access token belonging to a site admin
// is not a master key; CanAdminOrg, CanOwnOrg and CanAdminProject stay there too.
func (p *principal) actsAsUser() bool {
	switch p.method {
	case MethodSession, MethodBearer, MethodDev, MethodUserToken:
		return true
	case MethodAPIKey, MethodOrgToken:
		return false
	}

	return false
}

// interactive is isInteractive for a caller holding the sealed interface.
//
// The type assertion is the fail-closed half: *principal is the only real
// implementation, so anything else -- including a foreign struct that EMBEDS
// Principal to satisfy the interface (see the package doc) -- is not interactive
// and is refused by every caller of this function.
func interactive(p Principal) bool {
	impl, ok := p.(*principal)

	return ok && impl.isInteractive()
}

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

	// Robot reports the robot's grant, if this principal authenticated with an
	// org token. It sits beside APIKey because it answers the same question about
	// a different credential -- WHAT DOES THIS MACHINE CREDENTIAL AUTHORIZE -- and
	// that is authorization data, which is what this interface is for. (A robot
	// has no user row, so UserID/Email/DisplayName are empty for one: this is the
	// only way /me can report what a `bkro_` token actually is.)
	Robot() (RobotGrant, bool)

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

// RobotGrant is what an ORG TOKEN authorizes: one organization, at one scope,
// across every project in it -- present and future.
//
// Note the absent field. There is no ProjectID, and that is the feature: the
// decision never names a project, so a project created after the token was minted
// is covered with zero provisioning. That is the literal "no touchy on anything
// else" the robot exists for, and it is why a robot must never acquire an
// org_memberships row: with no row, CanReadProject's `p.orgs[orgID]` branch is
// unreachable for it no matter what anyone writes later.
type RobotGrant struct {
	RobotID pgtype.UUID
	OrgID   pgtype.UUID
	Scope   Scope
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

	// robot is non-nil if and only if method == MethodOrgToken. A robot principal
	// has NO fields above it populated -- no user id, no email, no site role, no
	// orgs map, no projects map -- because there are none to populate: an org token
	// belongs to an organization, not to a human.
	robot *RobotGrant

	// maxScope is the CEILING a live-role credential carries, and it is only ever
	// consulted for a principal that actsAsUser but is not interactive -- i.e. a
	// personal access token. Interactive principals are constructed with ScopeWrite
	// (no ceiling): a human at a console is already bounded by their roles, and a
	// zero value here would silently make every session read-only.
	maxScope Scope
}

func (p *principal) sealed() {}

func (p *principal) UserID() pgtype.UUID { return p.userID }
func (p *principal) Issuer() string      { return p.issuer }
func (p *principal) Subject() string     { return p.subject }
func (p *principal) Email() string       { return p.email }
func (p *principal) DisplayName() string { return p.displayName }
func (p *principal) Method() Method      { return p.method }
func (p *principal) SiteRole() SiteRole  { return p.siteRole }

// IsSiteAdmin is false for every non-interactive credential -- an API key, a
// personal access token, a robot -- even when the owning user IS a site admin.
//
// A machine credential is a delegation, capped to far less than the human who
// minted it. If it carried site admin with it, a read-scoped key minted for one
// project would silently be a master key for the whole installation -- and the
// schema deliberately cannot notice, because validation does not join anything
// (see api_keys.sql). The cap is applied here, at construction, and again on
// every question below.
func (p *principal) IsSiteAdmin() bool {
	return p.isInteractive() && p.siteRole == SiteRoleAdmin
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

func (p *principal) Robot() (RobotGrant, bool) {
	if p.robot == nil {
		return RobotGrant{}, false
	}

	return *p.robot, true
}

// CanViewOrg reports whether the principal may see the org at all.
//
// A personal access token passes this: it acts as its owner, and `bakery org
// list` working with a PAT is the whole point of the credential. A robot and an
// API key do not -- neither has an orgs map, and a machine credential that could
// enumerate a tenant is a reconnaissance primitive for no benefit.
func (p *principal) CanViewOrg(orgID pgtype.UUID) bool {
	if !p.actsAsUser() {
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
	if !p.isInteractive() {
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
	if !p.isInteractive() {
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
	// A credential that does not act as its user is answered by its GRANT and by
	// nothing else. A principal with no grant at all (key == nil, robot == nil) is
	// refused -- which is what a newly declared Method gets until an arm is
	// deliberately written for it.
	if !p.actsAsUser() {
		return p.grants(orgID, projectID, ScopeRead)
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
	// A grant-only credential is answered by its grant.
	if !p.actsAsUser() {
		return p.grants(orgID, projectID, ScopeWrite)
	}

	if p.isInteractive() {
		// THE ORG-ADMIN SHORT-CIRCUIT IS FOR HUMANS ONLY, and this branch is what
		// makes that structural rather than a second check someone can drop.
		//
		// An org admin who mints a personal access token gets write exactly where they
		// hold an explicit project role, and nowhere else -- the same narrowing that
		// has always applied to an API key. It is a real consequence and the mint-time
		// reveal copy says so: an org owner with no project role cannot push with a
		// PAT. Widening it would mean the commonest credential in the installation
		// silently carried org-wide write.
		if p.CanAdminOrg(orgID) {
			return true
		}
	} else if p.maxScope != ScopeWrite {
		// THE CEILING, and it is read ONLY here -- for a credential that acts as a
		// user without being one, i.e. a personal access token. A token minted `read`
		// can never write, however senior its holder is or becomes.
		//
		// Keeping it out of the interactive path is not tidiness: maxScope's zero
		// value is the empty string, so a principal built without it would be silently
		// read-only, and "the console stopped being able to push" is a much quieter
		// failure than it deserves to be.
		return false
	}

	role, ok := p.projects[projectID]

	return ok && (role == ProjectRoleWriter || role == ProjectRoleAdmin)
}

// CanAdminProject reports whether the principal may manage the project's
// settings, memberships and API keys.
func (p *principal) CanAdminProject(orgID, projectID pgtype.UUID) bool {
	if !p.isInteractive() {
		return false
	}

	if p.CanAdminOrg(orgID) {
		return true
	}

	role, ok := p.projects[projectID]

	return ok && role == ProjectRoleAdmin
}

// grants is the whole authorization story for a credential that does NOT act as
// a user: an API key or a robot. It dispatches on which grant the principal
// actually carries, never on its Method -- so a principal carrying neither (the
// state a newly declared Method starts in) is refused by falling off the end.
func (p *principal) grants(orgID, projectID pgtype.UUID, want Scope) bool {
	if p.robot != nil {
		return p.robotGrants(orgID, want)
	}

	return p.keyGrants(projectID, want)
}

// robotGrants: the routed ORG must be the robot's org, and the scope must cover
// the operation. No project is named, deliberately -- see RobotGrant.
func (p *principal) robotGrants(orgID pgtype.UUID, want Scope) bool {
	if p.robot == nil || p.robot.OrgID != orgID {
		return false
	}

	if want == ScopeWrite {
		return p.robot.Scope == ScopeWrite
	}

	return true
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
