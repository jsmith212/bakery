package hashserv

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// Queries is the CONSUMER-SIDE database surface hashserv needs: the hashserv tables and
// nothing else. *db.Store satisfies it (it embeds *repository.Queries).
//
// It is declared here, and passed to the backend at construction, rather than being reached
// through cache.Deps -- because Deps DELIBERATELY carries no *repository.Queries, and that
// absence is what enforces "blob.Service is the only writer of object metadata"
// (see the package doc on internal/cache). hashserv is the one backend that does not route
// through blob.Service at all: it writes hash rows, never object metadata, so it needs its
// own door. Narrowing that door to exactly these methods is what keeps the rule intact --
// nothing reachable from here can touch cache_objects or blobs.
type Queries interface {
	GetUnihash(ctx context.Context, arg repository.GetUnihashParams) (string, error)
	GetUnihashFull(ctx context.Context, arg repository.GetUnihashFullParams) (repository.GetUnihashFullRow, error)
	InsertUnihash(ctx context.Context, arg repository.InsertUnihashParams) (int64, error)
	InsertOuthash(ctx context.Context, arg repository.InsertOuthashParams) (int64, error)
	GetEquivalentForOuthash(
		ctx context.Context, arg repository.GetEquivalentForOuthashParams,
	) (repository.GetEquivalentForOuthashRow, error)
	GetOuthash(ctx context.Context, arg repository.GetOuthashParams) (repository.GetOuthashRow, error)
	GetOuthashWithUnihash(
		ctx context.Context, arg repository.GetOuthashWithUnihashParams,
	) (repository.GetOuthashWithUnihashRow, error)
	UnihashExists(ctx context.Context, arg repository.UnihashExistsParams) (bool, error)

	RemoveUnihashesByTaskhash(ctx context.Context, arg repository.RemoveUnihashesByTaskhashParams) (int64, error)
	RemoveOuthashesByTaskhash(ctx context.Context, arg repository.RemoveOuthashesByTaskhashParams) (int64, error)
	RemoveUnihashesByUnihash(ctx context.Context, arg repository.RemoveUnihashesByUnihashParams) (int64, error)
	RemoveOuthashesByOuthash(ctx context.Context, arg repository.RemoveOuthashesByOuthashParams) (int64, error)
	RemoveUnihashesByMethod(ctx context.Context, arg repository.RemoveUnihashesByMethodParams) (int64, error)
	RemoveOuthashesByMethod(ctx context.Context, arg repository.RemoveOuthashesByMethodParams) (int64, error)
}

// upstreamLookup is the read side of an upstream hashserv, as the equivalence algorithm
// needs it. *Upstream (upstream.go) satisfies it. It is an interface so that "no upstream
// configured" is simply a nil, and so the algorithm can be tested without a network.
type upstreamLookup interface {
	GetUnihash(ctx context.Context, method, taskhash string) (string, bool, error)
	GetOuthash(ctx context.Context, method, outhash, taskhash string) (*outhashResponse, bool, error)
	UnihashExists(ctx context.Context, unihash string) (bool, error)
}

// backfillWriter adapts the hashserv queries to upstream.go's backfillStore, which takes the
// backendID per call because one worker may serve several backends.
type backfillWriter struct{ q Queries }

func (w backfillWriter) InsertUnihash(ctx context.Context, backendID int64, method, taskhash, unihash string) error {
	_, err := store{q: w.q, backendID: backendID}.insertUnihash(ctx, method, taskhash, unihash)

	return err
}

// store binds the queries to one backend. Every method is scoped by backendID -- that is the
// multi-tenancy boundary, and it is a parameter of every single query rather than a filter
// someone can forget to apply.
type store struct {
	q         Queries
	backendID int64
}

// text renders an optional wire field as a nullable column. A client omits keys it does not
// have; an absent PN is NULL, not "".
func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// getUnihash resolves (method, taskhash) -> unihash. A miss is ORDINARY -- it is simply a
// task nobody has reported yet, and on a cold cache it is the answer to every single query
// in the setscene graph -- so it is (,"" false, nil), never an error.
func (s store) getUnihash(ctx context.Context, method, taskhash string) (string, bool, error) {
	unihash, err := s.q.GetUnihash(ctx, repository.GetUnihashParams{
		BackendID: s.backendID,
		Method:    method,
		Taskhash:  taskhash,
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("hashserv: get unihash: %w", err)
	}

	return unihash, true, nil
}

// unihashExists backs exists-stream. Note it is NOT scoped by method: the client asks "does
// this unihash exist at all", because it is deciding whether an sstate object named after it
// could be there.
func (s store) unihashExists(ctx context.Context, unihash string) (bool, error) {
	ok, err := s.q.UnihashExists(ctx, repository.UnihashExistsParams{
		BackendID: s.backendID,
		Unihash:   unihash,
	})
	if err != nil {
		return false, fmt.Errorf("hashserv: unihash exists: %w", err)
	}

	return ok, nil
}

// insertUnihash writes (method, taskhash) -> unihash, WRITE-ONCE.
//
// ON CONFLICT DO NOTHING is the whole contract: once a taskhash has a unihash it never
// changes, not by a re-report, not by a hostile report, not ever. Downstream tasks have
// already baked that unihash into their own taskhashes and into sstate object filenames;
// changing it would strand every one of them.
func (s store) insertUnihash(ctx context.Context, method, taskhash, unihash string) (bool, error) {
	n, err := s.q.InsertUnihash(ctx, repository.InsertUnihashParams{
		BackendID: s.backendID,
		Method:    method,
		Taskhash:  taskhash,
		Unihash:   unihash,
	})
	if err != nil {
		return false, fmt.Errorf("hashserv: insert unihash: %w", err)
	}

	return n == 1, nil
}

// report is the core algorithm, and the reason the whole system exists. It reproduces
// hashserv's handle_report exactly; the steps are numbered to match spec §6.
//
// The payoff: if a recipe's inputs change in a way that does not change its OUTPUT (a
// whitespace-only edit in a dependency), the taskhash changes but the outhash does not. We
// map the new taskhash onto the EXISTING unihash, so every downstream task keeps its old
// unihash and its old sstate objects stay valid -- and the rebuild stops propagating. That
// is the entire value of hash equivalence.
func (s store) report(ctx context.Context, req reportRequest, up upstreamLookup) (unihashResponse, bool, error) {
	// (3) Insert the outhash row. `newRow` is THE GATE for everything below: equivalence is
	// computed ONLY when the outhash row is new, so a duplicate report -- which is most
	// reports, on a warm cache -- is a cheap no-op lookup and not a scan.
	newRow, err := s.insertOuthash(ctx, req)
	if err != nil {
		return unihashResponse{}, false, err
	}

	equivalent := false

	if newRow {
		unihash, adopted, err := s.resolveUnihash(ctx, req, up)
		if err != nil {
			return unihashResponse{}, false, err
		}

		equivalent = adopted

		if _, err := s.insertUnihash(ctx, req.Method, req.Taskhash, unihash); err != nil {
			return unihashResponse{}, false, err
		}
	}

	// (4) ALWAYS re-read. This is not defensive coding and it is not only about concurrency
	// -- it is load-bearing for the ordinary sequential case, and upstream's
	// test_diverging_report_race is precisely the test that catches its absence.
	//
	// Consider a task that is non-deterministic, so it reports a SECOND, different outhash
	// under a taskhash that already has a unihash. The new outhash row is new, no equivalent
	// exists for it, so the algorithm "mints" the client's unihash -- but insertUnihash is
	// write-once and no-ops, because that taskhash is already mapped. Returning the minted
	// value would hand back a unihash we did not store. Only the re-read returns what is
	// actually true.
	stored, ok, err := s.getUnihash(ctx, req.Method, req.Taskhash)
	if err != nil {
		return unihashResponse{}, false, err
	}

	if !ok {
		// Unreachable on the write path (we just inserted, or it already existed), but a
		// concurrent `remove` can delete the row between the insert and the re-read. Echo
		// the client's own unihash rather than inventing one: that is what a server with no
		// knowledge does, and it leaves the build correct if unoptimized.
		stored = req.Unihash
	}

	return unihashResponse{Taskhash: req.Taskhash, Method: req.Method, Unihash: stored}, equivalent, nil
}

// insertOuthash writes the outhash row and reports whether it was NEW.
func (s store) insertOuthash(ctx context.Context, req reportRequest) (bool, error) {
	n, err := s.q.InsertOuthash(ctx, repository.InsertOuthashParams{
		BackendID:      s.backendID,
		Method:         req.Method,
		Taskhash:       req.Taskhash,
		Outhash:        req.Outhash,
		Owner:          text(req.Owner),
		Pn:             text(req.PN),
		Pv:             text(req.PV),
		Pr:             text(req.PR),
		Task:           text(req.Task),
		OuthashSiginfo: text(req.OuthashSiginfo),
	})
	if err != nil {
		return false, fmt.Errorf("hashserv: insert outhash: %w", err)
	}

	return n == 1, nil
}

// resolveUnihash decides which unihash a brand-new outhash row should carry. It reports
// whether the answer was ADOPTED from an existing equivalent task (as opposed to minted),
// because that -- and only that -- is the moment the system earned its keep.
func (s store) resolveUnihash(
	ctx context.Context, req reportRequest, up upstreamLookup,
) (unihash string, adopted bool, err error) {
	// (3a) THE EQUIVALENCE. Find a DIFFERENT taskhash that produced the same output. Its
	// unihash wins. Oldest first -- which is what makes unihashes stable over time, and why
	// created_at has to be recorded and roughly monotonic.
	row, err := s.q.GetEquivalentForOuthash(ctx, repository.GetEquivalentForOuthashParams{
		BackendID: s.backendID,
		Method:    req.Method,
		Outhash:   req.Outhash,
		Taskhash:  req.Taskhash,
	})

	switch {
	case err == nil:
		return row.Unihash, true, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", false, fmt.Errorf("hashserv: equivalent for outhash: %w", err)
	}

	// (3b) First sighting. Before minting the client's unihash, ask upstream: if upstream
	// already knows this output, adopt ITS unihash. Otherwise we would diverge from upstream
	// for an output upstream has already named, and every sstate object it has published
	// under that name would stop resolving for our users.
	if up != nil {
		if row, ok, err := up.GetOuthash(ctx, req.Method, req.Outhash, req.Taskhash); err == nil && ok &&
			validUnihash(row.Unihash) {
			return row.Unihash, true, nil
		}
	}

	return req.Unihash, false, nil
}

// persistUpstream write-throughs a row fetched from an upstream, reproducing upstream's own
// update_unified: insert BOTH the (method, taskhash) -> unihash mapping AND the outhash row.
//
// # It binds the UPSTREAM'S taskhash, never the caller's
//
// This is the fix for a cache-poisoning bug. `get-outhash` is keyed on (method, outhash) and
// upstream returns the taskhash of whatever row first produced that output -- NOT the taskhash
// the caller sent, which get_outhash ignores entirely. Writing `(method, caller_taskhash) ->
// upstream_unihash` would let a caller holding only @read (or none, on an open mirror) fabricate
// a permanent, write-once mapping between two values it fully controls, and since the unihash is
// embedded in the sstate object filename, point a victim's build at the wrong object. Upstream's
// update_unified inserts `data["taskhash"]` -- the upstream row's own taskhash -- and so do we.
func (s store) persistUpstream(ctx context.Context, row outhashResponse) error {
	if !validUnihash(row.Unihash) || row.Taskhash == "" {
		return nil
	}

	if _, err := s.insertUnihash(ctx, row.Method, row.Taskhash, row.Unihash); err != nil {
		return err
	}

	// The outhash row too, so a later local get-outhash for this output is a hit rather than
	// another upstream round trip. taskhash here is again the UPSTREAM row's, matching the
	// unihash we just wrote.
	_, err := s.q.InsertOuthash(ctx, repository.InsertOuthashParams{
		BackendID:      s.backendID,
		Method:         row.Method,
		Taskhash:       row.Taskhash,
		Outhash:        row.Outhash,
		Owner:          text(row.Owner),
		Pn:             text(row.PN),
		Pv:             text(row.PV),
		Pr:             text(row.PR),
		Task:           text(row.Task),
		OuthashSiginfo: text(row.OuthashSiginfo),
	})
	if err != nil {
		return fmt.Errorf("hashserv: persist upstream outhash: %w", err)
	}

	return nil
}

// reportEquiv directly asserts (method, taskhash) -> unihash with no outhash in play, then
// reads back whatever is ACTUALLY stored. bitbake uses it to retro-map a taskhash onto a
// unihash it already knows. Still write-once: the read-back, not the write, is the answer.
func (s store) reportEquiv(ctx context.Context, req reportEquivRequest) (unihashResponse, error) {
	if _, err := s.insertUnihash(ctx, req.Method, req.Taskhash, req.Unihash); err != nil {
		return unihashResponse{}, err
	}

	stored, ok, err := s.getUnihash(ctx, req.Method, req.Taskhash)
	if err != nil {
		return unihashResponse{}, err
	}

	if !ok {
		stored = req.Unihash
	}

	return unihashResponse{Taskhash: req.Taskhash, Method: req.Method, Unihash: stored}, nil
}

// reportReadOnly is the path taken when the caller may READ but not REPORT: an anonymous
// client on an open mirror, or a read-scoped key. It looks the output up and returns the
// stored unihash if we know it, else echoes the client's own back. It NEVER WRITES.
//
// This is upstream's documented read-only mode and it is the right behavior for a mirror --
// but it is a SILENT non-write, so the caller meters it (reports_dropped_total). A build
// whose reports are all being dropped is a build getting no equivalence, and that should be
// visible on a dashboard rather than inferred months later from a mysteriously cold cache.
func (s store) reportReadOnly(ctx context.Context, req reportRequest) (unihashResponse, error) {
	row, ok, err := s.getOuthash(ctx, req.Method, req.Outhash)
	if err != nil {
		return unihashResponse{}, err
	}

	unihash := req.Unihash
	if ok {
		unihash = row.Unihash
	}

	return unihashResponse{Taskhash: req.Taskhash, Method: req.Method, Unihash: unihash}, nil
}

// getOuthash returns the joined outhash row for an OUTPUT -- keyed on (method, outhash),
// oldest first. There is deliberately no taskhash in the predicate: the question is "has ANY
// task produced this output", which is the only form of the question that can discover an
// equivalence the caller did not already know. See the comment on the query.
func (s store) getOuthash(ctx context.Context, method, outhash string) (outhashResponse, bool, error) {
	row, err := s.q.GetOuthashWithUnihash(ctx, repository.GetOuthashWithUnihashParams{
		BackendID: s.backendID,
		Method:    method,
		Outhash:   outhash,
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return outhashResponse{}, false, nil
	case err != nil:
		return outhashResponse{}, false, fmt.Errorf("hashserv: get outhash: %w", err)
	}

	return outhashResponse{
		Method:         row.Method,
		Taskhash:       row.Taskhash,
		Outhash:        row.Outhash,
		Created:        formatCreated(row.CreatedAt),
		Owner:          row.Owner.String,
		PN:             row.Pn.String,
		PV:             row.Pv.String,
		PR:             row.Pr.String,
		Task:           row.Task.String,
		OuthashSiginfo: row.OuthashSiginfo.String,
		Unihash:        row.Unihash,
	}, true, nil
}

// getOuthashRaw backs `get-outhash` with with_unihash = false: the outhash row alone, with
// no unihash and no join, so an output whose task has no unihash yet is still answerable.
func (s store) getOuthashRaw(ctx context.Context, method, outhash string) (outhashResponse, bool, error) {
	row, err := s.q.GetOuthash(ctx, repository.GetOuthashParams{
		BackendID: s.backendID,
		Method:    method,
		Outhash:   outhash,
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return outhashResponse{}, false, nil
	case err != nil:
		return outhashResponse{}, false, fmt.Errorf("hashserv: get outhash: %w", err)
	}

	return outhashResponse{
		Method:         row.Method,
		Taskhash:       row.Taskhash,
		Outhash:        row.Outhash,
		Created:        formatCreated(row.CreatedAt),
		Owner:          row.Owner.String,
		PN:             row.Pn.String,
		PV:             row.Pv.String,
		PR:             row.Pr.String,
		Task:           row.Task.String,
		OuthashSiginfo: row.OuthashSiginfo.String,
	}, true, nil
}

// formatCreated renders a timestamp the way Python's json_serialize does -- datetime.isoformat(),
// microsecond precision, no timezone suffix. The client parses it back with fromisoformat.
func formatCreated(ts pgtype.Timestamptz) string {
	return ts.Time.UTC().Format("2006-01-02T15:04:05.000000")
}

// getUnihashFull backs `get` with "all": true -- the joined outhash row rather than the bare
// unihash. bitbake itself always asks with all=false; bitbake-hashclient is what uses this.
func (s store) getUnihashFull(ctx context.Context, method, taskhash string) (outhashResponse, bool, error) {
	row, err := s.q.GetUnihashFull(ctx, repository.GetUnihashFullParams{
		BackendID: s.backendID,
		Method:    method,
		Taskhash:  taskhash,
	})

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return outhashResponse{}, false, nil
	case err != nil:
		return outhashResponse{}, false, fmt.Errorf("hashserv: get unihash full: %w", err)
	}

	return outhashResponse{
		Method:         row.Method,
		Taskhash:       row.Taskhash,
		Outhash:        row.Outhash,
		Created:        row.CreatedAt.Time.UTC().Format("2006-01-02T15:04:05.000000"),
		Owner:          row.Owner.String,
		PN:             row.Pn.String,
		PV:             row.Pv.String,
		PR:             row.Pr.String,
		Task:           row.Task.String,
		OuthashSiginfo: row.OuthashSiginfo.String,
		Unihash:        row.Unihash,
	}, true, nil
}

// removableColumns is the CLOSED SET of columns `remove` may filter on -- exactly the four
// upstream's own suite filters on.
//
// A filter naming anything else is an invoke-error, never a silently-empty delete: `remove`
// deletes GC ROOTS (the unihash is what roots every sstate object), so "matched nothing" and
// "matched everything" must never be confusable, and an unrecognized column must not quietly
// widen into a table scan.
//
// The per-table asymmetry is upstream's and it is load-bearing: taskhash and method exist on
// BOTH tables; unihash exists only on the unihash table; outhash only on the outhash table. A
// remove by unihash therefore leaves the outhash rows standing, and upstream's
// test_remove_unihash asserts exactly that.
var removableColumns = []string{"taskhash", "unihash", "outhash", "method"}

// remove deletes by a single-column filter, returning the total rows removed across both
// tables (upstream sums them, and so does its test).
func (s store) remove(ctx context.Context, where map[string]string) (int64, error) {
	if len(where) != 1 {
		return 0, newInvokeError("remove: expected exactly one filter column, got %d", len(where))
	}

	var total int64

	for col, val := range where {
		var (
			uni, out int64
			err      error
		)

		switch col {
		case "taskhash":
			uni, err = s.q.RemoveUnihashesByTaskhash(ctx, repository.RemoveUnihashesByTaskhashParams{
				BackendID: s.backendID, Taskhash: val,
			})
			if err == nil {
				out, err = s.q.RemoveOuthashesByTaskhash(ctx, repository.RemoveOuthashesByTaskhashParams{
					BackendID: s.backendID, Taskhash: val,
				})
			}

		case "method":
			uni, err = s.q.RemoveUnihashesByMethod(ctx, repository.RemoveUnihashesByMethodParams{
				BackendID: s.backendID, Method: val,
			})
			if err == nil {
				out, err = s.q.RemoveOuthashesByMethod(ctx, repository.RemoveOuthashesByMethodParams{
					BackendID: s.backendID, Method: val,
				})
			}

		case "unihash":
			uni, err = s.q.RemoveUnihashesByUnihash(ctx, repository.RemoveUnihashesByUnihashParams{
				BackendID: s.backendID, Unihash: val,
			})

		case "outhash":
			out, err = s.q.RemoveOuthashesByOuthash(ctx, repository.RemoveOuthashesByOuthashParams{
				BackendID: s.backendID, Outhash: val,
			})

		default:
			return 0, newInvokeError("remove: cannot filter on %q; supported: %v", col, removableColumns)
		}

		if err != nil {
			return 0, fmt.Errorf("hashserv: remove by %s: %w", col, err)
		}

		total += uni + out
	}

	return total, nil
}
