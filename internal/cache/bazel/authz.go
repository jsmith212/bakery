package bazel

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/jsmith212/bakery/internal/auth"
	"github.com/jsmith212/bakery/internal/cache"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// errNoCredential is internal: authorize turns it (and any authenticate failure)
// into a single Unauthenticated status, so an attacker cannot tell "no header" from
// "bad token" from "unknown key".
var errNoCredential = errors.New("bazel: no credential presented")

// authorize is called at the top of ALL EIGHT gRPC handlers. It is deliberately NOT
// an interceptor: route resolution must run BEFORE authentication (an unknown or
// disabled backend must be NotFound even to an anonymous caller, the gRPC form of
// the sstate unconfigured-backend 404), and an interceptor cannot see ByteStream's
// resource_name, which arrives in the first frame.
//
// Order: resolve the route FIRST (NotFound before Unauthenticated), then authenticate
// ONLY if the route demands it. Writes ALWAYS need a write-scoped key -- there is
// deliberately no WriteAuthRequired on cache.Route, because an unauthenticated write
// is a cache-poisoning vector that must not be representable.
func (b *Backend) authorize(ctx context.Context, instance string, write bool) (cache.Route, error) {
	org, project, ok := splitInstance(instance)
	if !ok {
		return cache.Route{}, grpcstatus.Error(codes.NotFound, "instance not found")
	}

	route, ok := b.routes.Resolve(ctx, org, project, repository.BackendKindBazel)
	if !ok || !route.Enabled {
		return cache.Route{}, grpcstatus.Error(codes.NotFound, "instance not found")
	}

	// A read against an open mirror needs no credential at all.
	if !write && !route.ReadAuthRequired {
		return route, nil
	}

	principal, err := b.authenticate(ctx)
	if err != nil {
		return cache.Route{}, grpcstatus.Error(codes.Unauthenticated, "missing or invalid credential")
	}

	if write {
		if !principal.CanWriteProject(route.OrgID, route.ProjectID) {
			// Authenticated but not write-scoped: PermissionDenied, distinct from the
			// Unauthenticated a missing/invalid credential gets.
			return cache.Route{}, grpcstatus.Error(codes.PermissionDenied, "write requires a write-scoped key")
		}

		return route, nil
	}

	if !principal.CanReadProject(route.OrgID, route.ProjectID) {
		// A read on a private mirror that the credential does not admit collapses to
		// Unauthenticated, never PermissionDenied -- the same "reads never expose an
		// authorization oracle" posture the HTTP read path takes with 401.
		return cache.Route{}, grpcstatus.Error(codes.Unauthenticated, "missing or invalid credential")
	}

	return route, nil
}

// splitInstance requires instance_name to be exactly two non-empty slug segments --
// "{org}/{project}". Anything else is NotFound (via authorize), indistinguishable
// from a project that does not exist.
func splitInstance(instance string) (org, project string, ok bool) {
	org, project, found := strings.Cut(instance, "/")
	if !found || org == "" || project == "" || strings.Contains(project, "/") {
		return "", "", false
	}

	return org, project, true
}

// authenticate reads the credential from gRPC metadata and resolves it to a
// Principal. It accepts Bearer <bkry_...> AND Basic <b64>; for Basic it offers BOTH
// decoded fields as candidates, PASSWORD FIRST, exactly as auth.AuthenticateCache
// does -- a Bakery credential is ONE opaque token, so it may sit in either field and
// there is no id:secret to split. A non-token shape never reaches the database.
func (b *Backend) authenticate(ctx context.Context) (Principal, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, errNoCredential
	}

	candidates := credentialCandidates(md.Get("authorization"))
	if len(candidates) == 0 {
		return nil, errNoCredential
	}

	var err error

	for _, token := range candidates {
		var p Principal

		if p, err = b.authn.AuthenticateToken(ctx, token); err == nil {
			return p, nil
		}
	}

	return nil, err
}

// credentialCandidates extracts the bkry_-shaped tokens from the "authorization"
// metadata values. The shape check (auth.TokenPrefix) is what keeps a non-token from
// costing a database round trip.
func credentialCandidates(headers []string) []string {
	out := make([]string, 0, 2)

	add := func(field string) {
		if strings.HasPrefix(field, auth.TokenPrefix) {
			out = append(out, field)
		}
	}

	for _, h := range headers {
		scheme, rest, ok := strings.Cut(h, " ")
		if !ok {
			// No scheme prefix: a bare `authorization: bkry_...`. add() shape-checks it
			// with auth.TokenPrefix, so a non-token value costs nothing. A Bakery
			// credential is ONE opaque token; a client that sends it schemeless still
			// authenticates rather than silently running with the cache disabled.
			add(strings.TrimSpace(h))

			continue
		}

		switch {
		case strings.EqualFold(scheme, "bearer"):
			add(strings.TrimSpace(rest))

		case strings.EqualFold(scheme, "basic"):
			raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
			if err != nil {
				continue
			}

			user, pass, _ := strings.Cut(string(raw), ":")
			// PASSWORD FIRST, then username -- the snippet generator puts the whole
			// token in both fields, and password-first mirrors AuthenticateCache.
			add(pass)
			add(user)
		}
	}

	return out
}
