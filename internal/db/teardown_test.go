// Teardown (migration 000018): the deleting_at soft-delete marks, and the two
// queries that make a marked row read as ABSENT rather than as merely disabled.
//
// package db_test for the same reason as db_test.go: these need a real Postgres,
// and the harness that provides one imports internal/db.
package db_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jsmith212/bakery/internal/db/dbtest"
	"github.com/jsmith212/bakery/internal/db/repository"
)

// TestDeletingBackendReadsAsUnconfigured is CLAUDE.md's unconfigured-backend rule
// applied to a backend being torn down.
//
// GetBackend is the route resolver's own probe (httpblob.CachedResolver.load), and
// its pgx.ErrNoRows IS the 404 -- every data-plane gate is `if !ok || !route.Enabled`
// over that one call, so filtering here makes the sstate mount, the hashserv
// upgrade, the bazel instance lookup and both OCI route families 404 with no Go
// change and no flag for any of them to forget.
//
// It must be ErrNoRows and not `enabled = false`. Those are different answers: a
// disabled backend is a row an operator can flip back on, and something that reads
// it as present will keep reasoning about it (the snippet generator's own "configured
// but disabled" sentence is exactly that). A backend being emptied is not coming
// back.
func TestDeletingBackendReadsAsUnconfigured(t *testing.T) {
	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	backendID := seedBackendKind(t, pool, repository.BackendKindSstate)

	row, err := q.GetBackendByID(ctx, backendID)
	if err != nil {
		t.Fatalf("GetBackendByID: %v", err)
	}

	if row.DeletingAt.Valid {
		t.Fatal("a freshly created backend is already marked deleting")
	}

	if _, err := q.GetBackend(ctx, repository.GetBackendParams{
		ProjectID: row.ProjectID, Kind: row.Kind,
	}); err != nil {
		t.Fatalf("GetBackend before the mark: %v", err)
	}

	marked, err := q.MarkBackendDeleting(ctx, backendID)
	if err != nil {
		t.Fatalf("MarkBackendDeleting: %v", err)
	}

	if !marked.DeletingAt.Valid {
		t.Fatal("MarkBackendDeleting left deleting_at NULL")
	}

	// AND enabled = false. The two facts are the same fact, and it means the GC's
	// disabled clamp (least(configured, 30d)) applies as a second line of defence if
	// the teardown stage is somehow not running.
	if marked.Enabled {
		t.Error("a backend marked for teardown is still enabled")
	}

	if _, err := q.GetBackend(ctx, repository.GetBackendParams{
		ProjectID: row.ProjectID, Kind: row.Kind,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetBackend after the mark = %v, want pgx.ErrNoRows (the 404 every mount answers)", err)
	}

	// The CONTROL PLANE still sees it, and that asymmetry is the point: the console
	// has to be able to say "deleting", and this is the only listing that can tell it.
	backends, err := q.ListBackendsForProject(ctx, row.ProjectID)
	if err != nil {
		t.Fatalf("ListBackendsForProject: %v", err)
	}

	if len(backends) != 1 || !backends[0].DeletingAt.Valid {
		t.Errorf("ListBackendsForProject = %+v, want the marked row with deleting_at set", backends)
	}

	// A SECOND MARK KEEPS THE FIRST INSTANT. That is what makes a repeat DELETE an
	// idempotent 202 rather than a restarted clock.
	again, err := q.MarkBackendDeleting(ctx, backendID)
	if err != nil {
		t.Fatalf("second MarkBackendDeleting: %v", err)
	}

	if !again.DeletingAt.Time.Equal(marked.DeletingAt.Time) {
		t.Errorf("deleting_at moved on a second mark: %v -> %v",
			marked.DeletingAt.Time, again.DeletingAt.Time)
	}
}

// TestDeletingProjectResolvesToNothing is the project half, and ResolveRoute is
// where it has to live: that ONE statement is both the cache route resolver's
// project probe and the control-plane guard's {project} resolution
// (api.resolveScope), so a marked project 404s on its cache mounts and on every
// /api/v1 route from the same predicate -- there is no second place for the two to
// disagree.
func TestDeletingProjectResolvesToNothing(t *testing.T) {
	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	backendID := seedBackendKind(t, pool, repository.BackendKindSstate)

	backend, err := q.GetBackendByID(ctx, backendID)
	if err != nil {
		t.Fatalf("GetBackendByID: %v", err)
	}

	project, err := q.GetProject(ctx, backend.ProjectID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	org, err := q.GetOrganization(ctx, project.OrgID)
	if err != nil {
		t.Fatalf("GetOrganization: %v", err)
	}

	params := repository.ResolveRouteParams{Slug: org.Slug, Slug_2: project.Slug}

	if _, err := q.ResolveRoute(ctx, params); err != nil {
		t.Fatalf("ResolveRoute before the mark: %v", err)
	}

	if _, err := q.MarkProjectDeleting(ctx, project.ID); err != nil {
		t.Fatalf("MarkProjectDeleting: %v", err)
	}

	if _, err := q.ResolveRoute(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ResolveRoute after the mark = %v, want pgx.ErrNoRows", err)
	}

	// HIDDEN FROM THE LISTING, for everyone. Not a permission question: the project's
	// routes already 404 and its slug is about to become available again, so a console
	// that still listed it would offer a link to a 404 and a rename form for a row that
	// is going away.
	projects, err := q.ListProjectsForOrg(ctx, project.OrgID)
	if err != nil {
		t.Fatalf("ListProjectsForOrg: %v", err)
	}

	if len(projects) != 0 {
		t.Errorf("ListProjectsForOrg = %+v, want none: a marked project is hidden", projects)
	}
}

// TestTornDownProjectDeleteRefusesWhileABackendRemains pins the RESTRICT ordering
// the teardown stage relies on: the project row can only go after its last backend
// row has, and ListProjectsForTeardown's anti-join is what stops the engine even
// trying before then.
func TestTornDownProjectDeleteRefusesWhileABackendRemains(t *testing.T) {
	pool := dbtest.New(t)
	ctx := t.Context()
	q := repository.New(pool)

	backendID := seedBackendKind(t, pool, repository.BackendKindSstate)

	backend, err := q.GetBackendByID(ctx, backendID)
	if err != nil {
		t.Fatalf("GetBackendByID: %v", err)
	}

	if _, err := q.MarkProjectDeleting(ctx, backend.ProjectID); err != nil {
		t.Fatalf("MarkProjectDeleting: %v", err)
	}

	pending, err := q.ListProjectsForTeardown(ctx)
	if err != nil {
		t.Fatalf("ListProjectsForTeardown: %v", err)
	}

	if len(pending) != 0 {
		t.Errorf("ListProjectsForTeardown = %+v while a backend still exists, want none", pending)
	}

	if _, err := q.DeleteTornDownBackend(ctx, backendID); err != nil {
		t.Fatalf("DeleteTornDownBackend on an unmarked backend: %v", err)
	}

	// Unmarked: the guard in DeleteTornDownBackend is what stops a wrong id deleting a
	// live backend, and it is asserted here rather than assumed.
	if _, err := q.GetBackendByID(ctx, backendID); err != nil {
		t.Fatalf("an UNMARKED backend was deleted by DeleteTornDownBackend: %v", err)
	}

	if _, err := q.MarkBackendDeleting(ctx, backendID); err != nil {
		t.Fatalf("MarkBackendDeleting: %v", err)
	}

	n, err := q.DeleteTornDownBackend(ctx, backendID)
	if err != nil {
		t.Fatalf("DeleteTornDownBackend: %v", err)
	}

	if n != 1 {
		t.Fatalf("DeleteTornDownBackend deleted %d rows, want 1", n)
	}

	pending, err = q.ListProjectsForTeardown(ctx)
	if err != nil {
		t.Fatalf("ListProjectsForTeardown: %v", err)
	}

	if len(pending) != 1 || pending[0].ID != backend.ProjectID {
		t.Fatalf("ListProjectsForTeardown = %+v, want the now-empty marked project", pending)
	}

	if rows, err := q.DeleteTornDownProject(ctx, backend.ProjectID); err != nil || rows != 1 {
		t.Fatalf("DeleteTornDownProject = (%d, %v), want (1, nil)", rows, err)
	}
}
