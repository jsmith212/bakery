package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// READ-TRIGGERED USAGE MEASUREMENT.
//
// The freshness decision is ordinary handler logic and every one of its branches has
// a failure that only shows up in production: measuring on every read turns a
// dashboard refresh into a count over ten million rows; never measuring is the
// six-hour staleness this change exists to fix; and failing the read when the
// measurement fails takes a whole screen down over the freshest of its numbers.
//
// So it is tested here, against a fake measurer, and not only end to end.

// fakeMeasurer records calls and can be made slow or failing.
type fakeMeasurer struct {
	calls int
	err   error
}

// The MEASUREMENT'S own timeout lives in the engine (gc.measureTimeout), not here, so
// there is nothing for this fake to stall: from the handler's point of view a
// measurement that ran out of time is a measurement that returned an error, which is
// the `err` field.
func (m *fakeMeasurer) MeasureProject(_ context.Context, _ pgtype.UUID) error {
	m.calls++

	return m.err
}

// usageStore is a fakeStore carrying one sstate usage row measured `age` ago. A
// negative age means the row exists but has NEVER been measured, which is the state
// a brand-new backend is in and the case the whole feature exists for.
func usageStore(t *testing.T, age time.Duration) *fakeStore {
	t.Helper()

	store := fixtureStore(t)

	measured := pgtype.Timestamptz{}
	if age >= 0 {
		measured = pgtype.Timestamptz{
			Time: time.Now().Add(-age), InfinityModifier: 0, Valid: true,
		}
	}

	store.projectUsage = []repository.GetProjectBackendUsageRow{{
		Kind:         repository.BackendKindSstate,
		ObjectsCount: pgtype.Int8{Int64: 7, Valid: true},
		LogicalBytes: pgtype.Int8{Int64: 4096, Valid: true},
		MeasuredAt:   measured,
	}}

	return store
}

// usageAPI wires a fake measurer and a freshness window onto a test API. Both fields
// are unexported and set directly rather than through Config, because Config.GC is a
// *gc.Engine and there is deliberately no way to build one without a database.
func usageAPI(t *testing.T, store Store, m usageMeasurer, freshness time.Duration) *API {
	t.Helper()

	a := testAPI(t, store, nil)
	a.measurer = m
	a.usageFreshness = freshness

	return a
}

func TestProjectUsageFreshness(t *testing.T) {
	reader := principals(t)["proj_read"]
	path := Prefix + "/orgs/acme/projects/firmware/usage"

	t.Run("a fresh row is served without measuring", func(t *testing.T) {
		m := &fakeMeasurer{}
		a := usageAPI(t, usageStore(t, time.Second), m, time.Minute)

		if w := do(t, a, reader, http.MethodGet, path, ""); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}

		// The whole point of the window: a dashboard refreshed in a tight loop must not
		// become one aggregate per request over every cache_objects row of the project.
		if m.calls != 0 {
			t.Errorf("MeasureProject called %d times for a fresh row, want 0", m.calls)
		}
	})

	t.Run("a stale row is measured before answering", func(t *testing.T) {
		m := &fakeMeasurer{}
		a := usageAPI(t, usageStore(t, time.Hour), m, time.Minute)

		if w := do(t, a, reader, http.MethodGet, path, ""); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}

		if m.calls != 1 {
			t.Errorf("MeasureProject called %d times for a stale row, want 1", m.calls)
		}
	})

	// A NEVER-MEASURED project is stale. This is the case the feature exists for: a
	// backend created a moment ago, whose figures a human is looking at right now.
	t.Run("a never-measured project is measured", func(t *testing.T) {
		m := &fakeMeasurer{}
		a := usageAPI(t, usageStore(t, -1), m, time.Minute)

		if w := do(t, a, reader, http.MethodGet, path, ""); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}

		if m.calls != 1 {
			t.Errorf("MeasureProject called %d times for a never-measured project, want 1", m.calls)
		}
	})

	// ZERO DISABLES IT, exactly restoring the behaviour every release before this one
	// had: every figure comes from the periodic backstop.
	t.Run("--usage-freshness=0 never measures", func(t *testing.T) {
		m := &fakeMeasurer{}
		a := usageAPI(t, usageStore(t, 365*24*time.Hour), m, 0)

		if w := do(t, a, reader, http.MethodGet, path, ""); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}

		if m.calls != 0 {
			t.Errorf("MeasureProject called %d times with the knob off, want 0", m.calls)
		}
	})

	// NIL-TOLERANT. An embedder, or any test, with no engine wired must keep serving
	// what is on file rather than panicking on a nil interface.
	t.Run("a nil measurer serves the row as-is", func(t *testing.T) {
		a := usageAPI(t, usageStore(t, time.Hour), nil, time.Minute)

		w := do(t, a, reader, http.MethodGet, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}

		if got := decodeUsage(t, w.Body.Bytes()); len(got) != 1 || got[0].MeasuredAt == nil {
			t.Errorf("usage = %+v, want the row on file", got)
		}
	})

	// THE MEASUREMENT NEVER FAILS THE READ. A timeout, a cancelled context or a
	// database hiccup leaves the caller with the rows already in hand -- stale, and
	// honestly labelled as such by measured_at. 500ing a whole screen because a
	// derived figure could not be recomputed is the worse answer.
	t.Run("a failing measurement serves the stale row", func(t *testing.T) {
		m := &fakeMeasurer{err: errors.New("measurement timed out")}
		a := usageAPI(t, usageStore(t, time.Hour), m, time.Minute)

		w := do(t, a, reader, http.MethodGet, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}

		got := decodeUsage(t, w.Body.Bytes())
		if len(got) != 1 || got[0].MeasuredAt == nil {
			t.Fatalf("usage = %+v, want the stale row served unchanged", got)
		}

		// measured_at is the truth-teller: it still names the OLD measurement, so the
		// console renders "measured an hour ago" rather than implying a refresh happened.
		if age := time.Since(*got[0].MeasuredAt); age < 30*time.Minute {
			t.Errorf("measured_at is %v old, want the stale value untouched", age)
		}
	})
}

// TestProjectUsageMeasureEndpoint covers the explicit Refresh button.
func TestProjectUsageMeasureEndpoint(t *testing.T) {
	reader := principals(t)["proj_read"]
	path := Prefix + "/orgs/acme/projects/firmware/usage/measure"

	t.Run("measures and answers 200", func(t *testing.T) {
		m := &fakeMeasurer{}
		a := usageAPI(t, usageStore(t, time.Hour), m, 0)

		w := do(t, a, reader, http.MethodPost, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}

		if m.calls != 1 {
			t.Errorf("MeasureProject called %d times, want 1", m.calls)
		}

		// The knob is OFF here on purpose: the explicit refresh must not depend on
		// --usage-freshness, which governs ordinary page loads. A human pressing Refresh
		// is asking for a measurement, not for a policy decision.
		if got := decodeUsage(t, w.Body.Bytes()); len(got) != 1 {
			t.Errorf("usage = %+v, want the project's rows", got)
		}
	})

	// THE RATE LIMIT, AND IT IS NOT AN ERROR. "You asked too soon" is not information
	// a dashboard can act on -- and the row it gets back carries measured_at, which
	// already says exactly how fresh the answer is. A 429 here would give the client
	// nothing to do except show the same rows with an error attached.
	t.Run("a repeat inside the rate limit is a 200 that measures nothing", func(t *testing.T) {
		m := &fakeMeasurer{}
		a := usageAPI(t, usageStore(t, time.Second), m, 0)

		w := do(t, a, reader, http.MethodPost, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}

		if m.calls != 0 {
			t.Errorf("MeasureProject called %d times inside the rate limit, want 0", m.calls)
		}

		if got := decodeUsage(t, w.Body.Bytes()); len(got) != 1 {
			t.Errorf("usage = %+v, want the current rows anyway", got)
		}
	})

	// The rate limit is TIGHTER than the read freshness window, and deliberately so:
	// a row 30s old is fresh enough that a page load leaves it alone, and stale enough
	// that pressing Refresh does something.
	t.Run("the rate limit is tighter than the read freshness window", func(t *testing.T) {
		m := &fakeMeasurer{}
		a := usageAPI(t, usageStore(t, 30*time.Second), m, time.Minute)

		if w := do(t, a, reader, http.MethodGet, Prefix+"/orgs/acme/projects/firmware/usage",
			""); w.Code != http.StatusOK {
			t.Fatalf("get status = %d, want 200", w.Code)
		}

		if m.calls != 0 {
			t.Fatalf("the GET measured a 30s-old row against a 60s window")
		}

		if w := do(t, a, reader, http.MethodPost, path, ""); w.Code != http.StatusOK {
			t.Fatalf("post status = %d, want 200", w.Code)
		}

		if m.calls != 1 {
			t.Errorf("MeasureProject called %d times on an explicit refresh, want 1", m.calls)
		}
	})
}

func decodeUsage(t *testing.T, body []byte) []ProjectBackendUsage {
	t.Helper()

	var out ListResponse[ProjectBackendUsage]
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode usage: %v (body %s)", err, body)
	}

	return out.Items
}
