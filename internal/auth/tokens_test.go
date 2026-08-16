package auth

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/metrics"
)

// ---------------------------------------------------------------------------
// The allowlist inversion.
// ---------------------------------------------------------------------------

// declaredMethods reads every `Method` constant out of the WHOLE PACKAGE's
// source, not out of principal.go alone.
//
// Go has no reflection over constants, so the only way to make "you added a
// Method and did not classify it" a TEST FAILURE rather than a code-review
// question is to read the declarations back out of the source. The alternative --
// a hand-maintained slice next to the constants -- fails in exactly the case it
// exists to catch, because whoever forgot to classify the new Method also forgot
// to add it to the slice.
//
// The file list is the same trap one level up. This package already spreads
// credential code across tokens.go, usertoken.go, orgtoken.go and apikey.go, so
// `MethodDeployKey` declared beside its validator in a new deploykey.go would be
// invisible to a scan of principal.go -- and invisible to every gate that drives
// itself from this function. Scanning the directory costs one ReadDir and cannot
// go stale.
func declaredMethods(t *testing.T) []Method {
	t.Helper()

	var out []Method

	for _, file := range packageFiles(t, ".") {
		out = append(out, methodConstsIn(t, file)...)
	}

	if len(out) == 0 {
		t.Fatal("declaredMethods found no Method constants; the parser lost track of the const block")
	}

	return out
}

// packageFiles parses every non-test .go file in dir. Test files are excluded
// deliberately: a Method declared in a _test.go is a fixture, not a credential.
func packageFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()

	var out []*ast.File

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		out = append(out, file)
	}

	if len(out) == 0 {
		t.Fatalf("found no non-test Go files in %s; the source scan has lost its inputs", dir)
	}

	return out
}

// methodConstsIn returns the `Method` constants declared in one parsed file.
func methodConstsIn(t *testing.T, file *ast.File) []Method {
	t.Helper()

	var out []Method

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}

		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}

			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "Method" || len(value.Values) == 0 {
				continue
			}

			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				t.Fatalf("Method constant %v is not a string literal; declaredMethods cannot read it",
					value.Names)
			}

			unquoted, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquote %s: %v", lit.Value, err)
			}

			out = append(out, Method(unquoted))
		}
	}

	return out
}

// TestEveryMethodIsExplicitlyClassified is the gate that makes a new credential
// kind default CLOSED.
//
// Before the allowlist inversion every capability guard asked `p.method ==
// MethodAPIKey`. That is a DENYLIST: declaring MethodUserToken made a personal
// access token a full human principal -- site admin included, if its owner was
// one -- until seven separate guards were found and edited, with no compiler
// error and no failing test in between. The window in which that is true is
// exactly the window a credential wave schedules for adding Methods.
//
// So: every Method constant declared anywhere in package auth must appear in the table
// below, WITH THE EXACT SET OF CAPABILITIES it is allowed to answer `true` to,
// asserted against a principal carrying the strongest roles the struct can hold.
// Adding a constant without adding a row fails. Opening a capability arm for a
// non-interactive Method without amending its row fails too -- which is the
// point: opening one is a deliberate act with a test to update, never a side
// effect of declaring a constant.
func TestEveryMethodIsExplicitlyClassified(t *testing.T) {
	t.Parallel()

	// The classification. Every field is stated for every Method, so a `false` is
	// as deliberate as a `true` and nothing is inherited from a zero value.
	type capabilities struct {
		interactive bool

		siteAdmin      bool
		viewOrg        bool
		adminOrg       bool
		ownOrg         bool
		readProject    bool
		writeProject   bool
		adminProject   bool
		mintCredential bool
	}

	classified := map[Method]capabilities{
		// A human at a console or a freshly verified CLI login: everything.
		MethodSession: {
			interactive: true, siteAdmin: true, viewOrg: true, adminOrg: true, ownOrg: true,
			readProject: true, writeProject: true, adminProject: true, mintCredential: true,
		},
		MethodBearer: {
			interactive: true, siteAdmin: true, viewOrg: true, adminOrg: true, ownOrg: true,
			readProject: true, writeProject: true, adminProject: true, mintCredential: true,
		},
		MethodDev: {
			interactive: true, siteAdmin: true, viewOrg: true, adminOrg: true, ownOrg: true,
			readProject: true, writeProject: true, adminProject: true, mintCredential: true,
		},

		// An API key answers from its GRANT and from nothing else. The principal below
		// carries no grant at all, so it reads and writes nothing -- which is also what
		// a newly declared, unclassified Method would get.
		MethodAPIKey: {
			interactive: false, siteAdmin: false, viewOrg: false, adminOrg: false, ownOrg: false,
			readProject: false, writeProject: false, adminProject: false, mintCredential: false,
		},

		// A personal access token ACTS AS its owner: it reads their orgs map and their
		// projects map, live. It is still not a human at a console -- no site admin, no
		// org admin, no project admin, and it cannot mint another credential.
		MethodUserToken: {
			interactive: false, siteAdmin: false, viewOrg: true, adminOrg: false, ownOrg: false,
			readProject: true, writeProject: true, adminProject: false, mintCredential: false,
		},

		// A robot answers from its RobotGrant. The principal below carries none, so
		// like the API key it authorizes nothing at all.
		MethodOrgToken: {
			interactive: false, siteAdmin: false, viewOrg: false, adminOrg: false, ownOrg: false,
			readProject: false, writeProject: false, adminProject: false, mintCredential: false,
		},
	}

	declared := declaredMethods(t)

	for _, m := range declared {
		if _, ok := classified[m]; !ok {
			t.Errorf("Method %q is declared in package auth but is not classified here.\n"+
				"Every Method must state its full capability set; an unclassified one "+
				"silently inherits whatever the guards happen to allow.", m)
		}
	}

	seen := make(map[Method]bool, len(declared))
	for _, m := range declared {
		seen[m] = true
	}

	for m := range classified {
		if !seen[m] {
			t.Errorf("Method %q is classified here but no longer declared in principal.go; "+
				"remove the row so the table stays a mirror of the source", m)
		}
	}

	for _, m := range declared {
		want := classified[m]

		// The strongest roles the struct can hold, and NO grant: a leak shows up here.
		p := &principal{
			userID: uuid(0x01), issuer: "https://idp.example.com", subject: "sub",
			email: "human@example.com", displayName: "Human",
			method:   m,
			siteRole: SiteRoleAdmin,
			orgs:     map[pgtype.UUID]OrgRole{orgA: OrgRoleOwner},
			projects: map[pgtype.UUID]ProjectRole{projectA: ProjectRoleAdmin},
			key:      nil, robot: nil, maxScope: ScopeWrite,
		}

		got := capabilities{
			interactive:    p.isInteractive(),
			siteAdmin:      p.IsSiteAdmin(),
			viewOrg:        p.CanViewOrg(orgA),
			adminOrg:       p.CanAdminOrg(orgA),
			ownOrg:         p.CanOwnOrg(orgA),
			readProject:    p.CanReadProject(orgA, projectA),
			writeProject:   p.CanWriteProject(orgA, projectA),
			adminProject:   p.CanAdminProject(orgA, projectA),
			mintCredential: requireMintAuthority(p) == nil,
		}

		if got != want {
			t.Errorf("%q: capabilities = %+v, want %+v.\n"+
				"A capability arm was opened or closed without amending this table.", m, got, want)
		}
	}
}

// TestUserTokenScopeCeilingIsIndependentOfRole pins the half of the PAT ruling
// that a role table cannot express: max_scope is a CEILING, not a grant.
//
// Read-scoped means the token can never write, however senior its holder is or
// becomes. Write-scoped means writes are bounded by the holder's live project
// roles and by NOTHING else -- in particular not by the org-admin short-circuit,
// which is unreachable for every non-interactive credential.
func TestUserTokenScopeCeilingIsIndependentOfRole(t *testing.T) {
	t.Parallel()

	pat := func(maxScope Scope, projects map[pgtype.UUID]ProjectRole, orgs map[pgtype.UUID]OrgRole) *principal {
		return &principal{
			userID: uuid(0x01), issuer: "", subject: "", email: "", displayName: "",
			method: MethodUserToken, siteRole: SiteRoleAdmin,
			orgs: orgs, projects: projects, key: nil, robot: nil, maxScope: maxScope,
		}
	}

	writerProject := map[pgtype.UUID]ProjectRole{projectA: ProjectRoleWriter}
	ownerOrg := map[pgtype.UUID]OrgRole{orgA: OrgRoleOwner}

	if pat(ScopeRead, writerProject, ownerOrg).CanWriteProject(orgA, projectA) {
		t.Error("a read-scoped personal access token wrote a project its holder can write")
	}

	if !pat(ScopeWrite, writerProject, ownerOrg).CanWriteProject(orgA, projectA) {
		t.Error("a write-scoped token could not write a project its holder is a writer on")
	}

	// The org-admin short-circuit: an org OWNER with no explicit project role. A
	// session gets write here; a token must not.
	if pat(ScopeWrite, nil, ownerOrg).CanWriteProject(orgA, projectA) {
		t.Error("a personal access token inherited write from its owner's ORG-ADMIN role; " +
			"the short-circuit must be unreachable for a non-interactive credential")
	}

	// ...but org membership still implies READ, exactly as it does for the human.
	if !pat(ScopeRead, nil, ownerOrg).CanReadProject(orgA, projectA) {
		t.Error("a personal access token could not read a project in an org its holder belongs to")
	}
}

// TestNonInteractiveWriteIgnoresTheOrgAdminShortCircuit pins the one guard whose
// inversion changes more than a boolean.
//
// CanWriteProject short-circuits on CanAdminOrg for a human, and that is correct
// for a human. For a machine credential it must be unreachable: an org admin who
// mints a token gets write exactly where the token's own grant says so, never by
// inheriting the role of the person who minted it. The early return is what makes
// that structural rather than a second check someone can drop.
func TestNonInteractiveWriteIgnoresTheOrgAdminShortCircuit(t *testing.T) {
	t.Parallel()

	// An org OWNER, with a read-scoped key for the very project in question.
	p := &principal{
		userID: uuid(0x01), issuer: "", subject: "", email: "", displayName: "",
		method:   MethodAPIKey,
		siteRole: SiteRoleAdmin,
		orgs:     map[pgtype.UUID]OrgRole{orgA: OrgRoleOwner},
		projects: nil,
		key:      &KeyGrant{KeyID: uuid(0xff), ProjectID: projectA, Scope: ScopeRead},
		robot:    nil,
		maxScope: ScopeRead,
	}

	if p.CanWriteProject(orgA, projectA) {
		t.Error("a read-scoped credential wrote a project because its owner is an org owner")
	}

	if !p.CanReadProject(orgA, projectA) {
		t.Error("a read-scoped credential cannot read the project it was minted for")
	}
}

// ---------------------------------------------------------------------------
// The prefix family.
// ---------------------------------------------------------------------------

// tokenOfKind is a plausible plaintext for each credential kind. The bodies are
// long enough to satisfy looksLikeAPIKey's display-length floor, so the bkry_
// case reaches a real probe rather than being refused on shape.
const (
	apiKeyToken    = TokenPrefix + "AAAAAAAAbbbbccccddddeeeeffffgggghhhhiiiijjj"
	userTokenValue = UserTokenPrefix + "AAAAAAAAbbbbccccddddeeeeffffgggghhhhiiiijjj"
	orgTokenValue  = OrgTokenPrefix + "AAAAAAAAbbbbccccddddeeeeffffgggghhhhiiiijjj"
)

// TestTokenKindRoutesOnThePrefixAlone. The family gate and the dispatch switch
// must agree, and looksLikeAPIKey must stay bkry_-only: widening it is precisely
// how a bkru_ token ends up probing api_keys.
func TestTokenKindRoutesOnThePrefixAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      string
		wantKind   kind
		wantFamily bool
		wantAPIKey bool // looksLikeAPIKey, the bkry_-ONLY gate
	}{
		{"an api key", apiKeyToken, kindAPIKey, true, true},
		{"a user token", userTokenValue, kindUserToken, true, false},
		{"an org token", orgTokenValue, kindOrgToken, true, false},
		{"a short api key still routes to api_keys", TokenPrefix + "x", kindAPIKey, true, false},
		{"a bare prefix is not a token", TokenPrefix, kindUnknown, false, false},
		{"a bare user prefix is not a token", UserTokenPrefix, kindUnknown, false, false},
		{"an empty string", "", kindUnknown, false, false},
		{"a docker hub pat", "dckr_pat_S3CRET_VALUE_xyz", kindUnknown, false, false},
		{"the anonymous sentinel", "anonymous", kindUnknown, false, false},
		{"a jwt", "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.e30.sig", kindUnknown, false, false},
		{"a near miss", "bkr_abcdefghijklmnop", kindUnknown, false, false},
		{"the wrong case", "BKRY_abcdefghijklmnop", kindUnknown, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tokenKind(tt.token); got != tt.wantKind {
				t.Errorf("tokenKind() = %q, want %q", got, tt.wantKind)
			}

			if got := LooksLikeBakeryToken(tt.token); got != tt.wantFamily {
				t.Errorf("LooksLikeBakeryToken() = %v, want %v", got, tt.wantFamily)
			}

			if got := looksLikeAPIKey(tt.token); got != tt.wantAPIKey {
				t.Errorf("looksLikeAPIKey() = %v, want %v -- it must stay bkry_-only",
					got, tt.wantAPIKey)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The three seams.
// ---------------------------------------------------------------------------

// tokenFixture is the REAL Service over a real, migrated Postgres, with one live
// credential of each kind seeded into its own table.
//
// It used to be a fake key store plus two stub validators, because two of the
// three tables did not exist yet. They do now, and the stubs are gone: every
// assertion below drives the production probes -- the covering-index scan on
// api_keys, the index-scan-plus-PK-join on user_tokens, the zero-join scan on
// org_tokens -- through the production dispatch switch. A stub that answers "yes,
// valid" cannot prove a credential reaches the right TABLE, which is the entire
// property this file exists to hold.
//
// The three stores are wrapped in counting decorators so the tests can still
// assert what did NOT happen: no api_keys probe for a `bkru_` token, no probe of
// any kind for a forwarded Docker Hub PAT.
type tokenFixture struct {
	*testService

	keys       *countingKeyStore
	userTokens *countingUserTokenStore
	orgTokens  *countingOrgTokenStore

	svc *Service

	// The three plaintexts. Each exists here and nowhere else -- the schema stores
	// only their SHA-256.
	apiKey    string
	userToken string
	orgToken  string

	userID    pgtype.UUID
	orgID     pgtype.UUID
	projectID pgtype.UUID
	robotID   pgtype.UUID
}

// probes is the total number of database probes across all three tables.
func (f *tokenFixture) probes() int {
	return f.keys.validate + f.userTokens.validate + f.orgTokens.validate
}

func (f *tokenFixture) resetProbes() {
	f.keys.validate, f.userTokens.validate, f.orgTokens.validate = 0, 0, 0
}

func newTokenFixture(t *testing.T) *tokenFixture {
	t.Helper()

	ts := newTestService(t, "", false)
	ctx := t.Context()

	org, err := ts.store.CreateOrganization(ctx, repository.CreateOrganizationParams{
		Slug: "acme", Name: "Acme",
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	project, err := ts.store.CreateProject(ctx, repository.CreateProjectParams{
		OrgID: org.ID, Slug: "firmware", Name: "Firmware",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	user := seedUser(t, ts, "anna@example.com", "anna")
	grantOrgRole(t, ts, user.ID, org.ID, OrgRoleMember)
	grantProjectRole(t, ts, user.ID, project.ID, ProjectRoleWriter)

	fix := &tokenFixture{
		testService: ts,
		keys:        &countingKeyStore{inner: ts.keys},
		userTokens:  &countingUserTokenStore{inner: ts.userTokens},
		orgTokens:   &countingOrgTokenStore{inner: ts.orgTokens},
		svc:         ts.Service,
		apiKey:      "",
		userToken:   "",
		orgToken:    "",
		userID:      user.ID,
		orgID:       org.ID,
		projectID:   project.ID,
		robotID:     pgtype.UUID{},
	}

	// The decorators go on BEFORE any credential is minted, so a mint that probed
	// would be visible too.
	ts.keys = fix.keys
	ts.userTokens = fix.userTokens
	ts.orgTokens = fix.orgTokens

	fix.apiKey = seedAPIKey(t, ts, user.ID, project.ID, ScopeWrite)
	fix.userToken = seedUserToken(t, ts, user.ID, ScopeWrite)
	fix.robotID, fix.orgToken = seedRobotToken(t, ts, "ci", org.ID, user.ID, ScopeWrite)

	fix.resetProbes()

	return fix
}

// ---------------------------------------------------------------------------
// Seed helpers. Each writes through the REAL query, so a schema CHECK that would
// refuse the production write refuses these too.
// ---------------------------------------------------------------------------

func seedUser(t *testing.T, ts *testService, email, subject string) repository.User {
	t.Helper()

	user, err := ts.store.UpsertUser(t.Context(), repository.UpsertUserParams{
		Issuer: "https://idp.example.com", Subject: subject,
		Email: email, DisplayName: email,
		AvatarURL: pgtype.Text{String: "", Valid: false},
		SiteRole:  SiteRoleUser,
		SiteGroup: pgtype.Text{String: "", Valid: false},
	})
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}

	return user
}

func grantOrgRole(t *testing.T, ts *testService, userID, orgID pgtype.UUID, role OrgRole) {
	t.Helper()

	if _, err := ts.store.GrantOrgMembershipLocal(t.Context(), repository.GrantOrgMembershipLocalParams{
		UserID: userID, OrgID: orgID,
		LocalRole: repository.NullOrgRole{OrgRole: role, Valid: true},
		GrantedBy: userID,
	}); err != nil {
		t.Fatalf("grant org role: %v", err)
	}
}

func grantProjectRole(t *testing.T, ts *testService, userID, projectID pgtype.UUID, role ProjectRole) {
	t.Helper()

	if _, err := ts.store.UpsertProjectMembership(t.Context(), repository.UpsertProjectMembershipParams{
		UserID: userID, ID: projectID, Role: role,
	}); err != nil {
		t.Fatalf("grant project role: %v", err)
	}
}

func seedAPIKey(t *testing.T, ts *testService, userID, projectID pgtype.UUID, scope Scope) string {
	t.Helper()

	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}

	if _, err := ts.store.CreateAPIKey(t.Context(), repository.CreateAPIKeyParams{
		UserID: userID, ProjectID: projectID, Name: "ci",
		TokenSha256: key.Hash, TokenPrefix: key.Prefix, Scope: scope,
		ExpiresAt: pgtype.Timestamptz{Time: time.Time{}, InfinityModifier: pgtype.Finite, Valid: false},
	}); err != nil {
		t.Fatalf("seed api key: %v", err)
	}

	return key.Token
}

func seedUserToken(t *testing.T, ts *testService, userID pgtype.UUID, maxScope Scope) string {
	t.Helper()

	token, err := GenerateToken(UserTokenPrefix)
	if err != nil {
		t.Fatalf("generate user token: %v", err)
	}

	if _, err := ts.store.CreateUserToken(t.Context(), repository.CreateUserTokenParams{
		UserID: userID, Name: "laptop",
		TokenSha256: token.Hash, TokenPrefix: token.Prefix, MaxScope: maxScope,
		ExpiresAt: pgtype.Timestamptz{Time: time.Time{}, InfinityModifier: pgtype.Finite, Valid: false},
	}); err != nil {
		t.Fatalf("seed user token: %v", err)
	}

	return token.Token
}

func seedRobotToken(
	t *testing.T, ts *testService, name string, orgID, createdBy pgtype.UUID, scope Scope,
) (pgtype.UUID, string) {
	t.Helper()

	robot, err := ts.store.CreateRobot(t.Context(), repository.CreateRobotParams{
		OrgID: orgID, Name: name, Description: "",
		CreatedBy: createdBy, CreatedByEmail: "anna@example.com",
	})
	if err != nil {
		t.Fatalf("seed robot: %v", err)
	}

	token, err := GenerateToken(OrgTokenPrefix)
	if err != nil {
		t.Fatalf("generate org token: %v", err)
	}

	if _, err := ts.store.CreateOrgToken(t.Context(), repository.CreateOrgTokenParams{
		RobotID: robot.ID, OrgID: orgID, Name: "primary",
		TokenSha256: token.Hash, TokenPrefix: token.Prefix, Scope: scope,
		ExpiresAt: pgtype.Timestamptz{
			Time: time.Now().Add(24 * time.Hour), InfinityModifier: pgtype.Finite, Valid: true,
		},
		CreatedBy: createdBy, CreatedByEmail: "anna@example.com",
	}); err != nil {
		t.Fatalf("seed org token: %v", err)
	}

	return robot.ID, token.Token
}

// ---------------------------------------------------------------------------
// Counting decorators. They exist for the NEGATIVE assertions -- "this token
// never touched that table" -- which are the ones a passing positive test cannot
// make.
// ---------------------------------------------------------------------------

type countingKeyStore struct {
	inner    keyStore
	validate int
}

func (c *countingKeyStore) validateKey(ctx context.Context, hash []byte) (keyGrantRow, error) {
	c.validate++

	return c.inner.validateKey(ctx, hash)
}

func (c *countingKeyStore) touchKeys(ctx context.Context, ids []pgtype.UUID) error {
	return c.inner.touchKeys(ctx, ids)
}

type countingUserTokenStore struct {
	inner    userTokenStore
	validate int
}

func (c *countingUserTokenStore) validateUserToken(
	ctx context.Context, hash []byte,
) (userTokenRow, error) {
	c.validate++

	return c.inner.validateUserToken(ctx, hash)
}

func (c *countingUserTokenStore) touchUserTokens(ctx context.Context, ids []pgtype.UUID) error {
	return c.inner.touchUserTokens(ctx, ids)
}

type countingOrgTokenStore struct {
	inner    orgTokenStore
	validate int
}

func (c *countingOrgTokenStore) validateOrgToken(
	ctx context.Context, hash []byte,
) (orgTokenRow, error) {
	c.validate++

	return c.inner.validateOrgToken(ctx, hash)
}

func (c *countingOrgTokenStore) touchOrgTokens(ctx context.Context, ids []pgtype.UUID) error {
	return c.inner.touchOrgTokens(ctx, ids)
}

// plane is one way a credential can arrive. Every one of them must resolve every
// kind in the family -- that is the property the whole seam exists to deliver.
type plane struct {
	name string
	call func(t *testing.T, svc *Service, token string) (Principal, error)
}

func allPlanes() []plane {
	return []plane{
		{
			// netrc, `curl --netrc`, the CLI's own SetBasicAuth("bakery", token).
			name: "basic password field",
			call: func(_ *testing.T, svc *Service, tok string) (Principal, error) {
				req := httptest.NewRequest(http.MethodGet, "/cache/acme/fw/sstate/x.tgz", nil)
				req.SetBasicAuth("bakery", tok)

				return svc.AuthenticateCache(req.Context(), req)
			},
		},
		{
			// http://<token>@host -- URL-embedded credentials. A Bakery token is ONE
			// opaque string with no id:secret halves, so either field authenticates.
			name: "basic username field",
			call: func(_ *testing.T, svc *Service, tok string) (Principal, error) {
				req := httptest.NewRequest(http.MethodGet, "/cache/acme/fw/sstate/x.tgz", nil)
				req.SetBasicAuth(tok, "")

				return svc.AuthenticateCache(req.Context(), req)
			},
		},
		{
			name: "bearer",
			call: func(_ *testing.T, svc *Service, tok string) (Principal, error) {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
				req.Header.Set("Authorization", "Bearer "+tok)

				return svc.Authenticate(req.Context(), req)
			},
		},
		{
			// hashserv's in-band auth RPC: no *http.Request exists at that point, the
			// credential arrives inside a WebSocket frame after the upgrade.
			name: "hashserv in-band auth rpc",
			call: func(t *testing.T, svc *Service, tok string) (Principal, error) {
				return svc.AuthenticateToken(t.Context(), tok)
			},
		},
		{
			// The OCI backend's Authenticator is AuthenticateToken, reached only after
			// oci.credentialToken's shape gate -- which is auth.LooksLikeBakeryToken.
			// Both halves are asserted; the oci package asserts its own half again.
			name: "oci bearer",
			call: func(t *testing.T, svc *Service, tok string) (Principal, error) {
				if !LooksLikeBakeryToken(tok) {
					t.Fatalf("the OCI shape gate would discard %q before the authenticator saw it",
						tok[:min(len(tok), len(TokenPrefix))])
				}

				return svc.AuthenticateToken(t.Context(), tok)
			},
		},
	}
}

// TestEveryPlaneAcceptsEveryTokenKind is the F2 gate.
//
// The bug it exists to prevent is not exotic: AuthenticateCache selected its
// Basic field with looksLikeAPIKey (bkry_-only) and then called authenticateKey
// UNCONDITIONALLY, and AuthenticateToken -- the sole entry point for hashserv and
// the OCI backend -- was a hardcoded wrapper over authenticateKey. Ship a second
// credential kind against that and it works in a browser, works over Bearer to
// the control plane, and fails on every cache push, every registry pull and every
// hashserv connection, with a 401 that names nothing.
//
// Three kinds x five planes, all fifteen resolving to a principal reporting the
// method the token's own prefix implies.
func TestEveryPlaneAcceptsEveryTokenKind(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)

	kinds := []struct {
		name       string
		token      string
		wantMethod Method
	}{
		{"bkry_ api key", fix.apiKey, MethodAPIKey},
		{"bkru_ user token", fix.userToken, MethodUserToken},
		{"bkro_ org token", fix.orgToken, MethodOrgToken},
	}

	for _, k := range kinds {
		for _, pl := range allPlanes() {
			t.Run(k.name+"/"+pl.name, func(t *testing.T) {
				p, err := pl.call(t, fix.svc, k.token)
				if err != nil {
					t.Fatalf("%s over %s: error = %v, want a principal", k.name, pl.name, err)
				}

				if p.Method() != k.wantMethod {
					t.Errorf("Method() = %q, want %q -- the prefix, not the plane, picks the validator",
						p.Method(), k.wantMethod)
				}
			})
		}
	}

	// The three principals are not merely non-nil, they are the RIGHT SHAPE. This is
	// what the stub validators could not assert: a user token carries its owner's
	// live authority, a robot carries a grant and no identity at all.
	t.Run("shapes", func(t *testing.T) {
		pat, err := fix.svc.AuthenticateToken(t.Context(), fix.userToken)
		if err != nil {
			t.Fatalf("user token: %v", err)
		}

		if pat.UserID() != fix.userID {
			t.Error("a personal access token did not resolve to its owner")
		}

		if !pat.CanWriteProject(fix.orgID, fix.projectID) {
			t.Error("a write-scoped personal access token could not write its owner's project")
		}

		if pat.IsSiteAdmin() || pat.CanAdminOrg(fix.orgID) {
			t.Error("a personal access token administers something")
		}

		bot, err := fix.svc.AuthenticateToken(t.Context(), fix.orgToken)
		if err != nil {
			t.Fatalf("org token: %v", err)
		}

		grant, ok := bot.Robot()
		if !ok {
			t.Fatal("an org token produced a principal with no robot grant")
		}

		if grant.RobotID != fix.robotID || grant.OrgID != fix.orgID || grant.Scope != ScopeWrite {
			t.Errorf("robot grant = %+v, want {%v %v write}", grant, fix.robotID, fix.orgID)
		}

		if bot.UserID().Valid || bot.Email() != "" {
			t.Error("a robot principal carries a user identity; it has no user row to carry one from")
		}
	})
}

// TestAuthAttemptsAreLabelledByTokenKind (F11). observeErr used to hardcode
// MethodAPIKey at every token seam, so every user-token and robot failure landed
// in the api_key series -- the one an operator watches to decide whether a key is
// wrong. Two credential kinds' failures summed into a third kind's alert.
func TestAuthAttemptsAreLabelledByTokenKind(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)
	ctx := t.Context()

	// One success per kind, and one failure that belongs to no kind at all.
	if _, err := fix.svc.AuthenticateToken(ctx, fix.apiKey); err != nil {
		t.Fatalf("api key: %v", err)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, fix.userToken); err != nil {
		t.Fatalf("user token: %v", err)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, fix.orgToken); err != nil {
		t.Fatalf("org token: %v", err)
	}

	if _, err := fix.svc.AuthenticateToken(ctx, "dckr_pat_S3CRET"); err == nil {
		t.Fatal("a foreign credential authenticated")
	}

	want := map[[2]string]float64{
		{string(MethodAPIKey), "ok"}:       1,
		{string(MethodUserToken), "ok"}:    1,
		{string(MethodOrgToken), "ok"}:     1,
		{string(MethodAPIKey), "error"}:    1, // the unknown kind, deliberately (see kind.method)
		{string(MethodUserToken), "error"}: 0,
		{string(MethodOrgToken), "error"}:  0,
	}

	for labels, wantCount := range want {
		if got := authAttempts(t, fix.svc.metrics, labels[0], labels[1]); got != wantCount {
			t.Errorf("bakery_auth_attempts_total{method=%q,result=%q} = %v, want %v",
				labels[0], labels[1], got, wantCount)
		}
	}
}

// authAttempts reads one bakery_auth_attempts_total series straight off the
// registry -- there is no exported counter-reading API, and client_golang's
// testutil is not a dependency this package otherwise needs.
func authAttempts(t *testing.T, m *metrics.Metrics, method, result string) float64 {
	t.Helper()

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	for _, fam := range families {
		if fam.GetName() != "bakery_auth_attempts_total" {
			continue
		}

		for _, met := range fam.GetMetric() {
			var gotMethod, gotResult string

			for _, label := range met.GetLabel() {
				switch label.GetName() {
				case "method":
					gotMethod = label.GetValue()
				case "result":
					gotResult = label.GetValue()
				}
			}

			if gotMethod == method && gotResult == result {
				return met.GetCounter().GetValue()
			}
		}
	}

	return 0
}

// TestUserTokenNeverReachesProjectKeyProbe (R2).
//
// validateKeySQL is an Index Only Scan on api_keys_token_sha256_key and nothing
// else. A bkru_ or bkro_ token routed into it does not become a security hole --
// it misses -- but it burns a probe on the sstate HEAD storm to produce a
// guaranteed miss, and it turns "your token is for the wrong table" into an
// indistinguishable "the API key is not valid". The prefix decides, before any
// database work happens.
func TestUserTokenNeverReachesProjectKeyProbe(t *testing.T) {
	t.Parallel()

	fix := newTokenFixture(t)

	for _, tok := range []string{fix.userToken, fix.orgToken} {
		for _, pl := range allPlanes() {
			t.Run(string(tokenKind(tok))+"/"+pl.name, func(t *testing.T) {
				fix.resetProbes()

				if _, err := pl.call(t, fix.svc, tok); err != nil {
					t.Fatalf("error = %v, want a principal", err)
				}

				if fix.keys.validate != 0 {
					t.Errorf("the api_keys probe ran %d time(s) for a %q token",
						fix.keys.validate, tokenKind(tok))
				}

				// And exactly ONE probe, of the right table. Two would mean something
				// re-validated; zero would mean the plane never reached a validator at all.
				probed := map[kind]int{
					kindUserToken: fix.userTokens.validate,
					kindOrgToken:  fix.orgTokens.validate,
				}
				if got := probed[tokenKind(tok)]; got != 1 {
					t.Errorf("the %q table was probed %d time(s), want exactly 1", tokenKind(tok), got)
				}
			})
		}
	}
}

// TestForeignCredentialStillDiscarded (R3). The family gate widened; what it
// rejects, it must still reject BEFORE any validator, any probe, any metric that
// could hold the value, and any log line.
//
// Docker Engine forwards the operator's real Docker Hub login to whatever
// registry-mirrors names, unscoped, on every pull. That credential arrives here.
func TestForeignCredentialStillDiscarded(t *testing.T) {
	t.Parallel()

	foreign := []string{
		"dckr_pat_S3CRET_VALUE_xyz",
		"anonymous",
		"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.e30.sig",
		"bkr_abcdefghijklmnop",
		"BKRY_abcdefghijklmnop",
		TokenPrefix,
		UserTokenPrefix,
		OrgTokenPrefix,
	}

	fix := newTokenFixture(t)

	for _, tok := range foreign {
		t.Run(tok, func(t *testing.T) {
			if LooksLikeBakeryToken(tok) {
				t.Fatal("the family gate accepted a foreign credential; " +
					"it would reach a probe, a metric and possibly a log")
			}

			fix.resetProbes()

			if _, err := fix.svc.AuthenticateToken(t.Context(), tok); err == nil {
				t.Error("a foreign credential authenticated")
			}

			// NOT ONE PROBE OF ANY TABLE. The family gate refuses it before any
			// validator runs, which is what keeps a forwarded Docker Hub PAT out of a
			// database round trip, an error metric and a log line alike.
			if n := fix.probes(); n != 0 {
				t.Errorf("a foreign credential cost %d database probe(s)", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// No token mints a token.
// ---------------------------------------------------------------------------

// impostor is a foreign type that satisfies Principal by EMBEDDING it -- the one
// residual hole in a sealed interface (see the package doc). It gains nothing,
// and requireMintAuthority is one of the places that must be true of: it is not
// *principal, so it is not interactive, so it cannot mint.
type impostor struct{ Principal }

// TestNoTokenMintsAToken. A credential that can mint a credential is a
// self-service credential factory: a read-scoped key becomes, in two steps, a
// write credential attributed to a human who did not perform the mint.
//
// The guard is keyed on interactive(), so this table is a mirror of
// TestEveryMethodIsExplicitlyClassified's -- and stage 3's CreateUserToken and
// CreateOrgToken inherit it by calling requireMintAuthority as their first
// statement.
func TestNoTokenMintsAToken(t *testing.T) {
	t.Parallel()

	interactiveMethods := map[Method]bool{MethodSession: true, MethodBearer: true, MethodDev: true}

	for _, m := range declaredMethods(t) {
		p := &principal{
			userID: uuid(0x01), issuer: "", subject: "", email: "", displayName: "",
			method: m, siteRole: SiteRoleAdmin,
			orgs:     map[pgtype.UUID]OrgRole{orgA: OrgRoleOwner},
			projects: map[pgtype.UUID]ProjectRole{projectA: ProjectRoleAdmin},
			key:      nil, robot: nil, maxScope: ScopeWrite,
		}

		err := requireMintAuthority(p)

		if interactiveMethods[m] {
			if err != nil {
				t.Errorf("%q: requireMintAuthority = %v, want nil for an interactive principal", m, err)
			}

			continue
		}

		if err == nil {
			t.Errorf("%q: a non-interactive principal was allowed to mint a credential", m)
		}

		// And through the real minting entry point, which must refuse before it
		// touches the store (this Service has none).
		svc := &Service{}
		if _, _, kerr := svc.CreateAPIKey(t.Context(), p, CreateKeyInput{
			OrgID: orgA, ProjectID: projectA, Name: "ci", Scope: ScopeRead, ExpiresAt: nil,
		}); kerr == nil {
			t.Errorf("%q: CreateAPIKey minted a key for a non-interactive principal", m)
		}
	}

	if err := requireMintAuthority(nil); err == nil {
		t.Error("a nil principal minted a credential")
	}

	if err := requireMintAuthority(impostor{Principal: nil}); err == nil {
		t.Error("a foreign type embedding Principal minted a credential")
	}
}
