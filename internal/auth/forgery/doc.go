// Package forgery exists to be REJECTED by the compiler.
//
// It contains no code in a normal build. Behind the `principal_forgery` build
// tag it holds every way we could think of to manufacture an auth.Principal from
// outside internal/auth, and TestPrincipalIsUnforgeable compiles it with that tag
// and asserts the build FAILS.
//
// This is the executable form of the invariant "auth.Principal is constructible
// only inside internal/auth". A comment saying so can rot; a test that asks the
// type checker cannot. If someone later exports the principal struct or drops the
// sealed method "to make the API layer easier", this package starts compiling and
// the test goes red.
//
// The stakes: the OCI upstream fetch (M5) takes a Principal. If one can be forged,
// Bakery becomes an open relay serving Docker Hub with our rate-limit-bearing
// credentials.
package forgery
