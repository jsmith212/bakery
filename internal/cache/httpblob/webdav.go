package httpblob

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// servePropfind answers opendal's pre-write collection probe.
//
// sccache's opendal WebDAV backend PROPFINDs a path's parent before every write and
// MKCOLs any ancestor the PROPFIND reports missing. A blob store has NO directories, so
// we declare EVERY path an existing collection: opendal's mkcol loop then breaks on
// iteration 1 and sends zero MKCOLs, and the write proceeds straight to PUT.
//
// The 207 body is not cosmetic. opendal deserializes <D:getlastmodified> as a
// NON-optional String parsed as an RFC 2822 date, and needs
// <D:resourcetype><D:collection/></D:resourcetype> to classify the path as a directory.
// Omit either and opendal fails to deserialize the multistatus, its check() swallows the
// error into can_write=false, and sccache goes SILENTLY read-only for the whole process
// -- the exact ccache/hashserv-401 trap wearing a new hat.
func (b *Backend) servePropfind(w http.ResponseWriter, r *http.Request) {
	// RFC1123Z is an RFC 2822 date-time ("Mon, 02 Jan 2006 15:04:05 -0700"): a numeric
	// zone, which the RFC 2822 grammar opendal parses accepts unambiguously.
	modified := time.Now().UTC().Format(time.RFC1123Z)

	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<D:multistatus xmlns:D="DAV:">` +
		`<D:response>` +
		`<D:href>` + xmlEscape(r.URL.Path) + `</D:href>` +
		`<D:propstat>` +
		`<D:prop>` +
		`<D:resourcetype><D:collection/></D:resourcetype>` +
		`<D:getlastmodified>` + modified + `</D:getlastmodified>` +
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
