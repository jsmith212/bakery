package api

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jsmith212/bakery/internal/db/repository"
)

// ---------------------------------------------------------------------------
// Object browser (B3, spec docs/design/specs/2026-08-15-spa-api-wiring.md):
// GET .../projects/{project}/backends/{kind}/objects
//     ?namespace=&prefix=&after_key=&limit=
//
// KEYSET over cache_objects_pkey (backend_id, namespace, key), the same cursor
// shape internal/gc's own ScanObjectsForGC uses and for the identical reason:
// cache_objects is sized in the tens of millions of rows, and an OFFSET page on
// a table that size is a full scan of everything before it, every request.
// ---------------------------------------------------------------------------

// CacheObject is one row of the object browser.
type CacheObject struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	// Digest is lower-case hex sha256, the wire form nothing in this package has
	// needed to render before now.
	Digest    string    `json:"digest"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`

	// AccessedAt is rendered APPROXIMATELY. CLAUDE.md's toucher-ramp invariant:
	// the write lands on a staleness ramp seeded at migration time -- up to 24h
	// coarse for the first 7 days after the M6 upgrade, --gc-touch-staleness
	// (1h by default) after that -- so this is a "last accessed, roughly"
	// figure, never a live timestamp. nil means the object has never been read
	// since the upgrade, which is the ordinary state of every pre-existing row
	// on day one (000012), not an error.
	AccessedAt *time.Time `json:"accessed_at"`
}

func newCacheObject(r repository.ListCacheObjectsForBrowseRow) CacheObject {
	return CacheObject{
		Namespace: r.Namespace, Key: r.Key, Digest: hex.EncodeToString(r.Digest),
		SizeBytes: r.SizeBytes, CreatedAt: r.CreatedAt.Time, AccessedAt: timePtr(r.AccessedAt),
	}
}

// CacheObjectList is the keyset-paginated envelope. NextCursor is the last
// item's key to pass as the next request's ?after_key=; nil means this was the
// last page -- the same "a short page proves there is no more" signal
// GET /gc/runs uses.
type CacheObjectList struct {
	Items      []CacheObject `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

// objectsDefaultLimit/objectsMaxLimit: B3's own numbers (spec), CLAMPED per the
// gc.go:155-173 convention every new list endpoint in this wave copies -- a
// caller asking for too much gets the ceiling, never a 422, because the ceiling
// is an implementation bound, not a rule the caller needs to learn.
const (
	objectsDefaultLimit = 50
	objectsMaxLimit     = 200
)

// objectsLimit parses ?limit=.
func objectsLimit(s string) (int32, error) {
	if s == "" {
		return objectsDefaultLimit, nil
	}

	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, errValidation("limit", "limit must be a positive integer")
	}

	if n > objectsMaxLimit {
		n = objectsMaxLimit
	}

	return int32(n), nil
}

// prefixUpperBound computes the EXCLUSIVE upper bound of a "starts with prefix"
// key range -- the prefix with its last byte incremented, truncated there -- so
// query/usage.sql's ListCacheObjectsForBrowse can express the filter as a real
// cache_objects_pkey range scan (key >= prefix AND key < upper) instead of a
// LIKE, which only uses that index under the C collation (see
// query/objects.sql's ListObjectKeysByPrefix, which accepts the LIKE seq-scan
// for its own much smaller namespace on exactly this reasoning).
//
// Returns ("", false) for an empty prefix (no filter, no upper bound at all) and
// for a prefix whose every byte is already 0xFF (no representable next string;
// the caller then scans everything from the prefix onward). The second case does
// not arise for any key this system writes -- every cache_objects.key is a task
// hash, a digest hex string or a URL-shaped path, all ASCII.
func prefixUpperBound(prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}

	b := []byte(prefix)

	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++

			return string(b[:i+1]), true
		}
	}

	return "", false
}

// handleListCacheObjects is B3. ProjectRead.
//
// It resolves {kind} through backendOf (backends.go), which is what makes an
// UNCONFIGURED backend a 404 here too -- CLAUDE.md's invariant that a
// project/kind with no cache_backends row 404s applies to every mount this
// endpoint could otherwise be asked to browse.
func (a *API) handleListCacheObjects(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	backend, err := a.backendOf(r)
	if err != nil {
		return err
	}

	q := r.URL.Query()

	// namespace defaults to "" (nsDefault in internal/gc's own vocabulary) --
	// sstate's and downloads' own namespace. Browsing bazel's ac/ac-grpc/sccache/cas
	// or oci's tags/manifests/blobs needs an explicit ?namespace=; see
	// query/usage.sql's own comment for why this is matched exactly rather than
	// left open (an open match widens the leading PRIMARY KEY column from an
	// equality to a range and costs the index prefix).
	namespace := q.Get("namespace")
	prefix := q.Get("prefix")
	afterKey := q.Get("after_key")

	limit, err := objectsLimit(q.Get("limit"))
	if err != nil {
		return err
	}

	var prefixUpper pgtype.Text

	if upper, ok := prefixUpperBound(prefix); ok {
		prefixUpper = pgtype.Text{String: upper, Valid: true}
	}

	rows, err := a.store.ListCacheObjectsForBrowse(ctx, repository.ListCacheObjectsForBrowseParams{
		BackendID: backend.ID, Namespace: namespace, AfterKey: afterKey,
		Prefix: prefix, PrefixUpper: prefixUpper, PageLimit: limit,
	})
	if err != nil {
		return fmt.Errorf("list cache objects: %w", err)
	}

	out := make([]CacheObject, 0, len(rows))
	for _, row := range rows {
		out = append(out, newCacheObject(row))
	}

	// A FULL page means there may be more; a short page proves there is not --
	// the same convention handleListGCRuns uses.
	var next *string

	if int32(len(rows)) == limit && len(rows) > 0 {
		k := rows[len(rows)-1].Key
		next = &k
	}

	writeJSON(w, http.StatusOK, CacheObjectList{Items: out, NextCursor: next})

	return nil
}
