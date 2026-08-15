package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
	"github.com/jsmith212/bakery/internal/gc"
)

// fakeGCTrigger is a hand-written gcTrigger: no gomock, no testify, matching every
// other fake in this package. It records every call so a test can assert what the
// handler ASKED FOR (trigger, dry_run), not only what it returned.
type fakeGCTrigger struct {
	calls  []fakeGCTriggerCall
	nextID int64
	err    error
}

type fakeGCTriggerCall struct {
	trigger gc.Trigger
	dryRun  bool
}

func (f *fakeGCTrigger) TriggerAsync(_ context.Context, trigger gc.Trigger, dryRun bool) (int64, error) {
	f.calls = append(f.calls, fakeGCTriggerCall{trigger: trigger, dryRun: dryRun})

	if f.err != nil {
		return 0, f.err
	}

	f.nextID++

	return f.nextID, nil
}

// testGCAPI builds an API exactly as testAPI does, plus the gcTrigger testAPI has
// no parameter for -- kept separate rather than widening testAPI's signature,
// which every other test file in this package calls with two arguments.
func testGCAPI(t *testing.T, store Store, gcT gcTrigger) *API {
	t.Helper()

	return &API{
		store: store, auth: devLoginAuth{enabled: false}, keys: nil, gc: gcT,
		log: discardLogger(), allowSelfServeOrgs: true, allowLocalSiteAdmins: true,
		metrics: nil, routes: nil,
	}
}

// ---------------------------------------------------------------------------
// Authorization: site admin only, API-key principal structurally excluded.
// ---------------------------------------------------------------------------

// TestGCRoutesRequireSiteAdmin drives the REAL mounted routes and the REAL
// handlers (unlike TestGuardAuthorizationMatrix, which wraps a stub) through
// every role the fixture cast has, including an API-key principal.
//
// The guard's rule for AccessSiteAdmin is unconditional: a key's IsSiteAdmin() is
// false even when the OWNING human is a site admin (auth.KeyGrant carries no site
// role at all), and the guard admits a key to AccessAuthenticated ONLY -- so
// api_key is denied here structurally, the same way every other AccessSiteAdmin
// route is, not by a check this handler remembered to add.
func TestGCRoutesRequireSiteAdmin(t *testing.T) {
	t.Parallel()

	store := fixtureStore(t)
	store.gcRuns = []repository.ListGCRunsRow{
		{
			ID: 1, Status: repository.GcRunStatusSucceeded, Trigger: "interval", DryRun: false,
			StartedAt: pgtype.Timestamptz{Valid: true}, FinishedAt: pgtype.Timestamptz{Valid: true},
			Error: pgtype.Text{}, ObjectsDeleted: 0, BlobsMarked: 0, BlobsDeleted: 0,
			BytesReclaimed: 0, HashservRowsDeleted: 0,
		},
	}

	fakeGC := &fakeGCTrigger{}
	a := testGCAPI(t, store, fakeGC)

	mux := http.NewServeMux()
	a.mount(mux)

	cast := principals(t)

	routes := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/gc/runs"},
		{http.MethodGet, "/api/v1/gc/runs/1"},
		{http.MethodPost, "/api/v1/gc/run"},
	}

	for _, route := range routes {
		for role, p := range cast {
			t.Run(route.method+" "+route.path+"/"+role, func(t *testing.T) {
				var body io.Reader
				if route.method == http.MethodPost {
					body = strings.NewReader(`{}`)
				}

				r := httptest.NewRequest(route.method, route.path, body)
				if route.method == http.MethodPost {
					r.Header.Set("Content-Type", "application/json")
				}

				if p != nil {
					r = r.WithContext(withPrincipal(r.Context(), p))
				}

				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)

				switch role {
				case "anonymous":
					if w.Code != http.StatusUnauthorized {
						t.Errorf("status = %d, want 401 (no principal at all)", w.Code)
					}
				case "site_admin":
					if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
						t.Errorf("status = %d, a site admin must not be denied", w.Code)
					}
				default:
					// org_owner, org_admin, org_member, proj_admin, proj_write, proj_read,
					// outsider, api_key -- NONE of them is a site admin, and the guard's
					// AccessSiteAdmin path checks ONLY p.IsSiteAdmin(), never an org or
					// project role. api_key belongs in this branch, not a branch of its
					// own: it is denied for the same reason every other non-admin role is.
					if w.Code != http.StatusForbidden {
						t.Errorf("status = %d, want 403 for role %q", w.Code, role)
					}
				}
			})
		}
	}

	// The denied calls above must never have reached the engine: a 403 that still
	// triggered a sweep would be the write-behind-a-lie bug the matrix test's
	// "reached" assertion exists to catch, reproduced here for a real handler.
	if len(fakeGC.calls) != 1 {
		t.Errorf("TriggerAsync called %d times, want exactly 1 (the site_admin POST)", len(fakeGC.calls))
	}
}

// ---------------------------------------------------------------------------
// POST /gc/run: the 202 and 409 paths.
// ---------------------------------------------------------------------------

// TestTriggerGCRun202 is the accepted path: the handler answers as soon as
// TriggerAsync returns an id, carries the id and "running" in the body, and
// passes dry_run through UNCHANGED -- a dry run and a real one must not be
// silently coerced into each other on the way through the handler.
func TestTriggerGCRun202(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		dryRun bool
	}{
		{"a real run", `{"dry_run":false}`, false},
		{"a dry run", `{"dry_run":true}`, true},
		{"an empty body defaults to a real run", `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fakeGC := &fakeGCTrigger{}
			a := testGCAPI(t, fixtureStore(t), fakeGC)

			admin := principals(t)["site_admin"]

			r := httptest.NewRequest(http.MethodPost, "/api/v1/gc/run", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(withPrincipal(r.Context(), admin))

			w := httptest.NewRecorder()
			a.guard(AccessSiteAdmin, a.handleTriggerGCRun)(w, r)

			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202. body: %s", w.Code, w.Body.String())
			}

			var resp TriggerGCRunResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}

			if resp.ID != 1 {
				t.Errorf("response id = %d, want 1", resp.ID)
			}

			if resp.Status != "running" {
				t.Errorf("response status = %q, want %q", resp.Status, "running")
			}

			if len(fakeGC.calls) != 1 {
				t.Fatalf("TriggerAsync called %d times, want 1", len(fakeGC.calls))
			}

			if got := fakeGC.calls[0]; got.trigger != gc.TriggerAPI || got.dryRun != tt.dryRun {
				t.Errorf("TriggerAsync called with (trigger=%q, dryRun=%v), want (trigger=%q, dryRun=%v)",
					got.trigger, got.dryRun, gc.TriggerAPI, tt.dryRun)
			}
		})
	}
}

// TestTriggerGCRun409 is the refused path: gc.ErrAlreadyRunning renders exactly
// 409, with the closed conflict code the CLI and console branch on -- not a bare
// 500, which is what a handler that forgot to unwrap the sentinel would produce.
func TestTriggerGCRun409(t *testing.T) {
	t.Parallel()

	fakeGC := &fakeGCTrigger{err: gc.ErrAlreadyRunning}
	a := testGCAPI(t, fixtureStore(t), fakeGC)

	admin := principals(t)["site_admin"]

	r := httptest.NewRequest(http.MethodPost, "/api/v1/gc/run", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withPrincipal(r.Context(), admin))

	w := httptest.NewRecorder()
	a.guard(AccessSiteAdmin, a.handleTriggerGCRun)(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409. body: %s", w.Code, w.Body.String())
	}

	var body ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}

	if body.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want %q", body.Error.Code, CodeConflict)
	}
}

// TestTriggerGCRunWithNoEngineConfigured: a partial API (no Config.GC wired) must
// refuse cleanly, never panic on a.gc's nil interface. See API.gc's doc.
func TestTriggerGCRunWithNoEngineConfigured(t *testing.T) {
	t.Parallel()

	a := testGCAPI(t, fixtureStore(t), nil)
	admin := principals(t)["site_admin"]

	r := httptest.NewRequest(http.MethodPost, "/api/v1/gc/run", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withPrincipal(r.Context(), admin))

	w := httptest.NewRecorder()
	a.guard(AccessSiteAdmin, a.handleTriggerGCRun)(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (refused cleanly, not a panic)", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /gc/runs and GET /gc/runs/{id}.
// ---------------------------------------------------------------------------

// TestListGCRunsPagination proves the keyset cursor: a full page carries
// next_cursor set to the LAST row's id, and a short page carries nil -- the two
// signals a client needs to know whether to ask again.
func TestListGCRunsPagination(t *testing.T) {
	t.Parallel()

	store := fixtureStore(t)

	// Newest first, matching ListGCRuns' own ORDER BY id DESC.
	for id := int64(5); id >= 1; id-- {
		store.gcRuns = append(store.gcRuns, repository.ListGCRunsRow{
			ID: id, Status: repository.GcRunStatusSucceeded, Trigger: "interval", DryRun: false,
			StartedAt: pgtype.Timestamptz{Valid: true}, FinishedAt: pgtype.Timestamptz{Valid: true},
			Error: pgtype.Text{}, ObjectsDeleted: 0, BlobsMarked: 0, BlobsDeleted: 0,
			BytesReclaimed: 0, HashservRowsDeleted: 0,
		})
	}

	a := testGCAPI(t, store, &fakeGCTrigger{})
	admin := principals(t)["site_admin"]

	// Page 1 of 2: limit=3 over 5 rows is a FULL page, so a next cursor is expected.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/gc/runs?limit=3", nil)
	r = r.WithContext(withPrincipal(r.Context(), admin))

	w := httptest.NewRecorder()
	a.guard(AccessSiteAdmin, a.handleListGCRuns)(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
	}

	var page1 GCRunList
	if err := json.Unmarshal(w.Body.Bytes(), &page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}

	if len(page1.Items) != 3 || page1.Items[0].ID != 5 || page1.Items[2].ID != 3 {
		t.Fatalf("page 1 items = %+v, want ids [5 4 3]", page1.Items)
	}

	if page1.NextCursor == nil || *page1.NextCursor != 3 {
		t.Fatalf("page 1 next_cursor = %v, want 3 (the last row's id)", page1.NextCursor)
	}

	// Page 2: resume from the cursor. 2 rows remain, a SHORT page, so no cursor.
	r2 := httptest.NewRequest(
		http.MethodGet, "/api/v1/gc/runs?limit=3&before="+strconv.FormatInt(*page1.NextCursor, 10), nil,
	)
	r2 = r2.WithContext(withPrincipal(r2.Context(), admin))

	w2 := httptest.NewRecorder()
	a.guard(AccessSiteAdmin, a.handleListGCRuns)(w2, r2)

	var page2 GCRunList
	if err := json.Unmarshal(w2.Body.Bytes(), &page2); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}

	if len(page2.Items) != 2 || page2.Items[0].ID != 2 || page2.Items[1].ID != 1 {
		t.Fatalf("page 2 items = %+v, want ids [2 1]", page2.Items)
	}

	if page2.NextCursor != nil {
		t.Errorf("page 2 next_cursor = %v, want nil (short page, no more rows)", *page2.NextCursor)
	}
}

// TestListGCRunsRejectsAnUnknownStatus: ?status= is a closed vocabulary, and a
// typo must be a 422 the caller can act on, not a query that silently matches
// nothing.
func TestListGCRunsRejectsAnUnknownStatus(t *testing.T) {
	t.Parallel()

	a := testGCAPI(t, fixtureStore(t), &fakeGCTrigger{})
	admin := principals(t)["site_admin"]

	r := httptest.NewRequest(http.MethodGet, "/api/v1/gc/runs?status=bogus", nil)
	r = r.WithContext(withPrincipal(r.Context(), admin))

	w := httptest.NewRecorder()
	a.guard(AccessSiteAdmin, a.handleListGCRuns)(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

// TestGetGCRunNotFound: an id with no matching row is a clean 404, not a 500 --
// pgx.ErrNoRows must be unwrapped exactly as it is everywhere else in this API.
func TestGetGCRunNotFound(t *testing.T) {
	t.Parallel()

	a := testGCAPI(t, fixtureStore(t), &fakeGCTrigger{})
	admin := principals(t)["site_admin"]

	r := httptest.NewRequest(http.MethodGet, "/api/v1/gc/runs/999", nil)
	r.SetPathValue("id", "999")
	r = r.WithContext(withPrincipal(r.Context(), admin))

	w := httptest.NewRecorder()
	a.guard(AccessSiteAdmin, a.handleGetGCRun)(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404. body: %s", w.Code, w.Body.String())
	}
}

// TestGetGCRunInvalidID: {id} is a plain bigint, never a uuid -- unlike every
// other path identifier in this API -- and a non-numeric one is a 400, not a
// panic in strconv or a query issued with a garbage parameter.
func TestGetGCRunInvalidID(t *testing.T) {
	t.Parallel()

	a := testGCAPI(t, fixtureStore(t), &fakeGCTrigger{})
	admin := principals(t)["site_admin"]

	r := httptest.NewRequest(http.MethodGet, "/api/v1/gc/runs/not-a-number", nil)
	r.SetPathValue("id", "not-a-number")
	r = r.WithContext(withPrincipal(r.Context(), admin))

	w := httptest.NewRecorder()
	a.guard(AccessSiteAdmin, a.handleGetGCRun)(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// THE THREE 409s ARE DISTINCT SENTENCES (R7#1). All three are conflicts and all
// three are CodeConflict, but "wait for the running sweep", "your operator turned
// gc off" and "this deployment cannot run gc safely" are three different next
// actions, and a caller that cannot tell them apart retries the third one forever.
func TestTriggerGCRunConflictsAreDistinguishable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantAny []string
	}{
		{name: "already running", err: gc.ErrAlreadyRunning, wantAny: []string{"already running"}},
		{name: "disabled", err: gc.ErrDisabled, wantAny: []string{"disabled", "--gc-enabled"}},
		{
			name: "multi-instance", err: gc.ErrMultiInstance,
			wantAny: []string{"--allow-multi-instance"},
		},
	}

	seen := map[string]struct{}{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := testGCAPI(t, fixtureStore(t), &fakeGCTrigger{err: tc.err})
			admin := principals(t)["site_admin"]

			r := httptest.NewRequest(http.MethodPost, "/api/v1/gc/run", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			r = r.WithContext(withPrincipal(r.Context(), admin))

			w := httptest.NewRecorder()
			a.guard(AccessSiteAdmin, a.handleTriggerGCRun)(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409. body: %s", w.Code, w.Body.String())
			}

			var body ErrorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}

			if body.Error.Code != CodeConflict {
				t.Errorf("error code = %q, want %q", body.Error.Code, CodeConflict)
			}

			for _, want := range tc.wantAny {
				if !strings.Contains(body.Error.Message, want) {
					t.Errorf("message %q does not mention %q", body.Error.Message, want)
				}
			}

			if _, dup := seen[body.Error.Message]; dup {
				t.Errorf("message %q is shared with another refusal: the three conflicts must "+
					"not read the same", body.Error.Message)
			}

			seen[body.Error.Message] = struct{}{}
		})
	}
}

// THE USAGE-MEASUREMENT RUNS ARE HIDDEN BY DEFAULT (R7#12). MeasureUsage mints one
// gc_runs row every --gc-usage-interval (six hours, forever, whether or not
// retention is even enabled) purely so the measurement is auditable. Left visible,
// they bury the sweeps an operator opened the page to look at.
func TestListGCRunsHidesUsageRunsUnlessAsked(t *testing.T) {
	t.Parallel()

	store := fixtureStore(t)
	for id, trigger := range map[int64]string{3: "usage", 2: "api", 1: "interval"} {
		store.gcRuns = append(store.gcRuns, repository.ListGCRunsRow{
			ID: id, Status: repository.GcRunStatusSucceeded, Trigger: trigger, DryRun: trigger == "usage",
			StartedAt: pgtype.Timestamptz{Valid: true}, FinishedAt: pgtype.Timestamptz{Valid: true},
			Error: pgtype.Text{}, ObjectsDeleted: 0, BlobsMarked: 0, BlobsDeleted: 0,
			BytesReclaimed: 0, HashservRowsDeleted: 0,
		})
	}

	a := testGCAPI(t, store, &fakeGCTrigger{})
	admin := principals(t)["site_admin"]

	triggersFor := func(query string) []string {
		t.Helper()

		r := httptest.NewRequest(http.MethodGet, "/api/v1/gc/runs"+query, nil)
		r = r.WithContext(withPrincipal(r.Context(), admin))

		w := httptest.NewRecorder()
		a.guard(AccessSiteAdmin, a.handleListGCRuns)(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. body: %s", w.Code, w.Body.String())
		}

		var body GCRunList
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		out := make([]string, 0, len(body.Items))
		for _, item := range body.Items {
			out = append(out, item.Trigger)
		}

		return out
	}

	for _, query := range []string{"", "?limit=10"} {
		for _, trigger := range triggersFor(query) {
			if trigger == string(gc.TriggerUsage) {
				t.Errorf("GET /gc/runs%s listed a usage-measurement run by default", query)
			}
		}
	}

	for _, query := range []string{"?include_usage=true", "?trigger=usage"} {
		found := false

		for _, trigger := range triggersFor(query) {
			if trigger == string(gc.TriggerUsage) {
				found = true
			}
		}

		if !found {
			t.Errorf("GET /gc/runs%s did not list the usage-measurement run", query)
		}
	}
}
