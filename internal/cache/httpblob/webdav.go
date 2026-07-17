package httpblob

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsmith212/bakery/internal/cache"
)

// servePropfind answers opendal's WebDAV stat.
//
// sccache's opendal backend PROPFINDs before writing, and WHAT it probes differs by
// generation: opendal 0.55 (sccache 0.16) probes only the parent COLLECTION, while
// opendal 0.4x (sccache <= 0.8) stats the OBJECT PATH ITSELF before every write. The
// two need opposite answers, and opendal's own path convention disambiguates: a
// directory path carries a TRAILING SLASH (and the mount root arrives as the empty
// {path...}); everything else names a file.
//
//   - Collection: 207 declaring an existing collection. A blob store has no
//     directories, so every directory path "exists" -- opendal's mkcol loop then breaks
//     on iteration 1 and sends zero MKCOLs.
//   - File, object exists: 207 with <D:getcontentlength> and a NON-collection
//     resourcetype. Declaring it a collection makes the old writer see "a directory
//     sits where my file goes" and abort the PUT -- cache_writes=0, silently
//     read-only, which is exactly how CI's sccache v0.8.2 failed while 0.16 passed.
//   - File, no object: 404. opendal maps it to NotFound -- "no conflict, proceed to
//     PUT". NEVER 400: any non-2xx/404 becomes ErrorKind::Unexpected, which check()
//     swallows into can_write=false for the whole process.
//
// The 207 body is not cosmetic. opendal deserializes <D:getlastmodified> as a
// NON-optional String parsed as an RFC 2822 date, and classifies the path off
// <D:resourcetype>. Omit either and opendal fails to deserialize the multistatus with
// the same silent read-only latch.
func (b *Backend) servePropfind(w http.ResponseWriter, r *http.Request, route cache.Route) {
	decoded := r.PathValue(b.tail)

	// A directory: trailing slash, or the bare mount root (the parent of a TOP-LEVEL
	// key like sccache's ".sccache_check" probe).
	if decoded == "" || strings.HasSuffix(decoded, "/") {
		writeMultistatus(w, r.URL.Path, -1, time.Now().UTC())

		return
	}

	kind, key, err := b.policy.Classify(decoded)
	if err != nil {
		// Not a legal object key, so nothing can exist there. The honest stat answer is
		// 404 -- and never 400, which latches sccache read-only.
		http.NotFound(w, r)

		return
	}

	meta, err := b.deps.Blobs.Stat(r.Context(), route.Ref(b.policy.Namespace, kind, key))
	if err != nil || !meta.Exists {
		http.NotFound(w, r)

		return
	}

	modified := meta.UpdatedAt
	if modified.IsZero() {
		modified = time.Now().UTC()
	}

	writeMultistatus(w, r.URL.Path, meta.Size, modified)
}

// writeMultistatus renders the one-response 207 body. size < 0 means a collection;
// otherwise a file of that size.
func writeMultistatus(w http.ResponseWriter, href string, size int64, modified time.Time) {
	// RFC1123Z is an RFC 2822 date-time ("Mon, 02 Jan 2006 15:04:05 -0700"): a numeric
	// zone, which the RFC 2822 grammar opendal parses accepts unambiguously.
	stamp := modified.Format(time.RFC1123Z)

	resourcetype := "<D:resourcetype><D:collection/></D:resourcetype>"
	length := ""

	if size >= 0 {
		resourcetype = "<D:resourcetype/>"
		length = "<D:getcontentlength>" + strconv.FormatInt(size, 10) + "</D:getcontentlength>"
	}

	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<D:multistatus xmlns:D="DAV:">` +
		`<D:response>` +
		`<D:href>` + xmlEscape(href) + `</D:href>` +
		`<D:propstat>` +
		`<D:prop>` +
		resourcetype +
		length +
		`<D:getlastmodified>` + stamp + `</D:getlastmodified>` +
		`</D:prop>` +
		`<D:status>HTTP/1.1 200 OK</D:status>` +
		`</D:propstat>` +
		`</D:response>` +
		`</D:multistatus>`

	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus) // 207
	_, _ = io.WriteString(w, body)
}

// serveMkcol acknowledges a collection creation. There are no directories in a blob
// store, so this is a pure no-op that returns 201 -- opendal treats ANY non-2xx here as a
// failed write and latches sccache read-only, so a 201 is load-bearing even though it
// creates nothing.
func (b *Backend) serveMkcol(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusCreated) // 201
}

// xmlEscape escapes the five XML metacharacters in the href. The path is
// /cache/{org}/{project}/sccache/... -- validated slugs and hex, so metacharacters are
// not expected, but the href must be well-formed XML regardless.
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
)

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }
