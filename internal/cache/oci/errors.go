package oci

import (
	"encoding/json"
	"net/http"
)

// The OCI distribution spec's error codes this package can produce.
//
// The read-only mirror emits only the first block. The WRITABLE buildcache namespace
// (registry.go) adds the second: containerd parses the envelope when the body carries
// one, so a push failure that answers a bare status is a client-side "unknown error"
// where it could have been a diagnosis.
const (
	// codeNameUnknown: the repository -- or, for us, the org/project/backend the
	// repository would live under -- does not exist here.
	codeNameUnknown = "NAME_UNKNOWN"
	// codeManifestUnknown: a manifest miss. THE most important code in this file.
	codeManifestUnknown = "MANIFEST_UNKNOWN"
	// codeBlobUnknown: a blob miss.
	codeBlobUnknown = "BLOB_UNKNOWN"
	// codeUnauthorized: a credential is required and was absent or rejected. Paired
	// with the Bearer challenge, never sent bare.
	codeUnauthorized = "UNAUTHORIZED"
	// codeUnsupported: an operation this proxy does not implement (on the MIRROR, every
	// write verb).
	codeUnsupported = "UNSUPPORTED"

	// codeDenied: authenticated, but the key is not write-scoped for this project. The
	// 403 half of the write gate; see core.authorizeWrite.
	codeDenied = "DENIED"
	// codeDigestInvalid: the ?digest= on a blob PUT is missing, is not sha256, or does
	// not match the bytes that arrived.
	codeDigestInvalid = "DIGEST_INVALID"
	// codeManifestInvalid: an empty or unreadable manifest body. It is NOT a
	// content judgement -- nothing here parses a manifest.
	codeManifestInvalid = "MANIFEST_INVALID"
	// codeUnknown: an internal fault on a PUSH. The read path never 5xxes (a 5xx makes
	// some clients retry rather than fall back); a push has nothing to fall back to, so
	// hiding a storage fault behind a 404 would tell BuildKit "cache not found" and
	// turn an outage into a silent cold build.
	codeUnknown = "UNKNOWN"
)

// errorBody is the spec's error envelope: {"errors":[{"code":..,"message":..}]}.
// `detail` is omitted rather than sent null -- it is where a naive implementation
// leaks internals, and no client reads it.
type errorBody struct {
	Errors []errorItem `json:"errors"`
}

type errorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError renders an OCI error response.
//
// THE STATUS CODE IS THE PART THAT MATTERS AND THE BODY IS THE PART THAT DOES NOT.
// Every registry client in existence parses the status and, at best, logs the body:
// containerd, BuildKit, podman and Docker Engine all treat any non-2xx from a MIRROR
// as "this mirror cannot serve me" and transparently fall back to the real registry.
// That is why:
//
//   - A MISS IS 404. Never 403 (which reads as a policy problem and, on the sstate
//     mount, provokes BitBake into a full-body GET), never a 200 with an empty body
//     (which a client will happily treat as a zero-length manifest), and never a 500.
//   - AN UNCONFIGURED BACKEND IS 404 TOO, with the same body a missing image gets. A
//     project with no oci backend row has no mount to serve and must not say so.
//   - NOTHING HERE 500s. A 5xx from a mirror is the one answer that makes some clients
//     retry rather than fall back, so an internal fault must degrade to "I do not have
//     it" and be loud in OUR metrics instead.
//
// The message is a fixed string per call site. It never carries the requested
// reference, a credential, or an upstream error -- an error body is attacker-visible
// output and the only thing it may reveal is the code.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(errorBody{
		Errors: []errorItem{{Code: code, Message: message}},
	})
}

// notFound is the miss. Every miss in this package goes through one of these three so
// that "a miss is a 404 with an OCI body" is one decision, made once.
func notFound(w http.ResponseWriter, code string) {
	writeError(w, http.StatusNotFound, code, "not found")
}
