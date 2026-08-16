package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/auth"
)

// ---------------------------------------------------------------------------
// TestWireTypesMatchTSFixtures (R9#7).
//
// Until this file, web/src/lib/api/testdata's response fixtures existed only to
// drive Vitest -- nothing on the Go side ever read them, so a JSON tag rename in
// internal/api (or internal/auth's AuthConfig) was invisible here: `go build`
// stays green, `go test ./internal/api/...` stays green, and only Vitest --
// which most Go-side changes never trigger a run of -- would eventually notice
// the drift, if a fixture happened to be updated at all.
//
// This test marshals a POPULATED value of each wire type the fixtures cover and
// asserts its top-level JSON key set equals the fixture's. A rename of, say,
// Member.OrgRole's tag from `org_role` to `orgRole` makes `got` stop containing
// `org_role` while the fixture still does, and the test fails -- in `go test`,
// with no Vitest run required.
//
// # Locating the fixtures: runtime.Caller, not go:embed
//
// A go:embed directive cannot reach web/src/lib/api/testdata from here: an embed pattern is
// confined to the package directory the directive lives in and everything
// beneath it, and testdata sits three directories outside internal/api entirely
// (../../web/src/lib/api/testdata). So this walks the filesystem at TEST time
// instead: runtime.Caller(0) gives this source file's own path exactly as the
// toolchain placed it on disk, and repoRoot walks upward from there looking for
// go.mod -- which works under `go test`, under `just test`, and under any CI
// checkout, because all three run this file from its real location in the
// module, never from a temp copy.
//
// # Why the key set, not full equality against the fixture's VALUES
//
// The fixtures' rows deliberately vary which OPTIONAL fields they carry, to
// exercise different provenance shapes on the TS side (a claim-derived member,
// a locally-granted one, both at once). No single row is "the" contract; the
// UNION of keys across every row in a fixture is. And a marshaled Go value's
// KEY set is exactly what a JSON tag rename changes -- the values themselves
// are this file's own fixtures, not the wire's, and asserting them would just
// duplicate the struct literal below in prose.
//
// # Known gaps, not this test's job to close
//
// Two fields exist on their Go type but are exercised by NO fixture row, so
// the populated values below deliberately leave them at their zero value
// (omitempty then drops them, matching the fixture): Member.ProjectRole (no
// fixture carries a project's OWN member-list response, only an org's) and
// SnippetResponse.Files (no fixture exercises the docker/OCI snippet tool,
// which is the one that populates it). Populating either here would make this
// test fail against fixtures that are simply incomplete, not wrong -- widening
// the fixtures is a separate, TS-side change.
// ---------------------------------------------------------------------------

// repoRoot walks up from this test file's own path (via runtime.Caller) to the
// directory containing go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate this test file to find the repo root")
	}

	dir := filepath.Dir(file)

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("walked up from %s to the filesystem root without finding go.mod", file)
		}

		dir = parent
	}
}

// readTestdata reads one fixture from web/src/lib/api/testdata.
func readTestdata(t *testing.T, filename string) []byte {
	t.Helper()

	path := filepath.Join(repoRoot(t), "web", "src", "lib", "api", "testdata", filename)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return raw
}

// objectKeySet returns the sorted, de-duplicated top-level key set of a JSON
// object.
func objectKeySet(t *testing.T, raw []byte) []string {
	t.Helper()

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal as a JSON object: %v\n%s", err, raw)
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// listFixtureKeySet reads a `{"items":[...]}` fixture and returns the UNION of
// every item's top-level key set -- see the package doc above for why a union,
// not one row.
func listFixtureKeySet(t *testing.T, filename string) []string {
	t.Helper()

	raw := readTestdata(t, filename)

	var envelope struct {
		Items []json.RawMessage `json:"items"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal %s as a list envelope: %v", filename, err)
	}

	if len(envelope.Items) == 0 {
		t.Fatalf("%s has no items; nothing to union", filename)
	}

	union := map[string]struct{}{}

	for _, item := range envelope.Items {
		for _, k := range objectKeySet(t, item) {
			union[k] = struct{}{}
		}
	}

	keys := make([]string, 0, len(union))
	for k := range union {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// nestedTokenKeySet returns the union of top-level key sets from a robots-list
// fixture's `items[].tokens[]` -- the only place OrgToken's shape appears on the
// wire outside CreatedOrgToken, since there is no standalone list route for a
// robot's tokens (see robots.go: they ride along inside GET .../robots).
func nestedTokenKeySet(t *testing.T, filename string) []string {
	t.Helper()

	raw := readTestdata(t, filename)

	var envelope struct {
		Items []struct {
			Tokens []json.RawMessage `json:"tokens"`
		} `json:"items"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal %s as a robots envelope: %v", filename, err)
	}

	union := map[string]struct{}{}

	for _, item := range envelope.Items {
		for _, tok := range item.Tokens {
			for _, k := range objectKeySet(t, tok) {
				union[k] = struct{}{}
			}
		}
	}

	if len(union) == 0 {
		t.Fatalf("%s has no tokens; nothing to union", filename)
	}

	keys := make([]string, 0, len(union))
	for k := range union {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// unionKeySet merges the top-level key sets of several fixtures -- SnippetResponse
// needs both snippet-preview.json (local_conf/netrc/push_commands populated,
// env/api_key/warnings absent) and snippet-minted.json (the reverse) to see every
// key the type carries.
func unionKeySet(sets ...[]string) []string {
	union := map[string]struct{}{}

	for _, set := range sets {
		for _, k := range set {
			union[k] = struct{}{}
		}
	}

	keys := make([]string, 0, len(union))
	for k := range union {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// marshaledKeySet marshals v and returns its top-level JSON key set.
func marshaledKeySet(t *testing.T, v any) []string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}

	return objectKeySet(t, raw)
}

// assertSameKeySet is the test body every case below shares: a Go tag rename,
// addition or removal shows up here as an asymmetric diff.
func assertSameKeySet(t *testing.T, typeName string, got, want []string) {
	t.Helper()

	if slices.Equal(got, want) {
		return
	}

	var onlyInGo, onlyInFixture []string

	wantSet := make(map[string]struct{}, len(want))
	for _, k := range want {
		wantSet[k] = struct{}{}
	}

	gotSet := make(map[string]struct{}, len(got))
	for _, k := range got {
		gotSet[k] = struct{}{}
	}

	for _, k := range got {
		if _, ok := wantSet[k]; !ok {
			onlyInGo = append(onlyInGo, k)
		}
	}

	for _, k := range want {
		if _, ok := gotSet[k]; !ok {
			onlyInFixture = append(onlyInFixture, k)
		}
	}

	t.Errorf("%s: marshaled key set does not match the TS fixture's.\n"+
		"  only in the Go struct:      %v\n"+
		"  only in the TS fixture:     %v\n"+
		"A field present on only one side is either a Go json tag that drifted from "+
		"web/src/lib/api/types.ts, or a fixture that needs updating to match a "+
		"deliberate wire change.", typeName, onlyInGo, onlyInFixture)
}

func TestWireTypesMatchTSFixtures(t *testing.T) {
	now := time.Now().UTC()
	then := now.Add(-24 * time.Hour)

	t.Run("Member", func(t *testing.T) {
		// Member.ProjectRole is a known gap -- see the package doc above -- so it is
		// deliberately left unset here.
		member := Member{
			UserID: "u1", Email: "anna@acme.dev", DisplayName: "Anna Roux",
			OrgRole: "admin", OIDCRole: "admin", OIDCGroup: "cn=bakery-admins",
			LocalRole: "owner", GrantedBy: "u2", GrantedByEmail: "bo@acme.dev",
			GrantedAt: &then, Source: OrgRoleSourceBoth,
		}

		assertSameKeySet(t, "Member", marshaledKeySet(t, member), listFixtureKeySet(t, "org-members.json"))
	})

	t.Run("SiteAdmin", func(t *testing.T) {
		admin := SiteAdmin{
			UserID: "u1", Email: "ops@bakery.internal", DisplayName: "Ops",
			SiteRole: "admin", OIDCRole: "admin", OIDCGroup: "cn=platform-admins",
			LocalRole: "admin", GrantedBy: "u2", GrantedByEmail: "bo@acme.dev",
			GrantedAt: &then, Source: SiteRoleSourceBoth,
		}

		assertSameKeySet(t, "SiteAdmin", marshaledKeySet(t, admin), listFixtureKeySet(t, "site-admins.json"))
	})

	t.Run("Org", func(t *testing.T) {
		window := "2160h0m0s"
		quota := int64(549755813888)

		org := Org{
			ID: "o1", Slug: "acme", Name: "Acme", Role: "admin",
			DefaultRetentionWindow: &window, DefaultQuotaBytes: &quota,
			CreatedAt: then, UpdatedAt: now,
		}

		assertSameKeySet(t, "Org", marshaledKeySet(t, org), listFixtureKeySet(t, "orgs.json"))
	})

	t.Run("Project", func(t *testing.T) {
		project := Project{
			ID: "p1", OrgID: "o1", OrgSlug: "acme", Slug: "firmware", Name: "Firmware",
			Role: "admin", Backends: []string{"sstate", "downloads"},
			CreatedAt: then, UpdatedAt: now,
		}

		assertSameKeySet(t, "Project", marshaledKeySet(t, project), listFixtureKeySet(t, "projects.json"))
	})

	t.Run("Backend", func(t *testing.T) {
		window := "2160h0m0s"
		quota := int64(549755813888)

		backend := Backend{
			ID: 41, ProjectID: "p1", Kind: "sstate", Enabled: true, ReadAuthRequired: true,
			Config: json.RawMessage(`{}`), RetentionWindow: &window, QuotaBytes: &quota,
			CreatedAt: then, UpdatedAt: now,
		}

		assertSameKeySet(t, "Backend", marshaledKeySet(t, backend), listFixtureKeySet(t, "backends.json"))
	})

	t.Run("APIKey", func(t *testing.T) {
		key := populatedAPIKey()

		assertSameKeySet(t, "APIKey", marshaledKeySet(t, key), listFixtureKeySet(t, "keys.json"))
	})

	t.Run("CreatedAPIKey", func(t *testing.T) {
		key := CreatedAPIKey{APIKey: populatedAPIKey(), Token: "bkry_" + "0123456789abcdef"}

		// created-key.json is a BARE object (the single response to POST
		// /keys), not a `{"items":[...]}` envelope like keys.json.
		assertSameKeySet(t, "CreatedAPIKey", marshaledKeySet(t, key), objectKeySet(t, readTestdata(t, "created-key.json")))
	})

	t.Run("Me", func(t *testing.T) {
		// Me.APIKey is a known gap -- no fixture covers an API-key-authenticated
		// /me response -- so it is deliberately left nil (omitempty drops it).
		// Me.Robot is NOT a gap: me-robot.json covers it, and the field is
		// populated below even though a real robot response would never also
		// carry a display name -- this test asserts the WIRE SHAPE, not realism,
		// same as the Member subtest combining provenance that never co-occurs.
		me := Me{
			UserID: "u1", Email: "anna@acme.dev", DisplayName: "Anna Roux",
			AvatarURL: "https://lh3.googleusercontent.com/a/anna-roux",
			Method:    "session", SiteRole: "user", IsSiteAdmin: false,
			Orgs:     []MeOrg{{ID: "o1", Slug: "acme", Name: "Acme", Role: "admin"}},
			Projects: []MeProject{{ID: "p1", Slug: "firmware", OrgSlug: "acme", Role: "admin"}},
			Robot:    &MeRobotGrant{RobotID: "r1", OrgID: "o1", Scope: "write"},
		}

		// me.json and me-site-admin.json carry the SAME top-level key set (a site
		// admin's orgs/projects are empty arrays, not absent keys) -- either alone
		// is the whole contract, and unioning both documents that equivalence
		// rather than assuming it. me-robot.json adds the one key neither carries:
		// `robot`.
		want := unionKeySet(
			objectKeySet(t, readTestdata(t, "me.json")),
			objectKeySet(t, readTestdata(t, "me-site-admin.json")),
			objectKeySet(t, readTestdata(t, "me-robot.json")),
		)

		assertSameKeySet(t, "Me", marshaledKeySet(t, me), want)
	})

	t.Run("UserToken", func(t *testing.T) {
		tok := UserToken{
			ID: "t1", Name: "build-host-1", TokenPrefix: "bkru_9d8c7b6a", MaxScope: "write",
			CreatedAt: then, ExpiresAt: &now, LastUsedAt: &now, RevokedAt: &now,
		}

		assertSameKeySet(t, "UserToken", marshaledKeySet(t, tok), listFixtureKeySet(t, "user-tokens.json"))
	})

	t.Run("CreatedUserToken", func(t *testing.T) {
		tok := CreatedUserToken{
			UserToken: UserToken{
				ID: "t1", Name: "yocto-2026-08-16", TokenPrefix: "bkru_5e6f7081", MaxScope: "write",
				CreatedAt: then, ExpiresAt: &now,
			},
			Token: "bkru_" + "0123456789abcdef",
		}

		// created-user-token.json is a BARE object (the single response to POST
		// /user/tokens), not a `{"items":[...]}` envelope like user-tokens.json.
		assertSameKeySet(t, "CreatedUserToken",
			marshaledKeySet(t, tok), objectKeySet(t, readTestdata(t, "created-user-token.json")))
	})

	t.Run("Robot", func(t *testing.T) {
		robot := Robot{
			ID: "r1", OrgID: "o1", Name: "ci-runner", Description: "CI fleet cache",
			CreatedBy: "u1", CreatedByEmail: "anna@acme.dev", CreatedAt: then,
			Tokens: []OrgToken{populatedOrgToken()},
		}

		assertSameKeySet(t, "Robot", marshaledKeySet(t, robot), listFixtureKeySet(t, "robots.json"))
	})

	t.Run("OrgToken", func(t *testing.T) {
		// robots.json is the ONLY fixture carrying OrgToken's shape without the
		// `token` field -- there is no standalone GET .../tokens list route, only
		// the tokens NESTED inside each robot in the list response.
		assertSameKeySet(t, "OrgToken",
			marshaledKeySet(t, populatedOrgToken()), nestedTokenKeySet(t, "robots.json"))
	})

	t.Run("CreatedOrgToken", func(t *testing.T) {
		tok := CreatedOrgToken{OrgToken: populatedOrgToken(), Token: "bkro_" + "0123456789abcdef"}

		assertSameKeySet(t, "CreatedOrgToken",
			marshaledKeySet(t, tok), objectKeySet(t, readTestdata(t, "created-org-token.json")))
	})

	t.Run("GCRun", func(t *testing.T) {
		run := GCRun{
			ID: 812, Status: "failed", Trigger: "api", DryRun: false,
			StartedAt: then, FinishedAt: &now, Error: "sweep sstate: context deadline exceeded",
			ObjectsDeleted: 0, HashservRowsDeleted: 0, BlobsMarked: 0, BlobsDeleted: 0, BytesReclaimed: 0,
		}

		assertSameKeySet(t, "GCRun", marshaledKeySet(t, run), listFixtureKeySet(t, "gc-runs.json"))
	})

	t.Run("OrgProjectUsage", func(t *testing.T) {
		usage := OrgProjectUsage{
			ProjectSlug: "firmware", ObjectsCount: 184201, LogicalBytes: 228076715213,
			MeasuredAt: &now,
		}

		assertSameKeySet(t, "OrgProjectUsage", marshaledKeySet(t, usage), listFixtureKeySet(t, "org-usage.json"))
	})

	t.Run("ProjectBackendUsage", func(t *testing.T) {
		objects := int64(184201)
		bytes := int64(228076715213)
		quota := int64(549755813888)
		window := "2160h0m0s"

		usage := ProjectBackendUsage{
			Kind: "sstate", ObjectsCount: &objects, LogicalBytes: &bytes, MeasuredAt: &now,
			QuotaBytes: &quota, RetentionWindow: &window,
		}

		assertSameKeySet(t, "ProjectBackendUsage", marshaledKeySet(t, usage), listFixtureKeySet(t, "project-usage.json"))
	})

	t.Run("InstanceInfo", func(t *testing.T) {
		info := InstanceInfo{
			Version: "0.6.0", StorageDriver: "local", PublicAddr: ":8080",
			MetricsAddr: "127.0.0.1:9090", GRPCAddr: ":9092", ExternalURL: "https://bakery.corp",
			OIDCIssuer: "https://id.acme.dev/realms/main", DevLoginEnabled: false,
			GRPCExternalEndpoint: "grpcs://bakery.corp:9092",
			AllowSelfServeOrgs:   true, AllowLocalSiteAdmins: true, AllowMultiInstance: false,
			GCEnabled: true, GCInterval: "6h0m0s", GCUsageInterval: "6h0m0s", GCGracePeriod: "24h0m0s",
		}

		assertSameKeySet(t, "InstanceInfo", marshaledKeySet(t, info), objectKeySet(t, readTestdata(t, "instance.json")))
	})

	t.Run("SnippetResponse", func(t *testing.T) {
		// SnippetResponse.Files is a known gap -- see the package doc above -- so it
		// is deliberately left unset here.
		key := populatedAPIKey()
		created := CreatedAPIKey{APIKey: key, Token: "bkry_" + "0123456789abcdef"}

		resp := SnippetResponse{
			Tool: "sccache", Host: "bakery.corp", BaseURL: "https://bakery.corp/cache/acme/firmware",
			LocalConf: "", Netrc: "", PushCommands: []string{},
			Env:      []SnippetEnvVar{{Name: "SCCACHE_WEBDAV_TOKEN", Value: created.Token}},
			APIKey:   &created,
			Preview:  false,
			Warnings: []string{"The Bakery credential is one opaque bkry_ token, not a key-id and key-secret pair."},
		}

		want := unionKeySet(
			objectKeySet(t, readTestdata(t, "snippet-preview.json")),
			objectKeySet(t, readTestdata(t, "snippet-minted.json")),
		)

		assertSameKeySet(t, "SnippetResponse", marshaledKeySet(t, resp), want)
	})

	t.Run("AuthConfig", func(t *testing.T) {
		cfg := auth.AuthConfig{
			Issuer: "https://id.acme.dev/realms/main", ClientID: "bakery-console",
			Scopes:                      []string{"openid", "profile", "email", "groups"},
			AuthorizationEndpoint:       "https://id.acme.dev/realms/main/protocol/openid-connect/auth",
			TokenEndpoint:               "https://id.acme.dev/realms/main/protocol/openid-connect/token",
			DeviceAuthorizationEndpoint: "https://id.acme.dev/realms/main/protocol/openid-connect/auth/device",
			OIDCEnabled:                 true, DevLoginEnabled: false, AllowSelfServeOrgs: true,
		}

		assertSameKeySet(t, "AuthConfig", marshaledKeySet(t, cfg), objectKeySet(t, readTestdata(t, "auth-config.json")))
	})
}

// populatedOrgToken is the shared fully-populated OrgToken literal the Robot,
// OrgToken and CreatedOrgToken subtests all need.
func populatedOrgToken() OrgToken {
	now := time.Now().UTC()

	return OrgToken{
		ID: "t1", RobotID: "r1", OrgID: "o1", Name: "ci-2026",
		TokenPrefix: "bkro_2c3d4e5f", Scope: "write", ExpiresAt: now.Add(365 * 24 * time.Hour),
		CreatedBy: "u1", CreatedByEmail: "anna@acme.dev", CreatedAt: now,
		LastUsedAt: &now, RevokedAt: &now,
	}
}

// populatedAPIKey is the shared fully-populated APIKey literal CreatedAPIKey and
// SnippetResponse's api_key field also need.
func populatedAPIKey() APIKey {
	now := time.Now().UTC()

	return APIKey{
		ID: "k1", Name: "ci-runner", ProjectID: "p1", TokenPrefix: "bkry_7f3a91c2",
		Scope: "write", OwnerID: "u1", OwnerEmail: "anna@acme.dev", OwnerName: "Anna Roux",
		CreatedAt: now, ExpiresAt: &now, LastUsedAt: &now, RevokedAt: &now,
	}
}
