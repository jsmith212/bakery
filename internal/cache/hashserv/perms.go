package hashserv

import (
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/cache"
)

// Principal is the NARROW capability surface hashserv needs. Consumer-side on purpose:
// auth.Principal (sealed, unforgeable) satisfies it structurally, so this package never
// imports auth's concrete type, and a test can drive every authorization branch with a fake
// that answers exactly these two questions.
//
// Two questions is not an oversimplification -- it is the whole truth. An API-key principal
// is deliberately hollow: IsSiteAdmin() and every org/project-ADMIN capability are hard-false
// for MethodAPIKey, so CanReadProject and CanWriteProject are the only things a cache
// credential can ever answer yes to.
type Principal interface {
	CanReadProject(orgID, projectID pgtype.UUID) bool
	CanWriteProject(orgID, projectID pgtype.UUID) bool
}

// perms is hashserv's permission set, as a bitmask.
//
// Upstream's vocabulary is @none/@read/@report/@db-admin/@user-admin/@all, and its
// DEFAULT_ANON_PERMS grants an anonymous stranger @read + @report + @db-admin -- which means
// a stranger can delete your hash equivalence. Upstream's own CLI epilog concedes the
// defaults "are not particularly secure". We do not copy them, and there is no anon-perms
// knob to copy them back with.
type perms uint8

const (
	// permRead: get, get-stream, exists-stream, get-outhash. Granted to a read- or
	// write-scoped key -- or to ANYONE when the backend has read_auth_required = false,
	// which is the open-mirror case.
	permRead perms = 1 << iota

	// permReport: the write path of report / report-equiv. Requires a WRITE-SCOPED KEY,
	// always. An unauthenticated write is a cache-poisoning vector and is not a state this
	// server can be talked into: there is no configuration that grants permReport to an
	// anonymous connection.
	permReport

	// permDBAdmin: `remove`, and nothing else -- Bakery serves none of upstream's other
	// db-admin RPCs. It is project-scoped: a key can only ever purge the project it was
	// minted against.
	permDBAdmin
)

func (p perms) has(want perms) bool { return p&want == want }

// grant computes what a connection may do.
//
// The anonymous case is the interesting one. When read_auth_required = false the connection
// gets permRead and NOTHING ELSE -- notably not permReport. It does not get an error either:
// a `report` from a connection holding permRead but not permReport takes upstream's
// report_readonly path, which looks the output up, returns the stored-or-echoed unihash, and
// never writes. That is a graceful degradation and it is the correct behavior for an open
// mirror, but it is silent, so the handler meters it.
func grant(p Principal, route cache.Route) perms {
	if p == nil {
		if route.ReadAuthRequired {
			return 0
		}

		return permRead
	}

	var granted perms

	if p.CanReadProject(route.OrgID, route.ProjectID) {
		granted |= permRead
	}

	// A write-scoped key implies read: it can already rewrite what it can see, so withholding
	// read from it would be theatre.
	if p.CanWriteProject(route.OrgID, route.ProjectID) {
		granted |= permRead | permReport | permDBAdmin
	}

	// A credential that authenticated but grants nothing on THIS project still gets whatever
	// an anonymous caller would get -- being logged in must never be worse than being nobody.
	if granted == 0 && !route.ReadAuthRequired {
		granted = permRead
	}

	return granted
}
