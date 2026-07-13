package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// The /cache request path.
//
// It is deliberately SEPARATE from do (client.go): do targets /api/v1, forces an
// application/json content type as CSRF defence-in-depth, and decodes a JSON envelope.
// A cache object is an opaque byte stream at /cache/{org}/{project}/{kind}/{key}, so it
// shares only the server URL, the credential and the connection pool -- nothing else.

// cacheCredential is how a /cache request authenticates.
//
// A non-empty Key is a bkry_ API key, presented as HTTP Basic in the password field --
// the EXACT credential and mechanism BitBake uses for reads, so a green push also
// proves the read credential. An empty Key falls back to the logged-in device-grant
// session's OIDC bearer, which authenticateBearer verifies on the /cache path with no
// session or cookie: the default flow needs no API key at all.
type cacheCredential struct {
	Key string
}

// authorize sets the credential on a /cache request. An empty Key refreshes and
// presents the cached ID token; a failure to do so is ErrNeedsLogin, surfaced by the
// caller as the actionable "run bakery login".
func (c *Client) authorize(ctx context.Context, req *http.Request, cred cacheCredential) error {
	if cred.Key != "" {
		// The bkry_ token carries no ':' , so it rides unambiguously in the Basic
		// password field. The username is a fixed non-secret label the server ignores.
		req.SetBasicAuth("bakery", cred.Key)

		return nil
	}

	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+token)

	return nil
}

// cacheURL builds the object URL. key is the DECODED cache key (for sstate it carries
// slashes and colons); url.URL escapes each path byte per RFC 3986, preserving the
// structural slashes and the sstate colons that the server decodes right back.
func (c *Client) cacheURL(kind, org, project, key string) (string, error) {
	base, err := url.Parse(c.server)
	if err != nil {
		return "", fmt.Errorf("parse the server URL %q: %w", c.server, err)
	}

	base.Path = "/cache/" + org + "/" + project + "/" + kind + "/" + key

	return base.String(), nil
}

// cacheHeadTimeout bounds one HEAD probe. The HEAD phase is a fast metadata storm; a
// PUT streams a multi-GB body and is bounded by the caller's context instead.
const cacheHeadTimeout = 60 * time.Second

// head probes one object. It returns the HTTP status; a 200 is a hit (already present)
// and a 404 is a miss (upload it). The body is drained and discarded -- a HEAD has none
// and the status is the whole answer.
func (c *Client) head(ctx context.Context, kind, org, project, key string, cred cacheCredential) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, cacheHeadTimeout)
	defer cancel()

	u, err := c.cacheURL(kind, org, project, key)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return 0, fmt.Errorf("build the request: %w", err)
	}

	if err := c.authorize(ctx, req, cred); err != nil {
		return 0, err
	}

	resp, err := c.cacheHTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("HEAD %s: %w", c.server, err)
	}

	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, nil
}

// put streams body to one object. size is the Content-Length; a *os.File yields it from
// Stat, so the request is not chunked and the body is never buffered whole. The caller
// owns body and closes it.
func (c *Client) put(
	ctx context.Context, kind, org, project, key string, body io.Reader, size int64, cred cacheCredential,
) (int, error) {
	u, err := c.cacheURL(kind, org, project, key)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return 0, fmt.Errorf("build the request: %w", err)
	}

	// ContentLength lets net/http send a framed body rather than chunked, and lets the
	// server (and any proxy) reject an over-size upload before it starts.
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	if err := c.authorize(ctx, req, cred); err != nil {
		return 0, err
	}

	resp, err := c.cacheHTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("PUT %s: %w", c.server, err)
	}

	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))

	return resp.StatusCode, nil
}
