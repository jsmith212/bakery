package auth

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// goBuild compiles a package and returns the combined output. A build FAILURE is
// the expected outcome for the forgery package, so the error is a value here, not
// a fatality.
func goBuild(t *testing.T, args ...string) (string, error) {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "go", append([]string{"build"}, args...)...).CombinedOutput()

	return string(out), err
}

// Fixed ids for the capability tables.
var (
	orgA     = uuid(0x0a)
	orgB     = uuid(0x0b)
	projectA = uuid(0x1a) // in orgA
	projectB = uuid(0x1b) // in orgB
)

// sessionPrincipal builds a console principal. It is in the TEST file and calls
// the unexported constructor -- which is the whole point: a test in package auth
// can do this, and nothing outside package auth can.
func sessionPrincipal(site SiteRole, orgs map[pgtype.UUID]OrgRole, projects map[pgtype.UUID]ProjectRole) Principal {
	return &principal{
		userID: uuid(0x01), issuer: "https://idp", subject: "s", email: "u@example.com",
		displayName: "U", method: MethodSession, siteRole: site,
		orgs: orgs, projects: projects, key: nil,
	}
}

func keyPrincipal(project pgtype.UUID, scope Scope, site SiteRole) Principal {
	return &principal{
		userID: uuid(0x01), issuer: "", subject: "", email: "", displayName: "",
		method: MethodAPIKey, siteRole: site, orgs: nil, projects: nil,
		key: &KeyGrant{KeyID: uuid(0xff), ProjectID: project, Scope: scope},
	}
}

func TestPrincipalCapabilities(t *testing.T) {
	tests := []struct {
		name          string
		p             Principal
		wantReadA     bool
		wantWriteA    bool
		wantAdminProj bool
		wantAdminOrg  bool
		wantOwnOrg    bool
	}{
		{
			name:      "a site admin can do everything",
			p:         sessionPrincipal(SiteRoleAdmin, nil, nil),
			wantReadA: true, wantWriteA: true, wantAdminProj: true, wantAdminOrg: true, wantOwnOrg: true,
		},
		{
			name:      "a stranger can do nothing",
			p:         sessionPrincipal(SiteRoleUser, nil, nil),
			wantReadA: false, wantWriteA: false, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name:      "an org member reads every project in the org but writes none",
			p:         sessionPrincipal(SiteRoleUser, map[pgtype.UUID]OrgRole{orgA: OrgRoleMember}, nil),
			wantReadA: true, wantWriteA: false, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name:      "an org admin administers the org's projects",
			p:         sessionPrincipal(SiteRoleUser, map[pgtype.UUID]OrgRole{orgA: OrgRoleAdmin}, nil),
			wantReadA: true, wantWriteA: true, wantAdminProj: true, wantAdminOrg: true, wantOwnOrg: false,
		},
		{
			name:      "an org owner also owns",
			p:         sessionPrincipal(SiteRoleUser, map[pgtype.UUID]OrgRole{orgA: OrgRoleOwner}, nil),
			wantReadA: true, wantWriteA: true, wantAdminProj: true, wantAdminOrg: true, wantOwnOrg: true,
		},
		{
			name: "a project reader reads only",
			p: sessionPrincipal(SiteRoleUser, nil,
				map[pgtype.UUID]ProjectRole{projectA: ProjectRoleReader}),
			wantReadA: true, wantWriteA: false, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name: "a project writer writes",
			p: sessionPrincipal(SiteRoleUser, nil,
				map[pgtype.UUID]ProjectRole{projectA: ProjectRoleWriter}),
			wantReadA: true, wantWriteA: true, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name: "a project admin administers the project but not the org",
			p: sessionPrincipal(SiteRoleUser, nil,
				map[pgtype.UUID]ProjectRole{projectA: ProjectRoleAdmin}),
			wantReadA: true, wantWriteA: true, wantAdminProj: true, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name: "a membership in ANOTHER org grants nothing here",
			p: sessionPrincipal(SiteRoleUser, map[pgtype.UUID]OrgRole{orgB: OrgRoleOwner},
				map[pgtype.UUID]ProjectRole{projectB: ProjectRoleAdmin}),
			wantReadA: false, wantWriteA: false, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name:      "a read-scoped key reads its project and writes nothing",
			p:         keyPrincipal(projectA, ScopeRead, SiteRoleUser),
			wantReadA: true, wantWriteA: false, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name:      "a write-scoped key writes its project",
			p:         keyPrincipal(projectA, ScopeWrite, SiteRoleUser),
			wantReadA: true, wantWriteA: true, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			name:      "a key for ANOTHER project grants nothing here",
			p:         keyPrincipal(projectB, ScopeWrite, SiteRoleUser),
			wantReadA: false, wantWriteA: false, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
		{
			// THE cap. The owning user is a site admin, but a key is a DELEGATION
			// limited to one project and one scope. If site admin leaked through, a
			// read-scoped key minted for one project would be a master key for the
			// whole installation -- and validation cannot notice, because it joins
			// nothing.
			name:      "a key NEVER inherits the owner's site admin",
			p:         keyPrincipal(projectA, ScopeRead, SiteRoleAdmin),
			wantReadA: true, wantWriteA: false, wantAdminProj: false, wantAdminOrg: false, wantOwnOrg: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.CanReadProject(orgA, projectA); got != tt.wantReadA {
				t.Errorf("CanReadProject(orgA, projectA) = %v, want %v", got, tt.wantReadA)
			}

			if got := tt.p.CanWriteProject(orgA, projectA); got != tt.wantWriteA {
				t.Errorf("CanWriteProject(orgA, projectA) = %v, want %v", got, tt.wantWriteA)
			}

			if got := tt.p.CanAdminProject(orgA, projectA); got != tt.wantAdminProj {
				t.Errorf("CanAdminProject(orgA, projectA) = %v, want %v", got, tt.wantAdminProj)
			}

			if got := tt.p.CanAdminOrg(orgA); got != tt.wantAdminOrg {
				t.Errorf("CanAdminOrg(orgA) = %v, want %v", got, tt.wantAdminOrg)
			}

			if got := tt.p.CanOwnOrg(orgA); got != tt.wantOwnOrg {
				t.Errorf("CanOwnOrg(orgA) = %v, want %v", got, tt.wantOwnOrg)
			}
		})
	}
}

// TestAPIKeyPrincipalIsNotASiteAdmin isolates the escalation cap, because it is
// the difference between a scoped credential and a master key.
func TestAPIKeyPrincipalIsNotASiteAdmin(t *testing.T) {
	p := keyPrincipal(projectA, ScopeWrite, SiteRoleAdmin)

	if p.IsSiteAdmin() {
		t.Error("IsSiteAdmin() = true for an API key principal; a key must never carry site admin")
	}

	if p.CanViewOrg(orgA) || p.CanAdminOrg(orgA) || p.CanOwnOrg(orgA) {
		t.Error("an API key principal reached the org control plane; it must not")
	}

	grant, ok := p.APIKey()
	if !ok {
		t.Fatal("APIKey() reported no grant on an API key principal")
	}

	if grant.ProjectID != projectA || grant.Scope != ScopeWrite {
		t.Errorf("APIKey() = %+v, want projectA at write scope", grant)
	}

	// And a session principal has no key grant to be confused with one.
	if _, ok := sessionPrincipal(SiteRoleAdmin, nil, nil).APIKey(); ok {
		t.Error("APIKey() reported a grant on a session principal")
	}
}

func TestScopeIsCappedAtTheRole(t *testing.T) {
	tests := []struct {
		role   ProjectRole
		max    Scope
		wantRW bool // may this role grant a write-scoped key?
		wantRO bool // may this role grant a read-scoped key?
	}{
		{role: ProjectRoleReader, max: ScopeRead, wantRW: false, wantRO: true},
		{role: ProjectRoleWriter, max: ScopeWrite, wantRW: true, wantRO: true},
		{role: ProjectRoleAdmin, max: ScopeWrite, wantRW: true, wantRO: true},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			if got := MaxScopeForRole(tt.role); got != tt.max {
				t.Errorf("MaxScopeForRole(%q) = %q, want %q", tt.role, got, tt.max)
			}

			if got := ScopeWithinRole(ScopeWrite, tt.role); got != tt.wantRW {
				t.Errorf("ScopeWithinRole(write, %q) = %v, want %v", tt.role, got, tt.wantRW)
			}

			if got := ScopeWithinRole(ScopeRead, tt.role); got != tt.wantRO {
				t.Errorf("ScopeWithinRole(read, %q) = %v, want %v", tt.role, got, tt.wantRO)
			}
		})
	}
}

// TestContextHasNoZeroValuePrincipal is the context half of the invariant.
//
// The failure mode for an unauthenticated request must be a MISSING principal,
// never a zero-valued one that reads as "some valid user with no roles" -- because
// a principal with no roles still answers "who are you?" with a valid-looking
// identity, and the next refactor that trusts it is a bypass.
func TestContextHasNoZeroValuePrincipal(t *testing.T) {
	p, ok := FromContext(context.Background())
	if ok {
		t.Fatal("FromContext() on a bare context reported a principal")
	}

	if p != nil {
		t.Fatalf("FromContext() on a bare context returned %#v, want nil", p)
	}

	// A Principal that IS there comes back intact.
	want := sessionPrincipal(SiteRoleAdmin, nil, nil)

	got, ok := FromContext(withPrincipal(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("FromContext() round trip = (%v, %v), want the principal we put in", got, ok)
	}
}

// TestPrincipalIsUnforgeable makes the COMPILER the enforcement mechanism, not a
// comment.
//
// internal/auth/forgery/forgery.go is a package OUTSIDE internal/auth that tries
// every way there is to manufacture a Principal. It is behind a build tag, so it
// is invisible to a normal build; this test compiles it ON PURPOSE and asserts the
// compiler REFUSES it, then checks the refusals are the ones we intended and not
// some unrelated typo.
//
// Why this and not a reflect test: reflection can tell you a field is unexported,
// but it cannot tell you that no arrangement of exported identifiers produces a
// usable value. Only the type checker can, so we ask it.
func TestPrincipalIsUnforgeable(t *testing.T) {
	out, err := goBuild(t, "-tags", "principal_forgery", "./forgery")
	if err == nil {
		t.Fatal("the forgery package COMPILED. auth.Principal is constructible from outside " +
			"internal/auth, and the OCI upstream fetch can therefore be handed a forged identity.")
	}

	// Each of these is one attack the compiler must reject. If a future refactor
	// (say, exporting the principal struct "just for the API layer") opens one of
	// them, the corresponding line here stops erroring and the test fails.
	forbidden := []struct {
		attack string
		want   string
	}{
		{attack: "composite literal of the interface", want: "invalid composite literal type auth.Principal"},
		{attack: "reference to the unexported implementation", want: "undefined: auth.principal"},
		{attack: "implementing the interface in another package", want: "sealed"},
	}

	for _, f := range forbidden {
		if !strings.Contains(out, f.want) {
			t.Errorf("the compiler did not reject the %s attack with %q.\nBuild output:\n%s",
				f.attack, f.want, out)
		}
	}
}

// TestPrincipalHasNoExportedConstructor walks internal/auth's own source and
// asserts no exported function or method hands a Principal to a caller who did not
// authenticate for it.
//
// It is the belt to TestPrincipalIsUnforgeable's braces: that test proves you
// cannot BUILD one, and this one proves we do not accidentally GIVE one away --
// for example by adding an exported `NewPrincipal` helper "for tests" that ships in
// the production binary.
func TestPrincipalHasNoExportedConstructor(t *testing.T) {
	// The exported functions that may hand out a Principal, and why each is safe.
	// Adding a name here should require justifying it out loud.
	allowed := map[string]string{
		"Authenticate":      "verifies the request's credential before it returns one",
		"AuthenticateCache": "verifies the Basic/Bearer/cookie credential (constant-time key probe) before it returns one",
		"AuthenticateToken": "verifies a bare token through the SAME constant-time zero-join key probe before it " +
			"returns one -- it is authenticateKey with no HTTP in front of it, for hashserv's in-band `auth` RPC, " +
			"and it cannot mint a Principal the Basic and Bearer arms would have refused",
		"FromContext": "reads back a Principal this package already verified and put there",
		"FromRequest": "ditto",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/auth: %v", err)
	}

	fset := token.NewFileSet()

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}

			for _, result := range fn.Type.Results.List {
				ident, ok := result.Type.(*ast.Ident)
				if !ok || ident.Name != "Principal" {
					continue
				}

				if _, ok := allowed[fn.Name.Name]; !ok {
					t.Errorf("%s: exported func %s returns a Principal but is not on the allow-list. "+
						"If it does not VERIFY a credential first, it is a forgery primitive.",
						name, fn.Name.Name)
				}
			}
		}
	}
}
