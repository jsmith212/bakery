package httpblob

import (
	"io"
	"net/http"
	"strconv"

	"github.com/jsmith212/bakery/internal/blob"
)

// ServeObject writes one stored object's bytes as an HTTP response body.
//
// It is EXPORTED for the OCI backend (M5), which is a pull-through proxy and so
// cannot use httpblob.Backend at all: its miss path fetches upstream, ingests, and
// only then serves, a flow httpblob.Policy has no slot for, and its BuildKit route
// family (/v2/{org}/{project}/...) is not under /cache/ to begin with. What it does
// share is exactly this: the last twenty lines, where a blob.Service reader becomes a
// response. Registry clients pull multi-hundred-megabyte layers and resume
// interrupted pulls with Range, so getting 206/416/If-Range wrong there is not
// cosmetic -- and distribution's own blobserver does the same thing, for the same
// reason: it calls http.ServeContent.
//
// THE CALLER MUST HAVE SET Content-Type BEFORE CALLING. This is not tidiness: with no
// Content-Type set, ServeContent sniffs the first 512 bytes and seeks back, adding a
// read to every hit -- and for OCI the media type is a STORED FACT that must be
// echoed verbatim (containerd trusts it as ocispec.Descriptor.MediaType), so guessing
// it here would be wrong in the one place it matters most. The caller also owns
// Docker-Content-Digest, ETag and any other headers; this function writes only what
// ServeContent writes.
//
// The seek type-assertion is what buys Range: the local store's Get returns an
// *os.File, which survives unwrapped through Instrumented.Get and Service.Get. The
// fallback is for the deferred S3 store, whose body is not a Seeker -- there it is a
// plain 200 with an explicit Content-Length and no Range support, which every client
// tolerates (it simply restarts an interrupted pull).
//
// It does NOT close rc. The caller opened it and the caller closes it.
func ServeObject(w http.ResponseWriter, r *http.Request, meta blob.Meta, rc io.Reader) {
	if rs, ok := rc.(io.ReadSeeker); ok {
		http.ServeContent(w, r, "", meta.UpdatedAt, rs) // Range/206/416/If-Range for free

		return
	}

	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
