package auth

import (
	"context"
	"strings"
)

// The Bakery token family.
//
// Three credential kinds share one presentation: an opaque, prefixed, base64url
// string that arrives in an Authorization header (Bearer, or EITHER Basic field)
// or in hashserv's in-band auth RPC. The prefix is what tells them apart, and it
// is chosen so that routing is decidable from the first five characters with no
// database round trip and no ambiguity -- the three differ in one byte.
//
//	bkry_  api_keys    project-scoped, minted by a human for one project
//	bkru_  user_tokens personal access token: acts as one human, no browser
//	bkro_  org_tokens  robot: owned by an org, not by a human
//
// A prefix is not a namespace decoration. It is a SECRET-SCANNER hook (a leaked
// credential is greppable and its blast radius is readable from the string
// itself) and it is the dispatch key: a token must reach the validator for its
// own table and NO OTHER. A bkru_ token that fell into authenticateKey's
// api_keys probe would not be a security hole -- it would miss -- but it would
// be an unexplainable "my token does not work", which is worse than a refusal.
const (
	// UserTokenPrefix marks a personal access token (user_tokens).
	UserTokenPrefix = "bkru_"
	// OrgTokenPrefix marks a robot token (org_tokens).
	OrgTokenPrefix = "bkro_"
)

// kind names which table a presented credential belongs to.
type kind string

const (
	// kindUnknown is "not one of ours". It is the answer for a session cookie, an
	// OIDC ID token, a forwarded Docker Hub PAT, and every other credential that
	// arrives in the same header. Nothing probes a database for it.
	kindUnknown   kind = ""
	kindAPIKey    kind = "api_key"
	kindUserToken kind = "user_token"
	kindOrgToken  kind = "org_token"
)

// method is the Method a principal built from this kind reports, and therefore
// the `method` label its auth attempts are counted under.
//
// kindUnknown maps to MethodAPIKey deliberately: an unrecognised token reaching a
// token-only seam (hashserv's auth RPC, the OCI Bearer arm) has always been
// counted in the api_key series, and re-labelling it now would silently break
// every dashboard and alert that watches bakery_auth_attempts_total for
// credential failures. A new label value must mean a new credential kind, not a
// re-partitioning of the old one.
func (k kind) method() Method {
	switch k {
	case kindUserToken:
		return MethodUserToken
	case kindOrgToken:
		return MethodOrgToken
	case kindAPIKey, kindUnknown:
		return MethodAPIKey
	}

	return MethodAPIKey
}

// LooksLikeBakeryToken reports whether s is shaped like ANY Bakery credential.
//
// This is the FAMILY gate, and it is deliberately not the same function as
// looksLikeAPIKey. looksLikeAPIKey answers "is this an api_keys row's plaintext"
// and stays bkry_-only forever: widening it is how a bkru_ token ends up probing
// the wrong table. This one answers "is this ours at all", which is the question
// the OCI backend asks before it lets a credential travel any further into the
// process (see internal/cache/oci/authz.go): a forwarded Docker Hub PAT must be
// discarded here, before it can reach a database probe, an error metric, or a
// log line.
//
// The body check is one character, not looksLikeAPIKey's display-length floor.
// This gate decides whether a credential is EXAMINED, never whether it is
// accepted -- each validator re-applies its own shape rules -- and a stricter
// family gate would silently reclassify a malformed Bakery token as a foreign
// credential, which on an open OCI backend means "served anonymously" rather
// than "refused".
func LooksLikeBakeryToken(s string) bool {
	return tokenKind(s) != kindUnknown
}

// tokenKind is the dispatch switch. One pass, no fallthrough, no database.
func tokenKind(s string) kind {
	switch {
	case hasTokenPrefix(s, TokenPrefix):
		return kindAPIKey
	case hasTokenPrefix(s, UserTokenPrefix):
		return kindUserToken
	case hasTokenPrefix(s, OrgTokenPrefix):
		return kindOrgToken
	}

	return kindUnknown
}

func hasTokenPrefix(s, prefix string) bool {
	return strings.HasPrefix(s, prefix) && len(s) > len(prefix)
}

// tokenValidator turns one plaintext token into a Principal, or refuses it.
type tokenValidator func(ctx context.Context, token string) (Principal, error)

// authenticateBakeryToken is THE token seam: every plane that accepts a Bakery
// credential -- Basic on /cache/*, Bearer on /api/v1 and /cache/*, hashserv's
// in-band auth RPC, the OCI Bearer arm -- resolves it through this one function.
//
// One seam, because the alternative is what the code looked like before it: three
// call sites that each hardcoded authenticateKey, so teaching Bakery a new
// credential kind meant finding all three and remembering the metric label at
// each. Two of the three were missed in the design review that produced this
// change, and the one that was missed (AuthenticateToken) is the one hashserv and
// the OCI backend both depend on.
func (s *Service) authenticateBakeryToken(ctx context.Context, token string) (Principal, error) {
	k := tokenKind(token)

	p, err := s.validatorFor(k)(ctx, token)
	s.observeErr(k.method(), err)

	return p, err
}

// validatorFor picks the one validator that may see this token.
//
// The default arm returns ErrKeyInvalid WITHOUT calling any validator: an
// unrecognised credential must not cost a database round trip, and it must not be
// handed to authenticateKey "just in case", because that is precisely the probe
// the family gate exists to prevent.
func (s *Service) validatorFor(k kind) tokenValidator {
	switch k {
	case kindAPIKey:
		return s.authenticateKey

	case kindUserToken:
		return s.authenticateUserToken

	case kindOrgToken:
		return s.authenticateOrgToken

	case kindUnknown:
		return refuseToken
	}

	return refuseToken
}

func refuseToken(_ context.Context, _ string) (Principal, error) { return nil, ErrKeyInvalid }

// The `bkru_` and `bkro_` validators live in usertoken.go and orgtoken.go. They
// are Service methods with the tokenValidator signature, so validatorFor names
// them directly and there is no indirection left over from the stage that
// introduced this dispatch table before those tables existed.

// requireMintAuthority is the "no token mints a token" guard.
//
// EVERY credential-minting entry point calls it -- CreateAPIKey, CreateUserToken
// and CreateOrgToken, each as its FIRST statement. A machine credential that
// can mint another machine credential is a self-service credential factory: a
// read-scoped key for one project becomes, in two steps, a write credential for
// whatever its holder's human roles reach, and the mint is attributed to a human
// who did not perform it.
//
// It is keyed on interactive(), not on a list of forbidden methods, for the same
// reason the capability guards are: a new Method must be refused by default.
func requireMintAuthority(p Principal) error {
	if p == nil {
		return ErrUnauthenticated
	}

	if !interactive(p) {
		return ErrKeyInvalid
	}

	return nil
}
