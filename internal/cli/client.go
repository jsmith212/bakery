package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/api"
	"github.com/jsmith212/bakery/internal/auth"
)

// ErrNeedsLogin is what every authentication failure becomes.
//
// The user does not want the error envelope. They want the next command to type.
// A 401 has exactly one remedy from a CLI -- re-authenticate -- so the client
// collapses "no cached token", "the token expired", "the refresh token was
// revoked" and "the server said 401" into this single, actionable sentence, and
// main prints it and nothing else.
var ErrNeedsLogin = errors.New("not signed in to this server: run bakery login")

// maxBody caps how much of a response we will read. A CLI talking to a server
// that is actually a captive portal should fail, not exhaust memory.
const maxBody = 4 << 20 // 4 MiB

// defaultTimeout bounds every API call. The device-grant poll manages its own,
// longer deadline.
const defaultTimeout = 30 * time.Second

// APIError is a structured non-2xx from /api/v1.
//
// It carries the CODE, not just the message: the code vocabulary is closed and
// stable, so a caller that wants to branch (does this org already exist?) can,
// while the message is what the user reads.
type APIError struct {
	Status  int
	Code    string
	Message string
	Field   string
}

func (e *APIError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Field)
	}

	return e.Message
}

// Client is the API client. One per server.
type Client struct {
	server string
	http   *http.Client

	// cacheHTTP is the transport for the /cache object path. It has NO total Timeout:
	// an sstate tarball is multi-GB and a 30-second cap would fail every large upload.
	// A /cache request is bounded by its context instead -- a per-HEAD deadline on the
	// probe storm, the caller's context on a streaming PUT.
	cacheHTTP *http.Client

	tokens *TokenStore

	// now and sleep are injected so the device-grant poll can be tested without
	// spending eight real seconds proving that it backs off.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	// authConfig memoizes GET /auth/config for the process lifetime. It is fetched
	// lazily, so a command that never needs to refresh never pays for it.
	authConfig *auth.AuthConfig
}

// NewClient builds a client for one server.
func NewClient(server string, tokens *TokenStore) (*Client, error) {
	server = canonicalServer(server)
	if server == "" {
		return nil, errors.New("no server: pass --server or set BAKERY_SERVER")
	}

	u, err := url.Parse(server)
	if err != nil {
		return nil, fmt.Errorf("parse the server URL %q: %w", server, err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("the server URL %q must start with http:// or https://", server)
	}

	if u.Host == "" {
		return nil, fmt.Errorf("the server URL %q has no host", server)
	}

	return &Client{
		server:     server,
		http:       &http.Client{Timeout: defaultTimeout},
		cacheHTTP:  &http.Client{},
		tokens:     tokens,
		now:        time.Now,
		sleep:      sleepCtx,
		authConfig: nil,
	}, nil
}

// Server is the canonical server URL this client talks to.
func (c *Client) Server() string { return c.server }

// sleepCtx sleeps, or returns early if the context is cancelled. A user who hits
// ctrl-c during a device-grant poll should not wait out the interval first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// AuthConfig fetches (and memoizes) the server's /auth/config.
//
// This is the whole reason the CLI needs no configuration beyond --server: the
// issuer, the client id, the scopes and the device endpoint all come from the
// server that already resolved them at boot, so the CLI never redoes OIDC
// discovery and cannot disagree with the server about which IdP it trusts.
func (c *Client) AuthConfig(ctx context.Context) (auth.AuthConfig, error) {
	if c.authConfig != nil {
		return *c.authConfig, nil
	}

	var cfg auth.AuthConfig
	if err := c.do(ctx, http.MethodGet, "/auth/config", nil, &cfg, withoutAuth); err != nil {
		return auth.AuthConfig{}, err
	}

	c.authConfig = &cfg

	return cfg, nil
}

// authMode says whether a request carries the bearer token.
type authMode bool

const (
	withAuth    authMode = true
	withoutAuth authMode = false
)

// bearer returns the ID token to present, refreshing it first if it is stale.
//
// The refresh is TRANSPARENT and happens inside the ordinary request path: a
// user running `bakery key list` an hour after logging in gets their key list,
// not a browser. That is the entire point of asking for offline_access.
func (c *Client) bearer(ctx context.Context) (string, error) {
	if c.tokens == nil {
		return "", ErrNeedsLogin
	}

	tok, ok := c.tokens.Get(c.server)
	if !ok {
		return "", ErrNeedsLogin
	}

	if !tok.Stale(c.now()) {
		return tok.IDToken, nil
	}

	if tok.RefreshToken == "" {
		return "", ErrNeedsLogin
	}

	cfg, err := c.AuthConfig(ctx)
	if err != nil {
		return "", err
	}

	fresh, err := c.refresh(ctx, cfg, tok)
	if err != nil {
		// A refresh token that the IdP will not honour is not a transient fault --
		// it was revoked, or it expired, or the session was ended centrally. There
		// is no retry that helps and no error detail the user can act on.
		return "", ErrNeedsLogin
	}

	if err := c.tokens.Put(c.server, fresh); err != nil {
		return "", err
	}

	return fresh.IDToken, nil
}

// refresh exchanges the refresh token for a new ID token.
func (c *Client) refresh(ctx context.Context, cfg auth.AuthConfig, old Token) (Token, error) {
	if cfg.TokenEndpoint == "" {
		return Token{}, errors.New("the server advertises no token endpoint")
	}

	fresh, err := c.tokenExchange(ctx, cfg.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {old.RefreshToken},
		"client_id":     {cfg.ClientID},
	})
	if err != nil {
		return Token{}, err
	}

	// Refresh-token rotation is optional: RFC 6749 lets the IdP omit a new one and
	// keep the old one valid. Dropping the old one when it does would sign the
	// user out at the NEXT refresh, an hour later, for no reason.
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = old.RefreshToken
	}

	return fresh, nil
}

// do performs one API call and decodes the result.
func (c *Client) do(ctx context.Context, method, path string, body, out any, mode authMode) error {
	var reader io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode the request body: %w", err)
		}

		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.server+api.Prefix+path, reader)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	if body != nil {
		// Not optional. The API gates state-changing methods on a JSON content type
		// as CSRF defence-in-depth and answers anything else with a 415.
		req.Header.Set("Content-Type", "application/json")
	}

	if mode == withAuth {
		token, err := c.bearer(ctx)
		if err != nil {
			return err
		}

		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", c.server, err)
	}

	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("read the response from %s: %w", c.server, err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return ErrNeedsLogin
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiErrorFrom(resp.StatusCode, raw)
	}

	if out == nil || resp.StatusCode == http.StatusNoContent || len(raw) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("parse the response from %s: %w", c.server, err)
	}

	return nil
}

// apiErrorFrom turns a non-2xx body into an APIError.
//
// The body is SUPPOSED to be the error envelope, and when it is, the user gets
// the server's sentence. When it is not -- a reverse proxy's HTML 502, a captive
// portal, a bare "not found" from a mis-typed --server pointed at something that
// is not Bakery -- we must not print the raw body at them either. Say what
// happened, in one line.
func apiErrorFrom(status int, raw []byte) error {
	var body api.ErrorBody

	if err := json.Unmarshal(raw, &body); err == nil && body.Error.Code != "" {
		return &APIError{
			Status:  status,
			Code:    body.Error.Code,
			Message: body.Error.Message,
			Field:   body.Error.Field,
		}
	}

	return &APIError{
		Status:  status,
		Code:    "",
		Message: fmt.Sprintf("the server returned %s", strings.ToLower(http.StatusText(status))),
		Field:   "",
	}
}

// seg escapes one path segment. Slugs are already constrained, but a --server
// pointed at a hostile URL and a slug typed by hand are both user input.
func seg(s string) string { return url.PathEscape(s) }

// ---------------------------------------------------------------------------
// Endpoints
//
// One method per route. They take and return the api package's own wire types,
// so the client cannot drift from the server: a field renamed in internal/api is
// a compile error here, not a silently-empty column in a table.
// ---------------------------------------------------------------------------

// Me is GET /me.
func (c *Client) Me(ctx context.Context) (api.Me, error) {
	var me api.Me
	err := c.do(ctx, http.MethodGet, "/me", nil, &me, withAuth)

	return me, err
}

// ListOrgs is GET /orgs.
func (c *Client) ListOrgs(ctx context.Context) ([]api.Org, error) {
	var out api.ListResponse[api.Org]
	err := c.do(ctx, http.MethodGet, "/orgs", nil, &out, withAuth)

	return out.Items, err
}

// CreateOrg is POST /orgs.
func (c *Client) CreateOrg(ctx context.Context, slug, name string) (api.Org, error) {
	var out api.Org
	err := c.do(ctx, http.MethodPost, "/orgs",
		api.CreateOrgRequest{Slug: slug, Name: name}, &out, withAuth)

	return out, err
}

// GetOrg is GET /orgs/{org}.
func (c *Client) GetOrg(ctx context.Context, org string) (api.Org, error) {
	var out api.Org
	err := c.do(ctx, http.MethodGet, "/orgs/"+seg(org), nil, &out, withAuth)

	return out, err
}

// RenameOrg is PATCH /orgs/{org}. The slug is immutable; only the name moves.
func (c *Client) RenameOrg(ctx context.Context, org, name string) (api.Org, error) {
	var out api.Org
	err := c.do(ctx, http.MethodPatch, "/orgs/"+seg(org),
		api.UpdateOrgRequest{Name: name}, &out, withAuth)

	return out, err
}

// DeleteOrg is DELETE /orgs/{org}.
func (c *Client) DeleteOrg(ctx context.Context, org string) error {
	return c.do(ctx, http.MethodDelete, "/orgs/"+seg(org), nil, nil, withAuth)
}

// ListProjects is GET /orgs/{org}/projects.
func (c *Client) ListProjects(ctx context.Context, org string) ([]api.Project, error) {
	var out api.ListResponse[api.Project]
	err := c.do(ctx, http.MethodGet, "/orgs/"+seg(org)+"/projects", nil, &out, withAuth)

	return out.Items, err
}

// CreateProject is POST /orgs/{org}/projects.
func (c *Client) CreateProject(ctx context.Context, org, slug, name string) (api.Project, error) {
	var out api.Project
	err := c.do(ctx, http.MethodPost, "/orgs/"+seg(org)+"/projects",
		api.CreateProjectRequest{Slug: slug, Name: name}, &out, withAuth)

	return out, err
}

// GetProject is GET /orgs/{org}/projects/{project}.
func (c *Client) GetProject(ctx context.Context, org, project string) (api.Project, error) {
	var out api.Project
	err := c.do(ctx, http.MethodGet, projectPath(org, project), nil, &out, withAuth)

	return out, err
}

// RenameProject is PATCH /orgs/{org}/projects/{project}.
func (c *Client) RenameProject(ctx context.Context, org, project, name string) (api.Project, error) {
	var out api.Project
	err := c.do(ctx, http.MethodPatch, projectPath(org, project),
		api.UpdateProjectRequest{Name: name}, &out, withAuth)

	return out, err
}

// DeleteProject is DELETE /orgs/{org}/projects/{project}.
func (c *Client) DeleteProject(ctx context.Context, org, project string) error {
	return c.do(ctx, http.MethodDelete, projectPath(org, project), nil, nil, withAuth)
}

// ListOrgMembers is GET /orgs/{org}/members. The role it reports is the EFFECTIVE
// one: greatest(oidc_role, local_role).
//
// There is no Set/Remove counterpart HERE, but since M1.5 the API has them
// (PUT/DELETE /orgs/{org}/members/{user}, which write the local half). The CLI has
// not grown the verbs yet; the console is the surface for them.
func (c *Client) ListOrgMembers(ctx context.Context, org string) ([]api.Member, error) {
	var out api.ListResponse[api.Member]
	err := c.do(ctx, http.MethodGet, "/orgs/"+seg(org)+"/members", nil, &out, withAuth)

	return out.Items, err
}

// ListProjectMembers is GET /orgs/{org}/projects/{project}/members.
func (c *Client) ListProjectMembers(ctx context.Context, org, project string) ([]api.Member, error) {
	var out api.ListResponse[api.Member]
	err := c.do(ctx, http.MethodGet, projectPath(org, project)+"/members", nil, &out, withAuth)

	return out.Items, err
}

// SetProjectMember is PUT /orgs/{org}/projects/{project}/members/{user}. The
// user may be a uuid or an email.
func (c *Client) SetProjectMember(ctx context.Context, org, project, user, role string) (api.Member, error) {
	var out api.Member
	err := c.do(ctx, http.MethodPut, projectPath(org, project)+"/members/"+seg(user),
		api.PutProjectMemberRequest{Role: role}, &out, withAuth)

	return out, err
}

// RemoveProjectMember is DELETE /orgs/{org}/projects/{project}/members/{user}.
func (c *Client) RemoveProjectMember(ctx context.Context, org, project, user string) error {
	return c.do(ctx, http.MethodDelete,
		projectPath(org, project)+"/members/"+seg(user), nil, nil, withAuth)
}

// ListKeys is GET /orgs/{org}/projects/{project}/keys. Metadata only: this
// response type has no field a token could live in.
func (c *Client) ListKeys(ctx context.Context, org, project string) ([]api.APIKey, error) {
	var out api.ListResponse[api.APIKey]
	err := c.do(ctx, http.MethodGet, projectPath(org, project)+"/keys", nil, &out, withAuth)

	return out.Items, err
}

// CreateKey is POST /orgs/{org}/projects/{project}/keys. The ONLY call in the
// whole client whose response carries a secret.
func (c *Client) CreateKey(
	ctx context.Context, org, project, name, scope string, expiresAt *time.Time,
) (api.CreatedAPIKey, error) {
	var out api.CreatedAPIKey
	err := c.do(ctx, http.MethodPost, projectPath(org, project)+"/keys",
		api.CreateKeyRequest{Name: name, Scope: scope, ExpiresAt: expiresAt}, &out, withAuth)

	return out, err
}

// DeleteKey is DELETE /orgs/{org}/projects/{project}/keys/{key}.
func (c *Client) DeleteKey(ctx context.Context, org, project, key string) error {
	return c.do(ctx, http.MethodDelete,
		projectPath(org, project)+"/keys/"+seg(key), nil, nil, withAuth)
}

func projectPath(org, project string) string {
	return "/orgs/" + seg(org) + "/projects/" + seg(project)
}
