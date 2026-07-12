package auth

import (
	"context"
	"net/http"
)

// ctxKey is an unexported type, so no other package can collide with our key --
// nor plant a value under it. Combined with Principal being unimplementable
// outside this package, a Principal in the context is necessarily one this
// package verified and put there.
type ctxKey struct{}

// withPrincipal is unexported ON PURPOSE. If it were exported, any package could
// stuff a value into the context -- but note it still could not FORGE one,
// because it would have no way to obtain a Principal to stuff. The unexported
// form simply removes the question.
func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the verified Principal for the request, if there is one.
//
// The second return is the whole contract. An unauthenticated request yields
// (nil, false) -- there is no zero-value Principal that reads as "some valid
// user", because Principal is an interface and its zero value is nil, and a nil
// interface has no roles to accidentally honor. Callers that ignore ok and use
// the Principal will panic on the first method call: loud, and fail-closed.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	if !ok || p == nil {
		return nil, false
	}

	return p, true
}

// FromRequest is FromContext for an *http.Request.
func FromRequest(r *http.Request) (Principal, bool) {
	return FromContext(r.Context())
}
