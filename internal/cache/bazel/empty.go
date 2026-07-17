package bazel

// emptySHA256Hex is sha256("") -- the REAPI empty blob. REAPI: "Servers MUST behave
// as though empty blobs are always available, even if they have not been uploaded."
//
// This lives ONLY in the bazel backend, at a handful of call sites, and NEVER in
// blob.Service. blob.Service backs sstate, downloads, /ac, /cas and OCI; teaching it
// "e3b0c442 always exists" makes that true on the sstate mount, where it is false --
// and blob.Get would then Stat -> Exists -> store.Get -> ErrNotFound ->
// ErrDanglingMetadata, a 500 manufactured on a different backend. The empty blob is
// never stored: no blobs row, no cache_objects row, no refcount, not GC-visible.
const emptySHA256Hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// isEmpty reports whether (hash, size) is the REAPI empty blob. The gate is BOTH
// hash and size -- a size-0 digest with any OTHER hash is not the empty blob, it is
// a malformed digest (see zeroSizeButNotEmpty).
func isEmpty(hash string, size int64) bool {
	return size == 0 && hash == emptySHA256Hex
}

// zeroSizeButNotEmpty reports a malformed digest: size 0 paired with a hash that is
// not sha256(""). Callers that transfer bytes reject it with InvalidArgument, since
// no real blob has size 0 and a non-empty hash.
func zeroSizeButNotEmpty(hash string, size int64) bool {
	return size == 0 && hash != emptySHA256Hex
}
