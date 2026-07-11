// Package middleware provides the HTTP middleware chain.
package middleware

import "net/http"

// Middleware wraps an http.Handler with additional behaviour.
type Middleware func(http.Handler) http.Handler

// CreateStack composes middleware into a single Middleware. The first argument
// is the outermost wrapper, so it sees the request first and the response last.
func CreateStack(xs ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(xs) - 1; i >= 0; i-- {
			next = xs[i](next)
		}

		return next
	}
}

// Default is the middleware chain the server runs.
func Default() Middleware {
	return CreateStack(
		RequestLogger,
	)
}
