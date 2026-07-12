// Package slug validates the org and project slugs that make up the cache URL
// grammar.
//
// This is a MIRROR, not the authority. The authority is the database: every
// organizations.slug and projects.slug carries a CHECK that calls the IMMUTABLE
// SQL function bakery_slug_ok (internal/db/migrations/000001_foundation.up.sql),
// so a bad slug is refused no matter which writer proposes it -- the API, the dev
// seeder, a migration, or a psql session.
//
// This package exists only so the API can render a friendly 400 instead of
// surfacing a 23514. internal/db's TestSlugMirrorsDatabase asserts, against a
// real Postgres, that the two agree on every case in its table -- so a drift
// between this file and the migration is a failing test, not a production
// surprise.
package slug

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalid is returned for a slug that does not match the grammar.
var ErrInvalid = errors.New("invalid slug")

// ErrReserved is returned for a well-formed slug that collides with the routing
// grammar.
var ErrReserved = errors.New("reserved slug")

// Pattern is the slug grammar, byte-for-byte the regex in bakery_slug_ok:
// lowercase alphanumerics and hyphens, 1-63 characters, no leading or trailing
// hyphen.
var Pattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reserved is the routing denylist, byte-for-byte the ARRAY in bakery_slug_ok.
//
// Note `actionresults`, lowercase. The spec spells it `actionResults`, but the
// grammar above forbids uppercase, so that exact string is already
// unrepresentable and reserving it verbatim would be a no-op -- while
// `actionresults`, the string a user CAN create, would slip through.
var reserved = []string{
	"blobs", "uploads", "actions", "actionresults", "operations",
	"capabilities", "compressed-blobs", "ac", "cas", "v2", "api", "cache",
}

// Reserved returns the routing denylist. The slice is a copy; the list is not
// tunable policy and nothing may mutate it.
func Reserved() []string {
	return slices.Clone(reserved)
}

// IsReserved reports whether s collides with the routing grammar.
func IsReserved(s string) bool {
	return slices.Contains(reserved, s)
}

// Valid reports whether s is an acceptable org or project slug -- i.e. whether
// bakery_slug_ok(s) would return true.
func Valid(s string) bool {
	return Pattern.MatchString(s) && !IsReserved(s)
}

// Check returns nil for an acceptable slug, and an error wrapping ErrInvalid or
// ErrReserved otherwise. The message is safe to hand to a user.
func Check(s string) error {
	if !Pattern.MatchString(s) {
		return fmt.Errorf(
			"%w: %q must be 1-63 characters of lowercase letters, digits and hyphens, "+
				"starting and ending with a letter or digit",
			ErrInvalid, s,
		)
	}

	if IsReserved(s) {
		return fmt.Errorf("%w: %q is reserved by the cache URL grammar (reserved: %s)",
			ErrReserved, s, strings.Join(reserved, ", "))
	}

	return nil
}
