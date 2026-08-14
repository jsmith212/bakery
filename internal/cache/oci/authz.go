package oci

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/cache"
)

// anonymousToken is the literal token a registry client replays after an anonymous
// token dance. It is a SENTINEL and must be short-circuited before any verifier sees
// it -- see credentialToken.
const anonymousToken = "anonymous"

// Principal is the NARROW capability surface this backend needs. Consumer-side, so
// auth.Principal (sealed, unforgeable, no exported constructor) satisfies it
// structurally and this package never imports auth's concrete identity type.
//
// CanWriteProject is here even though the proxy has NO client-facing write path,
// because the write question is what the UPSTREAM leg is gated on -- see upstream.go.
type Principal interface {
	CanReadProject(orgID, projectID pgtype.UUID) bool
	CanWriteProject(orgID, projectID pgtype.UUID) bool
}

// Authenticator turns ONE opaque token into a Principal.
//
// It deliberately takes a TOKEN, not an *http.Request, which is the opposite of what
// httpblob's Authenticator does -- and the difference is a security property, not a
// style choice. Docker Engine forwards the USER'S REAL DOCKER HUB CREDENTIALS to
// whatever it has configured as a registry mirror, unchanged and on every pull (it has
// no per-mirror credential scoping). If this backend handed the raw request to
// auth.AuthenticateCache, every one of those Hub PATs would be run through our
// credential validation path -- a database probe, error metrics, and an audit trail --
// on the hot path of every layer pull, for credentials that were never meant for us.
//
// Taking a token means the SHAPE CHECK happens here, in this package, before anything
// leaves it: a credential that is not bkry_-shaped is never passed on, never probed,
// never counted, and never logged. See credentialToken.
type Authenticator interface {
	AuthenticateToken(ctx context.Context, token string) (Principal, error)
}

// credentialToken extracts a Bakery credential from the request, or ok=false.
//
// EVERYTHING IT REJECTS, IT REJECTS WITHOUT A ROUND TRIP. Three kinds of credential
// arrive on this route and only one of them is ours:
//
//  1. `Authorization: Bearer anonymous`. Every client that completes the anonymous
//     token dance replays this literal string on EVERY manifest and blob request. Left
//     to fall through, it reaches auth's Bearer arm, which treats a non-bkry_ token as
//     an OIDC ID token and asks the identity provider to verify it -- per request, on
//     the blob hot path, with an auth-failure metric each time. It is short-circuited
//     first, by name.
//  2. A real Docker Hub PAT, forwarded by Docker Engine as Basic. Shape-checked out
//     here; see Authenticator.
//  3. An actual bkry_ token, in a Bearer header, or in EITHER Basic field -- a Bakery
//     credential is ONE opaque token with no id:secret halves, so a client that puts
//     the whole thing in the username authenticates exactly as one that puts it in the
//     password. Password first, mirroring auth.AuthenticateCache.
//
// The returned token is never logged by this package, and no error path echoes it.
func credentialToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}

	scheme, rest, ok := strings.Cut(header, " ")
	if !ok {
		return "", false
	}

	rest = strings.TrimSpace(rest)

	switch {
	case strings.EqualFold(scheme, "bearer"):
		// The sentinel, checked before the shape test purely for legibility: it would
		// fail the bkry_ test anyway, and the point of naming it is that a reader knows
		// this branch is load-bearing rather than defensive.
		if rest == anonymousToken {
			return "", false
		}

		if isBakeryToken(rest) {
			return rest, true
		}

	case strings.EqualFold(scheme, "basic"):
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return "", false
		}

		user, pass, _ := strings.Cut(string(raw), ":")

		if isBakeryToken(pass) {
			return pass, true
		}

		if isBakeryToken(user) {
			return user, true
		}
	}

	return "", false
}

// isBakeryToken is the shape gate that keeps a foreign credential from ever costing a
// database round trip -- or appearing anywhere we could accidentally record it.
func isBakeryToken(s string) bool {
	return strings.HasPrefix(s, auth.TokenPrefix) && len(s) > len(auth.TokenPrefix)
}

// authorize resolves the caller's identity for a read.
//
// ROUTE BEFORE AUTH is already done by the caller (an unconfigured backend is 404 to
// everyone, including an anonymous caller, and it must not be possible to learn which
// projects exist by watching for a 401). What is left is one branch:
//
//   - ReadAuthRequired backend: a credential is REQUIRED. No credential, an invalid
//     one, or one that does not admit this project, are all one answer -- 401 with the
//     Bearer challenge. Never 403: a 403 is a project-existence oracle, and it is also
//     the code that makes some clients stop retrying rather than fall back.
//   - Open backend: whatever credential we can resolve, or ANONYMOUS. An
//     unrecognized credential is NOT an error here, and that is a deliberate product
//     decision with a verified cause: Docker Engine sends the user's Docker Hub
//     credentials to its mirror on every pull, so rejecting an unrecognized credential
//     would break every docker-login'd engine on earth while looking, in a code review,
//     exactly like correct security.
//
// A nil Principal is the anonymous caller, and it is meaningful downstream: no
// principal means no upstream fetch, structurally (see upstream.go). An anonymous
// caller is served from cache and gets a 404 on a miss.
func (b *Backend) authorize(w http.ResponseWriter, r *http.Request, route cache.Route) (Principal, bool) {
	token, ok := credentialToken(r)

	var principal Principal

	if ok {
		if p, err := b.authn.AuthenticateToken(r.Context(), token); err == nil {
			principal = p
		}
	}

	if !route.ReadAuthRequired {
		// OPEN BACKEND: a principal that does not admit THIS route is downgraded to
		// anonymous, not passed through. The nil-Principal gate is this package's whole
		// open-relay defence (no principal -> no upstream fetch, structurally), so
		// returning a valid-but-foreign credential here -- a bkry_ key minted for a
		// DIFFERENT project -- would hand any authenticated tenant an upstream-fetch
		// ticket on every open backend in the installation: misses would burn the
		// operator's Docker Hub rate limit on behalf of a tenant with no rights to this
		// backend, invisibly, until Hub 429s the whole deployment.
		if principal != nil && !principal.CanReadProject(route.OrgID, route.ProjectID) {
			principal = nil
		}

		return principal, true
	}

	if principal == nil || !principal.CanReadProject(route.OrgID, route.ProjectID) {
		b.challenge(w, r, route)
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "authentication required")

		return nil, false
	}

	return principal, true
}
