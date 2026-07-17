package bazel

import (
	"errors"
	"strconv"
	"strings"
)

// A ByteStream resource name carries the whole addressing tuple in one string --
// there is no instance_name field on ByteStream, unlike every other REAPI RPC. The
// grammar is NOT in bytestream.proto; it is doc comments on the CAS service:
//
//	write: {instance}/uploads/{uuid}/blobs/{digest_function/}{hash}/{size}{/metadata}
//	read:  {instance}/blobs/{digest_function/}{hash}/{size}
//
// Two facts make a naive parser reject 100% of stock traffic, and both are load-
// bearing here:
//
//   - instance_name MAY SPAN MULTIPLE SEGMENTS -- it is "{org}/{project}", so it
//     CONTAINS A SLASH. Never split positionally. Scan left-to-right for the FIRST
//     segment that is exactly "uploads", "blobs" or "compressed-blobs"; everything
//     before it is the instance. Matching whole SEGMENTS, not substrings, is what
//     lets an instance like "acme/blobstore" through -- "blobstore" != "blobs". And
//     the three markers are RESERVED SLUGS (CLAUDE.md), so an instance segment can
//     never equal one, which is what makes "first marker" unambiguous.
//   - For SHA256 the {digest_function/} segment is OMITTED. So after the marker the
//     tail is 2 segments (sha256: hash/size) or 3 (blake3: fn/hash/size). NEVER a
//     fixed count, and NEVER require the digest-function segment.
type resourceName struct {
	// instance is everything before the marker: "{org}/{project}". Authorization
	// resolves the route from it; the parser does not validate its shape.
	instance string

	// hash and size are the digest. The {uuid} on a write is parsed and DISCARDED --
	// moon reuses ONE uuid for every concurrent upload, so keying any state on it
	// corrupts blobs.
	hash string
	size int64
}

var (
	// errInvalidResource -> InvalidArgument. An unparseable resource name.
	errInvalidResource = errors.New("bazel: unparseable resource name")

	// errCompressedResource -> Unimplemented. We advertise IDENTITY only; a
	// compressed-blobs resource is a format we did not promise to serve.
	errCompressedResource = errors.New("bazel: compressed-blobs is not supported")
)

// parseResourceName parses a ByteStream Read or Write resource name.
func parseResourceName(name string) (resourceName, error) {
	segs := strings.Split(name, "/")

	marker := -1

	for i, s := range segs {
		if s == "uploads" || s == "blobs" || s == "compressed-blobs" {
			marker = i

			break
		}
	}

	if marker < 0 {
		return resourceName{}, errInvalidResource
	}

	instance := strings.Join(segs[:marker], "/")

	switch segs[marker] {
	case "compressed-blobs":
		return resourceName{}, errCompressedResource

	case "uploads":
		// A write: uploads/{uuid}/{blobs|compressed-blobs}/{tail}. The uuid is segs
		// [marker+1] and is discarded; the blob marker is segs[marker+2].
		if marker+2 >= len(segs) {
			return resourceName{}, errInvalidResource
		}

		switch segs[marker+2] {
		case "compressed-blobs":
			return resourceName{}, errCompressedResource
		case "blobs":
			return parseBlobTail(instance, segs[marker+3:])
		default:
			return resourceName{}, errInvalidResource
		}

	default: // "blobs"
		return parseBlobTail(instance, segs[marker+1:])
	}
}

// parseBlobTail extracts {hash}/{size} from the segments after a "blobs" marker,
// tolerating an optional leading {digest_function} segment and any trailing
// metadata (writes may append it).
//
// The digest-function segment, when present, is a name like "blake3" or "sha256" --
// never all hex, because those names carry non-hex letters. So "the first segment is
// all hex" reliably means the function was omitted (the SHA256 form) and the tail
// starts hash/size; otherwise the function is present and the tail is fn/hash/size.
func parseBlobTail(instance string, tail []string) (resourceName, error) {
	if len(tail) < 2 {
		return resourceName{}, errInvalidResource
	}

	var hash, sizeStr string

	switch {
	case isHex(tail[0]) && isDecimal(tail[1]):
		hash, sizeStr = tail[0], tail[1] // SHA256 form: digest function omitted
	case len(tail) >= 3:
		hash, sizeStr = tail[1], tail[2] // digest function present
	default:
		return resourceName{}, errInvalidResource
	}

	if hash == "" || !isHex(hash) {
		return resourceName{}, errInvalidResource
	}

	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil || size < 0 {
		return resourceName{}, errInvalidResource
	}

	return resourceName{instance: instance, hash: hash, size: size}, nil
}

// isHex reports whether s is non-empty and all hex digits (either case). A digest
// function name ("blake3", "sha256") always fails this -- that is the whole point.
func isHex(s string) bool {
	if s == "" {
		return false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}

	return true
}

// isDecimal reports whether s is a non-empty run of ASCII digits.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}

	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}
