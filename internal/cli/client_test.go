package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsmith212/bakery/internal/api"
)

// ---------------------------------------------------------------------------
// A stand-in /api/v1.
//
// It is a real net/http server speaking the REAL wire types -- every response
// below is an api.Org, an api.ListResponse[api.APIKey], an api.ErrorBody. A field
// renamed in internal/api is a compile error in this file, so these tests cannot
// drift into asserting a shape the server no longer serves. (The end-to-end test
// in integration_test.go goes further and drives the actual api.Handler over a
// real Postgres; this one exists to cover the paths that a live server cannot be
// made to produce on demand -- a 401 on a request that DID carry a token, a 502
// from a proxy in front, a body that is not the envelope at all.)
// ---------------------------------------------------------------------------

type recordedRequest struct {
	method      string
	path        string
	auth        string
	contentType string
	body        string
}

type fakeAPI struct {
	t *testing.T

	server *httptest.Server

	// handler is consulted for every /api/v1 request; a nil return falls through
	// to a 404.
	handler func(w http.ResponseWriter, r *http.Request) bool

	requests []recordedRequest
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	f := &fakeAPI{t: t, server: nil, handler: nil, requests: nil}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)

		f.requests = append(f.requests, recordedRequest{
			method:      r.Method,
			path:        strings.TrimPrefix(r.URL.Path, api.Prefix),
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})

		if f.handler != nil && f.handler(w, r) {
			return
		}

		writeErr(w, http.StatusNotFound, api.CodeNotFound, "not found")
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeAPI) last() recordedRequest {
	f.t.Helper()

	if len(f.requests) == 0 {
		f.t.Fatal("no request was made")
	}

	return f.requests[len(f.requests)-1]
}

// client builds a Client against the fake, with a fresh token already cached.
func (f *fakeAPI) client(t *testing.T) *Client {
	t.Helper()

	store, _ := tempStore(t)

	c, err := NewClient(f.server.URL, store)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if err := store.Put(c.Server(), Token{
		IDToken: fakeJWT(t, time.Now().Add(time.Hour)),
		Expiry:  time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	return c
}

func readAll(t *testing.T, r *http.Request) string {
	t.Helper()

	if r.Body == nil {
		return ""
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read the request body: %v", err)
	}

	return string(raw)
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSONResp(w, status, api.ErrorBody{
		Error: api.ErrorDetail{Code: code, Message: msg, Field: ""},
	})
}

// ---------------------------------------------------------------------------
// The verbs
// ---------------------------------------------------------------------------

// TestClientVerbs drives every CRUD method and asserts the method, the path and
// the request body it puts on the wire. A CLI whose `org rename` quietly issued a
// PUT, or whose `member set` addressed /members?user=, would still "work" against
// a mock -- it is the exact request that is the contract.
func TestClientVerbs(t *testing.T) {
	created := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name string

		// respond serves the one call this case makes.
		respond func(w http.ResponseWriter, r *http.Request) bool

		call func(ctx context.Context, c *Client) error

		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{
			name: "whoami",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusOK, api.Me{
					UserID: "u1", Email: "dev@bakery.local", DisplayName: "Dev",
					Method: "bearer", SiteRole: "admin", IsSiteAdmin: true,
					Orgs: []api.MeOrg{{ID: "o1", Slug: "acme", Name: "Acme", Role: "owner"}},
					Projects: []api.MeProject{
						{ID: "p1", Slug: "widgets", OrgSlug: "acme", Role: "admin"},
					},
					APIKey: nil,
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				me, err := c.Me(ctx)
				if err != nil {
					return err
				}

				if me.Email != "dev@bakery.local" || len(me.Orgs) != 1 {
					return errors.New("decoded /me does not match what the server sent")
				}

				return nil
			},
			wantMethod: http.MethodGet,
			wantPath:   "/me",
			wantBody:   "",
		},
		{
			name: "org list",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusOK, api.ListResponse[api.Org]{
					Items: []api.Org{
						{
							ID: "o1", Slug: "acme", Name: "Acme", Role: "owner",
							CreatedAt: created, UpdatedAt: created,
						},
					},
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				orgs, err := c.ListOrgs(ctx)
				if err != nil {
					return err
				}

				if len(orgs) != 1 || orgs[0].Slug != "acme" {
					return errors.New("org list did not round-trip")
				}

				return nil
			},
			wantMethod: http.MethodGet,
			wantPath:   "/orgs",
			wantBody:   "",
		},
		{
			name: "org create",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusCreated, api.Org{
					ID: "o1", Slug: "acme", Name: "Acme Inc", Role: "owner",
					CreatedAt: created, UpdatedAt: created,
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.CreateOrg(ctx, "acme", "Acme Inc")

				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/orgs",
			wantBody:   `{"slug":"acme","name":"Acme Inc"}`,
		},
		{
			name: "org rename is a PATCH of the name alone",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusOK, api.Org{
					ID: "o1", Slug: "acme", Name: "Acme Ltd", Role: "owner",
					CreatedAt: created, UpdatedAt: created,
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.RenameOrg(ctx, "acme", "Acme Ltd")

				return err
			},
			wantMethod: http.MethodPatch,
			wantPath:   "/orgs/acme",
			wantBody:   `{"name":"Acme Ltd"}`,
		},
		{
			name: "org delete",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(http.StatusNoContent)

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				return c.DeleteOrg(ctx, "acme")
			},
			wantMethod: http.MethodDelete,
			wantPath:   "/orgs/acme",
			wantBody:   "",
		},
		{
			name: "project create",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusCreated, api.Project{
					ID: "p1", OrgID: "o1", OrgSlug: "acme", Slug: "widgets",
					Name: "Widgets", Role: "admin", Backends: []string{},
					CreatedAt: created, UpdatedAt: created,
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				_, err := c.CreateProject(ctx, "acme", "widgets", "Widgets")

				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/orgs/acme/projects",
			wantBody:   `{"slug":"widgets","name":"Widgets"}`,
		},
		{
			name: "project show",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusOK, api.Project{
					ID: "p1", OrgID: "o1", OrgSlug: "acme", Slug: "widgets",
					Name: "Widgets", Role: "admin", Backends: []string{"sstate"},
					CreatedAt: created, UpdatedAt: created,
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				p, err := c.GetProject(ctx, "acme", "widgets")
				if err != nil {
					return err
				}

				if len(p.Backends) != 1 || p.Backends[0] != "sstate" {
					return errors.New("backends did not round-trip")
				}

				return nil
			},
			wantMethod: http.MethodGet,
			wantPath:   "/orgs/acme/projects/widgets",
			wantBody:   "",
		},
		{
			name: "member set addresses the user in the path",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusOK, api.Member{
					UserID: "u2", Email: "sam@example.com", DisplayName: "Sam",
					OrgRole: "member", ProjectRole: "writer", Source: api.OrgRoleSourceOIDC,
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				m, err := c.SetProjectMember(ctx, "acme", "widgets", "sam@example.com", "writer")
				if err != nil {
					return err
				}

				if m.ProjectRole != "writer" {
					return errors.New("project role did not round-trip")
				}

				return nil
			},
			wantMethod: http.MethodPut,
			// The email is a path segment, and it is escaped: an address with a
			// `/` or a `#` in it must not rewrite the route.
			wantPath: "/orgs/acme/projects/widgets/members/sam@example.com",
			wantBody: `{"role":"writer"}`,
		},
		{
			name: "member remove",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(http.StatusNoContent)

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				return c.RemoveProjectMember(ctx, "acme", "widgets", "sam@example.com")
			},
			wantMethod: http.MethodDelete,
			wantPath:   "/orgs/acme/projects/widgets/members/sam@example.com",
			wantBody:   "",
		},
		{
			name: "key list carries no token",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSONResp(w, http.StatusOK, api.ListResponse[api.APIKey]{
					Items: []api.APIKey{{
						ID: "k1", Name: "ci", ProjectID: "p1", TokenPrefix: "bkry_abc12345",
						Scope: "write", OwnerID: "u1", OwnerEmail: "dev@bakery.local",
						OwnerName: "Dev", CreatedAt: created,
						ExpiresAt: nil, LastUsedAt: nil, RevokedAt: nil,
					}},
				})

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				keys, err := c.ListKeys(ctx, "acme", "widgets")
				if err != nil {
					return err
				}

				if len(keys) != 1 || keys[0].TokenPrefix != "bkry_abc12345" {
					return errors.New("key list did not round-trip")
				}

				return nil
			},
			wantMethod: http.MethodGet,
			wantPath:   "/orgs/acme/projects/widgets/keys",
			wantBody:   "",
		},
		{
			name: "key revoke",
			respond: func(w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(http.StatusNoContent)

				return true
			},
			call: func(ctx context.Context, c *Client) error {
				return c.DeleteKey(ctx, "acme", "widgets", "k1")
			},
			wantMethod: http.MethodDelete,
			wantPath:   "/orgs/acme/projects/widgets/keys/k1",
			wantBody:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.handler = tc.respond

			c := f.client(t)

			if err := tc.call(t.Context(), c); err != nil {
				t.Fatalf("call: %v", err)
			}

			got := f.last()

			if got.method != tc.wantMethod {
				t.Errorf("method = %s, want %s", got.method, tc.wantMethod)
			}

			if got.path != tc.wantPath {
				t.Errorf("path = %s, want %s", got.path, tc.wantPath)
			}

			if body := strings.TrimSpace(got.body); body != tc.wantBody {
				t.Errorf("body = %s, want %s", body, tc.wantBody)
			}

			if !strings.HasPrefix(got.auth, "Bearer ") {
				t.Errorf("Authorization = %q, want a Bearer token", got.auth)
			}

			// The API gates state-changing methods on a JSON content type as CSRF
			// defence-in-depth and answers anything else with a 415. A client that
			// forgot the header would fail against a real server and pass against a
			// lenient mock, so assert it here.
			if tc.wantBody != "" && got.contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got.contentType)
			}
		})
	}
}

// TestKeyCreateSendsNoUserID: keys are per-user, and the CLI must never be able
// to mint one FOR someone else. The request body carries a name, a scope and an
// optional expiry, and nothing that names a user.
func TestKeyCreateSendsNoUserID(t *testing.T) {
	f := newFakeAPI(t)
	f.handler = func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSONResp(w, http.StatusCreated, api.CreatedAPIKey{
			APIKey: api.APIKey{
				ID: "k1", Name: "ci", ProjectID: "p1", TokenPrefix: "bkry_abc12345",
				Scope: "write", OwnerID: "u1", OwnerEmail: "dev@bakery.local", OwnerName: "Dev",
				CreatedAt: time.Now(), ExpiresAt: nil, LastUsedAt: nil, RevokedAt: nil,
			},
			Token: "bkry_" + strings.Repeat("x", 43),
		})

		return true
	}

	c := f.client(t)

	key, err := c.CreateKey(t.Context(), "acme", "widgets", "ci", "write", nil)
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	if key.Token == "" {
		t.Error("the created key carried no token")
	}

	body := f.last().body

	for _, forbidden := range []string{"user_id", "owner_id", "user"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the create-key body contains %q: %s", forbidden, body)
		}
	}
}

// TestClientErrors is the "real errors" requirement.
//
// The user gets a sentence they can act on. In particular a 401 becomes "run
// bakery login" -- not the JSON envelope, not "unexpected status 401".
func TestClientErrors(t *testing.T) {
	tests := []struct {
		name string

		status int
		body   string

		wantNeedsLogin bool
		wantCode       string
		wantMessage    string
	}{
		{
			name:   "401 tells the user what to do about it",
			status: http.StatusUnauthorized,
			body: `{"error":{"code":"unauthorized",` +
				`"message":"the request carried no credential"}}`,
			wantNeedsLogin: true,
			wantCode:       "",
			wantMessage:    "not signed in to this server: run bakery login",
		},
		{
			name:           "403 keeps the server's sentence",
			status:         http.StatusForbidden,
			body:           `{"error":{"code":"forbidden","message":"you may not administer this org"}}`,
			wantNeedsLogin: false,
			wantCode:       api.CodeForbidden,
			wantMessage:    "you may not administer this org",
		},
		{
			name:           "409 keeps the code so a caller can branch",
			status:         http.StatusConflict,
			body:           `{"error":{"code":"conflict","message":"that slug is taken"}}`,
			wantNeedsLogin: false,
			wantCode:       api.CodeConflict,
			wantMessage:    "that slug is taken",
		},
		{
			name:   "422 names the field",
			status: http.StatusUnprocessableEntity,
			body: `{"error":{"code":"reserved_slug",` +
				`"message":"that slug is reserved","field":"slug"}}`,
			wantNeedsLogin: false,
			wantCode:       api.CodeReservedSlug,
			wantMessage:    "that slug is reserved (slug)",
		},
		{
			// A reverse proxy in front of a dead Bakery, or a --server pointed at
			// something that is not Bakery at all. There is no envelope to read, and
			// printing the raw HTML at the user helps nobody.
			name:           "a non-envelope body does not become the message",
			status:         http.StatusBadGateway,
			body:           "<html><body><h1>502 Bad Gateway</h1></body></html>",
			wantNeedsLogin: false,
			wantCode:       "",
			wantMessage:    "the server returned bad gateway",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.handler = func(w http.ResponseWriter, _ *http.Request) bool {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))

				return true
			}

			c := f.client(t)

			_, err := c.ListOrgs(t.Context())
			if err == nil {
				t.Fatal("want an error, got none")
			}

			if got := errors.Is(err, ErrNeedsLogin); got != tc.wantNeedsLogin {
				t.Errorf("errors.Is(err, ErrNeedsLogin) = %v, want %v (err = %v)",
					got, tc.wantNeedsLogin, err)
			}

			if err.Error() != tc.wantMessage {
				t.Errorf("message = %q, want %q", err.Error(), tc.wantMessage)
			}

			if tc.wantCode != "" {
				var ae *APIError
				if !errors.As(err, &ae) {
					t.Fatalf("want an *APIError, got %T", err)
				}

				if ae.Code != tc.wantCode {
					t.Errorf("code = %q, want %q", ae.Code, tc.wantCode)
				}

				if ae.Status != tc.status {
					t.Errorf("status = %d, want %d", ae.Status, tc.status)
				}
			}
		})
	}
}

// TestNoCachedTokenNeverReachesTheServer: with nothing cached, `bakery whoami`
// must say "run bakery login" without making a request. Firing an anonymous
// request first would earn a 401 and arrive at the same message the slow way.
func TestNoCachedTokenNeverReachesTheServer(t *testing.T) {
	f := newFakeAPI(t)

	store, _ := tempStore(t)

	c, err := NewClient(f.server.URL, store)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Me(t.Context())
	if !errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("err = %v, want ErrNeedsLogin", err)
	}

	if len(f.requests) != 0 {
		t.Errorf("made %d request(s) with no token cached, want 0", len(f.requests))
	}
}

func TestNewClientRejectsABadServer(t *testing.T) {
	tests := []struct {
		name   string
		server string
	}{
		{name: "empty", server: ""},
		{name: "no scheme", server: "bakery.example.com"},
		{name: "not http", server: "ftp://bakery.example.com"},
		{name: "no host", server: "http://"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewClient(tc.server, nil); err == nil {
				t.Errorf("NewClient(%q) succeeded, want an error", tc.server)
			}
		})
	}
}
