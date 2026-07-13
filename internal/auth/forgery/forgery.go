//go:build principal_forgery

// This file MUST NOT COMPILE. See doc.go, and TestPrincipalIsUnforgeable.
//
// Every declaration below is an attempt to obtain an auth.Principal without
// authenticating. Each one must be a compile error. If you are reading this
// because the build broke, the question to ask is not "how do I fix this file" --
// it is "what did I just export from internal/auth, and does it let an attacker
// mint an identity".

package forgery

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
)

// ATTACK 1: the composite literal.
//
// The reflex. If Principal were an exported STRUCT, this would compile and hand
// the caller an identity with whatever roles they felt like typing -- and even a
// struct with only unexported fields would still yield a usable zero value.
// Principal is an interface, so this is not a composite literal type at all.
//
// want: invalid composite literal type auth.Principal
var _ = auth.Principal{}

// ATTACK 2: name the implementation directly.
//
// want: undefined: auth.principal
var _ = auth.principal{}

// ATTACK 3: implement the interface ourselves.
//
// impostor implements EVERY exported method of auth.Principal -- all seventeen of
// them, faithfully, returning site-admin for everything. It is still not an
// auth.Principal, because the interface carries an unexported method that only a
// type declared inside internal/auth can supply.
//
// want: sealed
var _ auth.Principal = impostor{}

type impostor struct{}

func (impostor) UserID() pgtype.UUID     { return pgtype.UUID{} }
func (impostor) Issuer() string          { return "https://evil.example.com" }
func (impostor) Subject() string         { return "attacker" }
func (impostor) Email() string           { return "attacker@evil.example.com" }
func (impostor) DisplayName() string     { return "Totally Legitimate" }
func (impostor) Method() auth.Method     { return auth.MethodSession }
func (impostor) SiteRole() auth.SiteRole { return auth.SiteRoleAdmin }
func (impostor) IsSiteAdmin() bool       { return true }

func (impostor) OrgRole(pgtype.UUID) (auth.OrgRole, bool) { return auth.OrgRoleOwner, true }

func (impostor) ProjectRole(pgtype.UUID) (auth.ProjectRole, bool) {
	return auth.ProjectRoleAdmin, true
}

func (impostor) APIKey() (auth.KeyGrant, bool) { return auth.KeyGrant{}, false }

func (impostor) CanViewOrg(pgtype.UUID) bool                   { return true }
func (impostor) CanAdminOrg(pgtype.UUID) bool                  { return true }
func (impostor) CanOwnOrg(pgtype.UUID) bool                    { return true }
func (impostor) CanReadProject(pgtype.UUID, pgtype.UUID) bool  { return true }
func (impostor) CanWriteProject(pgtype.UUID, pgtype.UUID) bool { return true }
func (impostor) CanAdminProject(pgtype.UUID, pgtype.UUID) bool { return true }

// ATTACK 4: declare the sealed method too.
//
// This does not help. An unexported method name is qualified by the package that
// declares it, so `forgery.sealed` is a different method from `auth.sealed` and
// pretender still does not implement the interface. This is the property that
// makes an unexported method a real seal rather than a speed bump.
//
// want: sealed
var _ auth.Principal = pretender{}

type pretender struct{ impostor }

func (pretender) sealed() {}
