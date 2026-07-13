package api

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// The wire types.
//
// Field names are snake_case, ids are canonical UUID strings, and every timestamp
// is RFC 3339 or JSON null -- never a formatted, humanised string. The console
// renders "2 min ago" and "760 GB"; the API does not, because the moment it does,
// a second consumer (the CLI) has to parse English back into a duration.
//
// Lists are always an OBJECT with an `items` array, never a bare top-level array:
// a bare array cannot grow a pagination cursor without a breaking change, and it
// is the classic JSON-hijacking shape.

// ListResponse is the envelope every collection uses.
type ListResponse[T any] struct {
	Items []T `json:"items"`
}

// list builds a ListResponse whose Items is never nil -- `[]`, not `null`, so the
// console can iterate without a guard.
func list[T any](items []T) ListResponse[T] {
	if items == nil {
		items = []T{}
	}

	return ListResponse[T]{Items: items}
}

// Org is an organization.
type Org struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`

	// Role is the CALLER's role in this org: member|admin|owner, or "" for a site
	// admin who is not a member. The console needs it to decide which buttons to
	// render, and computing it here saves the SPA from re-deriving authorization.
	Role string `json:"role,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newOrg(o repository.Organization, p Principal) Org {
	role := ""
	if r, ok := p.OrgRole(o.ID); ok {
		role = string(r)
	}

	return Org{
		ID: uuidString(o.ID), Slug: o.Slug, Name: o.Name, Role: role,
		CreatedAt: o.CreatedAt.Time, UpdatedAt: o.UpdatedAt.Time,
	}
}

// Project is a project within an org.
type Project struct {
	ID      string `json:"id"`
	OrgID   string `json:"org_id"`
	OrgSlug string `json:"org_slug"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`

	// Role is the caller's PROJECT role: reader|writer|admin, or "" when their
	// access comes from an org role rather than a project membership.
	Role string `json:"role,omitempty"`

	// Backends is the kinds configured on this project (sstate, downloads, ...),
	// which is what the projects screen lists per row.
	Backends []string `json:"backends"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newProject(pr repository.Project, orgSlug string, backends []string, p Principal) Project {
	role := ""
	if r, ok := p.ProjectRole(pr.ID); ok {
		role = string(r)
	}

	if backends == nil {
		backends = []string{}
	}

	return Project{
		ID: uuidString(pr.ID), OrgID: uuidString(pr.OrgID), OrgSlug: orgSlug,
		Slug: pr.Slug, Name: pr.Name, Role: role, Backends: backends,
		CreatedAt: pr.CreatedAt.Time, UpdatedAt: pr.UpdatedAt.Time,
	}
}

// Member is a person's roles, WITH THE PROVENANCE OF THE ORG ROLE.
//
// One type serves both membership lists. On the org list ProjectRole is absent; on
// a project's list it is present and EMPTY for an org member who holds no role in
// that project -- which is exactly the "org role: admin / project role: none" row
// the members screen renders. Returning the org's full roster from the project
// endpoint is deliberate: the screen needs to offer those people a project role,
// and a list that omitted them could only offer a free-text box.
//
// # Why provenance is on the wire and not merely in the database
//
// Org membership has TWO sources: an OIDC half the reconciler owns and a local half
// this API owns. OrgRole is the EFFECTIVE role, greatest(oidc, local), computed by
// the database. Reporting only that would make a local grant that outlives an LDAP
// revocation invisible -- which is a backdoor, not a UI simplification. So the
// console can always answer "why is this person an admin?", and "if I remove the
// local grant, are they still in?".
type Member struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`

	// OrgRole is the EFFECTIVE role: greatest(oidc_role, local_role). Never written
	// directly -- the database generates it.
	OrgRole string `json:"org_role"`

	// OIDCRole and OIDCGroup are the claim half: the role the IdP's groups confer,
	// and which group conferred it. Empty when no claim justifies this membership.
	// Read-only over this API -- change the group in the IdP or the group map.
	OIDCRole  string `json:"oidc_role,omitempty"`
	OIDCGroup string `json:"oidc_group,omitempty"`

	// LocalRole is the in-app half, granted by an org admin through PUT. Empty when
	// there is no local grant. GrantedBy/GrantedByEmail/GrantedAt are its audit
	// trail and are present exactly when LocalRole is.
	LocalRole      string     `json:"local_role,omitempty"`
	GrantedBy      string     `json:"granted_by,omitempty"`
	GrantedByEmail string     `json:"granted_by_email,omitempty"`
	GrantedAt      *time.Time `json:"granted_at"`

	// ProjectRole is managed in-app: reader|writer|admin, or "" for "no role in
	// this project". Only present on a project's member list.
	ProjectRole string `json:"project_role,omitempty"`

	// Source summarises the two halves above for a console that only wants a badge:
	// oidc_groups | local | oidc_groups+local. Absent on a response that carries no
	// org role at all (the project-role PUT), rather than claiming a source for a
	// role that is not there.
	Source string `json:"org_role_source,omitempty"`
}

// The values Member.Source takes. `oidc_groups+local` is not a curiosity: it is the
// state in which a DELETE clears the local grant and the user REMAINS A MEMBER, and
// an admin who cannot see that state coming is an admin who will be surprised by it.
const (
	OrgRoleSourceOIDC  = "oidc_groups"
	OrgRoleSourceLocal = "local"
	OrgRoleSourceBoth  = "oidc_groups+local"
)

// Site-admin provenance uses the SAME vocabulary, deliberately with the same values:
// both roles are hybrid in exactly the same way, and a console that renders one
// source badge should not have to learn two spellings of it.
const (
	SiteRoleSourceOIDC  = OrgRoleSourceOIDC
	SiteRoleSourceLocal = OrgRoleSourceLocal
	SiteRoleSourceBoth  = OrgRoleSourceBoth
)

// roleSource summarises which halves justify a hybrid role -- an org membership or a
// site role, which are the same shape.
func roleSource(oidc, local bool) string {
	switch {
	case oidc && local:
		return OrgRoleSourceBoth
	case local:
		return OrgRoleSourceLocal
	default:
		// Includes the neither-half case, which neither caller can reach: an org
		// membership row with no source cannot exist (the generated `role` would be
		// NULL and the column is NOT NULL), and the site-admin listing selects on
		// site_role = 'admin', which is greatest(oidc, local) and so requires one of
		// them to BE 'admin'. There is nothing honest to say but "the claims put them
		// here".
		return OrgRoleSourceOIDC
	}
}

// SiteAdmin is one site administrator, WITH THE SOURCE OF THE ROLE. It is the whole
// reason a hybrid site role is safe to offer.
//
// A site admin may be claim-derived (an OIDC group), locally granted (by another site
// admin, or by the CLI break-glass), or BOTH. The failure this type exists to prevent
// is a local grant that outlives the LDAP revocation that was supposed to remove
// it: the directory says the person is no longer a platform admin, and Bakery, on a
// grant nobody remembers making, says they still are.
//
// That state is not preventable -- it is inherent in having two sources -- so it is
// made VISIBLE instead. Every row says which half holds the role up: `ldap:
// platform-admins`, or `local: granted by jsmith on 2026-07-12`. A backdoor you can
// see on a screen is not much of a backdoor, and this listing is that screen. It is
// the mitigation, not a nice-to-have.
type SiteAdmin struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`

	// SiteRole is the EFFECTIVE role: coalesce(greatest(oidc, local), 'user'), computed
	// by the database. Always "admin" on this listing.
	SiteRole string `json:"site_role"`

	// OIDCRole and OIDCGroup are the claim half: `admin` and the group that conferred
	// it. Empty when no group claim makes this user a site admin. Read-only over this
	// API -- change the group in the IdP or in site_admin_groups.
	OIDCRole  string `json:"site_role_oidc,omitempty"`
	OIDCGroup string `json:"site_oidc_group,omitempty"`

	// LocalRole is the in-app half. GrantedBy/GrantedByEmail/GrantedAt are its audit
	// trail; granted_by is EMPTY for a grant made by the CLI break-glass, which has no
	// session and therefore no granter to name. That emptiness is a finding, not a
	// gap: it says someone with database access made this grant.
	LocalRole      string     `json:"site_role_local,omitempty"`
	GrantedBy      string     `json:"granted_by,omitempty"`
	GrantedByEmail string     `json:"granted_by_email,omitempty"`
	GrantedAt      *time.Time `json:"granted_at"`

	// Source is oidc_groups | local | oidc_groups+local. `oidc_groups+local` is the
	// state in which a DELETE clears the local grant and the user REMAINS a site
	// admin.
	Source string `json:"site_role_source"`
}

// newSiteAdmin renders one row of the site-admin listing.
func newSiteAdmin(a repository.ListSiteAdminsRow) SiteAdmin {
	return SiteAdmin{
		UserID: uuidString(a.ID), Email: a.Email, DisplayName: a.DisplayName,
		SiteRole:  string(a.SiteRole),
		OIDCRole:  siteRoleString(a.SiteRoleOidc),
		OIDCGroup: a.SiteOidcGroup.String,
		LocalRole: siteRoleString(a.SiteRoleLocal),
		GrantedBy: uuidString(a.SiteGrantedBy), GrantedByEmail: a.GrantedByEmail.String,
		GrantedAt: timePtr(a.SiteGrantedAt),
		Source:    roleSource(a.SiteRoleOidc.Valid, a.SiteRoleLocal.Valid),
	}
}

// newSiteAdminFromUser renders a users row the API just wrote. It is the same shape,
// from the whole-row RETURNING of the grant and revoke queries. granted_by_email is
// supplied by the caller, which resolved the granter before writing.
func newSiteAdminFromUser(u repository.User, grantedByEmail string) SiteAdmin {
	return SiteAdmin{
		UserID: uuidString(u.ID), Email: u.Email, DisplayName: u.DisplayName,
		SiteRole:  string(u.SiteRole),
		OIDCRole:  siteRoleString(u.SiteRoleOidc),
		OIDCGroup: u.SiteOidcGroup.String,
		LocalRole: siteRoleString(u.SiteRoleLocal),
		GrantedBy: uuidString(u.SiteGrantedBy), GrantedByEmail: grantedByEmail,
		GrantedAt: timePtr(u.SiteGrantedAt),
		Source:    roleSource(u.SiteRoleOidc.Valid, u.SiteRoleLocal.Valid),
	}
}

// SiteAdminRemoval is the response to DELETE /site-admins/{user}, and it exists for
// the same reason OrgMemberRemoval does: a bare 204 would be a LIE half the time.
//
// This endpoint owns only the LOCAL half of a site role. Clearing it demotes the user
// only if no OIDC group claim also makes them an admin; if one does, they are STILL A
// SITE ADMIN -- with every privilege in the installation -- and an operator who saw a
// success and concluded otherwise has a security incident, not a UI annoyance.
type SiteAdminRemoval struct {
	UserID string `json:"user_id"`

	// LocalRoleRevoked is true when a local grant was actually cleared.
	LocalRoleRevoked bool `json:"local_role_revoked"`

	// StillASiteAdmin is TRUE when the user REMAINS a site admin because an OIDC group
	// claim makes them one, independently of the grant just removed.
	StillASiteAdmin bool `json:"still_a_site_admin"`

	// Admin is the surviving site admin, present exactly when StillASiteAdmin. It
	// names the group that is holding the role up.
	Admin *SiteAdmin `json:"admin,omitempty"`

	// Message is prose for a human, shown verbatim.
	Message string `json:"message"`
}

// siteRoleString renders a nullable site role, with NULL as "" -- never as the zero
// SiteRole, which is an empty string that LOOKS like a role.
func siteRoleString(r repository.NullSiteRole) string {
	if !r.Valid {
		return ""
	}

	return string(r.SiteRole)
}

// newOrgMember renders one row of the org roster.
func newOrgMember(m repository.ListOrgMembersRow) Member {
	return Member{
		UserID: uuidString(m.UserID), Email: m.Email, DisplayName: m.DisplayName,
		OrgRole:   string(m.Role),
		OIDCRole:  orgRoleString(m.OidcRole),
		OIDCGroup: m.OidcGroup.String,
		LocalRole: orgRoleString(m.LocalRole),
		GrantedBy: uuidString(m.GrantedBy), GrantedByEmail: m.GrantedByEmail.String,
		GrantedAt:   timePtr(m.GrantedAt),
		ProjectRole: "",
		Source:      roleSource(m.OidcRole.Valid, m.LocalRole.Valid),
	}
}

// newMembership renders an org_memberships row the API just wrote. The user's email
// and display name are not on that row, so the caller supplies them -- it resolved
// the user before writing, so it has them.
func newMembership(m repository.OrgMembership, email, displayName, grantedByEmail string) Member {
	return Member{
		UserID: uuidString(m.UserID), Email: email, DisplayName: displayName,
		OrgRole:   string(m.Role),
		OIDCRole:  orgRoleString(m.OidcRole),
		OIDCGroup: m.OidcGroup.String,
		LocalRole: orgRoleString(m.LocalRole),
		GrantedBy: uuidString(m.GrantedBy), GrantedByEmail: grantedByEmail,
		GrantedAt:   timePtr(m.GrantedAt),
		ProjectRole: "",
		Source:      roleSource(m.OidcRole.Valid, m.LocalRole.Valid),
	}
}

// orgRoleString renders a nullable org role as a string, with NULL as "" -- never
// as the zero OrgRole, which would be an empty string that LOOKS like a role.
func orgRoleString(r repository.NullOrgRole) string {
	if !r.Valid {
		return ""
	}

	return string(r.OrgRole)
}

// OrgMemberRemoval is the response to DELETE /orgs/{org}/members/{user}, and it
// exists because a bare 204 would be a LIE half the time.
//
// The API owns only the local half of a membership. Clearing it removes the user
// from the org only if no OIDC claim also justifies them; if one does, the row
// survives and they are still a member, still hold their project roles, and still
// hold every API key they have minted. An admin who removes someone, sees a
// success, and reasonably concludes they are gone -- when they are not -- is a
// security incident waiting to happen. So the response says which of the two
// happened, and when the membership survives it says what is holding it up.
type OrgMemberRemoval struct {
	UserID string `json:"user_id"`

	// LocalRoleRevoked is true when a local grant was actually cleared.
	LocalRoleRevoked bool `json:"local_role_revoked"`

	// StillAMember is TRUE when the user REMAINS a member of the org because an
	// OIDC claim justifies them independently of the grant just removed.
	StillAMember bool `json:"still_a_member"`

	// Membership is the surviving membership, present exactly when StillAMember.
	// It carries the group that is holding it up.
	Membership *Member `json:"membership,omitempty"`

	// Message is prose for a human, and the console shows it verbatim. The codes
	// above are what a client branches on.
	Message string `json:"message"`
}

// APIKey is a key's METADATA. There is no Token field and no Hash field: this is
// the type returned by every endpoint except create, and it is structurally
// incapable of carrying the secret.
type APIKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`

	// TokenPrefix is `bkry_` plus the first 8 characters of the random part -- a
	// greppable, non-secret handle so the console can tell keys apart after the
	// one-time reveal.
	TokenPrefix string `json:"token_prefix"`

	Scope      string     `json:"scope"` // read|write
	OwnerID    string     `json:"owner_id"`
	OwnerEmail string     `json:"owner_email"`
	OwnerName  string     `json:"owner_name"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

// CreatedAPIKey is the ONE response in the whole API that carries a secret.
//
// It is a distinct type from APIKey rather than an APIKey with an optional Token,
// and that is not style. An `omitempty` Token on the shared type would mean every
// list endpoint is one forgotten field-clear away from leaking every key it
// returns. Here, the type returned by list literally has no field to put a token
// in, so the leak is not a bug that can be introduced -- it is a compile error.
type CreatedAPIKey struct {
	APIKey

	// Token is the plaintext `bkry_...`. It exists in this response and NOWHERE
	// else, ever: the schema stores only its SHA-256, so there is no query, no
	// admin path and no database dump that can recover it. If the user loses it,
	// they mint a new one.
	Token string `json:"token"`
}

func newAPIKey(row repository.ListAPIKeysForProjectRow, ownerEmail, ownerName string) APIKey {
	return APIKey{
		ID: uuidString(row.ID), Name: row.Name, ProjectID: uuidString(row.ProjectID),
		TokenPrefix: row.TokenPrefix, Scope: string(row.Scope),
		OwnerID: uuidString(row.UserID), OwnerEmail: ownerEmail, OwnerName: ownerName,
		CreatedAt: row.CreatedAt.Time,
		ExpiresAt: timePtr(row.ExpiresAt), LastUsedAt: timePtr(row.LastUsedAt),
		RevokedAt: timePtr(row.RevokedAt),
	}
}

// Backend is a cache-backend config row. M1 configures them; no backend serves
// traffic until M2.
type Backend struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`

	// Kind is sstate|downloads|hashserv|bazel|oci.
	Kind string `json:"kind"`

	Enabled bool `json:"enabled"`

	// ReadAuthRequired, and deliberately no WriteAuthRequired: reads may be opened
	// up per backend, but WRITES ALWAYS REQUIRE A KEY. "Unauthenticated writes" is
	// a cache-poisoning vector and is not a state the schema can represent, so it
	// is not a field the API can offer.
	ReadAuthRequired bool `json:"read_auth_required"`

	Config json.RawMessage `json:"config"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func newBackend(b repository.CacheBackend) Backend {
	cfg := json.RawMessage(b.Config)
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}

	return Backend{
		ID: b.ID, ProjectID: uuidString(b.ProjectID), Kind: string(b.Kind),
		Enabled: b.Enabled, ReadAuthRequired: b.ReadAuthRequired, Config: cfg,
		CreatedAt: b.CreatedAt.Time, UpdatedAt: b.UpdatedAt.Time,
	}
}

// Me is the current principal: who you are and everything you may do.
//
// The console fetches this once at boot and drives its whole navigation from it --
// which orgs exist for you, which projects, and whether to render an admin
// control. Note that it is a REPORT of authorization, not the enforcement of it:
// every endpoint re-checks. A console that hid a button would still be talking to
// a server that refuses the call.
type Me struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`

	// Method is session|bearer|api_key|dev: how this request proved who it is.
	Method string `json:"method"`

	SiteRole    string `json:"site_role"` // user|admin
	IsSiteAdmin bool   `json:"is_site_admin"`

	Orgs     []MeOrg     `json:"orgs"`
	Projects []MeProject `json:"projects"`

	// APIKey is present only when this request authenticated WITH a key, and it
	// describes that key's grant. A key principal never carries the owner's site or
	// org roles, so SiteRole reads "user" and Orgs is empty even when the owning
	// human is a site admin -- a delegation must not become a master key.
	APIKey *MeKeyGrant `json:"api_key,omitempty"`
}

// MeOrg is one of the caller's org memberships.
type MeOrg struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	Role string `json:"role"` // member|admin|owner
}

// MeProject is one of the caller's project memberships.
type MeProject struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	OrgSlug string `json:"org_slug"`
	Role    string `json:"role"` // reader|writer|admin
}

// MeKeyGrant is what an API key authorizes: one project, one scope.
type MeKeyGrant struct {
	KeyID     string `json:"key_id"`
	ProjectID string `json:"project_id"`
	Scope     string `json:"scope"`
}

// uuidString renders a pgtype.UUID as canonical 8-4-4-4-12 hex.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}

	s, err := u.Value()
	if err != nil {
		return ""
	}

	str, ok := s.(string)
	if !ok {
		return ""
	}

	return str
}

// parseUUID is the inverse. It is used for the {user} and {key} path segments,
// which are the only ids a caller ever supplies -- and both are re-checked against
// the authorized scope before anything is done with them.
func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, errBadRequest("that is not a valid id", err)
	}

	return u, nil
}

// timePtr turns a nullable timestamp into a *time.Time, so the JSON is `null`
// rather than Go's zero time -- "0001-01-01T00:00:00Z" is not "never", it is a
// date, and a console that formats it will happily print it.
func timePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}

	t := ts.Time

	return &t
}

// scopeOf parses a scope string from a request body.
func scopeOf(s string) (auth.Scope, error) {
	switch auth.Scope(s) {
	case auth.ScopeRead:
		return auth.ScopeRead, nil
	case auth.ScopeWrite:
		return auth.ScopeWrite, nil
	default:
		return "", errValidation("scope", `scope must be "read" or "write"`)
	}
}

// orgRoleOf parses an org role from a request body.
func orgRoleOf(s string) (auth.OrgRole, error) {
	switch auth.OrgRole(s) {
	case auth.OrgRoleMember:
		return auth.OrgRoleMember, nil
	case auth.OrgRoleAdmin:
		return auth.OrgRoleAdmin, nil
	case auth.OrgRoleOwner:
		return auth.OrgRoleOwner, nil
	default:
		return "", errValidation("role", `role must be "member", "admin" or "owner"`)
	}
}

// projectRoleOf parses a project role from a request body.
func projectRoleOf(s string) (auth.ProjectRole, error) {
	switch auth.ProjectRole(s) {
	case auth.ProjectRoleReader:
		return auth.ProjectRoleReader, nil
	case auth.ProjectRoleWriter:
		return auth.ProjectRoleWriter, nil
	case auth.ProjectRoleAdmin:
		return auth.ProjectRoleAdmin, nil
	default:
		return "", errValidation("role", `role must be "reader", "writer" or "admin"`)
	}
}

// backendKindOf parses a backend kind from a path segment or a request body.
func backendKindOf(s string) (repository.BackendKind, error) {
	switch repository.BackendKind(s) {
	case repository.BackendKindSstate:
		return repository.BackendKindSstate, nil
	case repository.BackendKindDownloads:
		return repository.BackendKindDownloads, nil
	case repository.BackendKindHashserv:
		return repository.BackendKindHashserv, nil
	case repository.BackendKindBazel:
		return repository.BackendKindBazel, nil
	case repository.BackendKindOci:
		return repository.BackendKindOci, nil
	default:
		return "", errValidation("kind",
			`kind must be one of "sstate", "downloads", "hashserv", "bazel", "oci"`)
	}
}
