package httpblob

import (
	"net/http"

	"github.com/jsmith212/bakery/internal/blob"
)

// serveDelete removes an object's metadata and answers 204.
//
// ccache uses DELETE as a first-class verb (its remote backend evicts entries) and treats
// ANY non-2xx -- including the 405 an UNregistered route returns -- as a hard failure that
// latches the WHOLE backend off, reads included, for that translation unit. So a delete is
// a real 204 whether or not the key was present: blob.Service.Delete already distinguishes
// present from absent, and neither is an error here.
//
// It removes METADATA only. The bytes are the GC's job -- another project may dedup onto
// the same blob, and cache_objects_blob_fk (ON DELETE RESTRICT) is what actually keeps a
// referenced blob's bytes alive; Delete never touches storage.
func (b *Backend) serveDelete(w http.ResponseWriter, r *http.Request, ref blob.Ref) {
	if _, err := b.deps.Blobs.Delete(r.Context(), ref); err != nil {
		http.Error(w, "delete", http.StatusInternalServerError) // 500

		return
	}

	w.WriteHeader(http.StatusNoContent) // 204
}
