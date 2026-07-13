package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrEnumTypeMissing means the database does not carry an enum type this binary
// depends on. It is a schema/binary skew, and it is fatal: see registerEnumTypes.
var ErrEnumTypeMissing = errors.New("enum type missing from database")

// enums is EVERY Postgres enum in the schema (migration 000001_foundation).
//
// pgx cannot build an encode plan for a Go value whose target OID it has never
// seen. For a scalar enum it mostly gets away with it -- a bare `ApiKeyScope`
// encodes as text and Postgres coerces it -- but a SLICE of one cannot: the
// parameter's OID is the ARRAY oid, pgx has no codec for it, and the query dies
// with "cannot find encode plan". That is why RevokeAPIKeysForMembership
// (`scope = ANY($3::api_key_scope[])`) has never worked.
//
// Adding an enum to a migration and not to this list reintroduces that bug. The
// verification in registerEnumTypes is what turns the omission into a boot
// failure instead of a 500 on a code path nobody exercises until it matters.
var enums = []string{
	"site_role",
	"org_role",
	"project_role",
	"api_key_scope",
	"backend_kind",
	"blob_state",
	"gc_run_status",
}

// enumTypeNames pairs each enum with its array type, scalar FIRST.
//
// The array type is a genuinely separate type with its own OID -- Postgres names
// it by prefixing an underscore, so api_key_scope (16420) has _api_key_scope
// (16419). Registering the scalar does NOT register the array, and the array is
// the one `= ANY($1::api_key_scope[])` actually binds.
//
// The order matters: an array codec resolves its ELEMENT codec out of the
// connection's TypeMap, so the scalar must already be registered when the array
// is loaded. Load them the other way round and pgx returns, verbatim,
// "array element OID not registered".
func enumTypeNames() []string {
	names := make([]string, 0, len(enums)*2)
	for _, e := range enums {
		names = append(names, e, "_"+e)
	}

	return names
}

// registerEnumTypes is the pool's AfterConnect hook. It runs on EVERY connection
// the pool opens -- not just the first -- which is what makes the registration
// survive MaxConnLifetime recycling and pool growth under load.
//
// A MISSING TYPE FAILS THE CONNECTION, deliberately.
//
// conn.LoadTypes is a silent partial-success API: it returns a nil error and a
// SHORT slice for names it cannot resolve (against a database with none of these
// types it returns zero types and no error at all). Handing that short slice to
// RegisterTypes yields a connection that is quietly missing codecs, and the
// missing codec does not surface at boot -- it surfaces as "cannot find encode
// plan" from inside a query, in production, on whichever code path first passes
// an enum slice. That is precisely the bug this function exists to kill, so
// swallowing it here would be self-defeating. We ask for a set of names and we
// require the whole set.
func registerEnumTypes(ctx context.Context, conn *pgx.Conn) error {
	names := enumTypeNames()

	types, err := conn.LoadTypes(ctx, names)
	if err != nil {
		return fmt.Errorf("load enum types: %w", err)
	}

	found := make(map[string]bool, len(types))
	for _, t := range types {
		found[t.Name] = true
	}

	for _, name := range names {
		if !found[name] {
			return fmt.Errorf("%w: %q", ErrEnumTypeMissing, name)
		}
	}

	conn.TypeMap().RegisterTypes(types)

	return nil
}
