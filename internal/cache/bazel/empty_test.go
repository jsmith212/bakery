package bazel

import "testing"

func TestEmptyBlobHelpers(t *testing.T) {
	const empty = emptySHA256Hex
	const other = "0000000000000000000000000000000000000000000000000000000000000000"

	if !isEmpty(empty, 0) {
		t.Error("isEmpty(emptyHash, 0) = false, want true")
	}

	if isEmpty(empty, 5) {
		t.Error("isEmpty(emptyHash, 5) = true: a non-zero size is never the empty blob")
	}

	if isEmpty(other, 0) {
		t.Error("isEmpty(otherHash, 0) = true: size 0 with a non-empty hash is NOT the empty blob")
	}

	// A size-0 digest with any OTHER hash is malformed, not the empty blob.
	if !zeroSizeButNotEmpty(other, 0) {
		t.Error("zeroSizeButNotEmpty(otherHash, 0) = false, want true")
	}

	if zeroSizeButNotEmpty(empty, 0) {
		t.Error("zeroSizeButNotEmpty(emptyHash, 0) = true: the real empty blob is not malformed")
	}

	if zeroSizeButNotEmpty(other, 9) {
		t.Error("zeroSizeButNotEmpty(otherHash, 9) = true: a non-zero size is not the zero-size case")
	}
}
