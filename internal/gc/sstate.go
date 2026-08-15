package gc

import (
	"regexp"
	"strings"
)

// THE sstate ROOT DERIVATION (spec §5).
//
// There is no gc_root column and there is not going to be one: the reachability
// root of an sstate object is already IN ITS NAME. sstate.bbclass builds the object
// filename as SSTATE_PKGSPEC + BB_UNIHASH + "_" + task + extension, so an object is
// reachable exactly when a hashserv row on the paired backend still carries that
// unihash. The derivation reads the name back.
//
// It is the ONE place in Bakery that parses a client's key, and every rule below is
// there because a stricter version of it deletes live objects:
//
//   - THREE sidecar suffixes, not two: .siginfo, .sig AND .done. yocto.md §1.4 names
//     all three; a .done file only reaches a mirror when someone rsync'd a whole
//     sstate-cache, which is exactly the deployment shape that has no hashserv rows
//     to fall back on.
//   - bb_unihash is captured as [^_]*, matching bitbake's own RE_SSTATE_PKGSPEC. It
//     is safe because a unihash is hex and cannot contain the '_' that terminates it.
//   - The unihash is validated as ^[0-9a-f]{1,64}$ and NEVER {64}: Scarthgap (the
//     release the conformance suite pins) emits 40-hex unihashes, and the 64-hex
//     UNIHASH_REGEX is a 2.10+ addition. Requiring 64 refuses a legitimate client.
//   - An UNPARSEABLE key is legal, not an error: do_populate_lic and friends use
//     SSTATE_SWSPEC, whose arch fields are EMPTY. Such a key is treated as
//     UNREACHABLE and dies on age alone, which is the conservative direction only
//     because the rule is conjunctive.

// sstateSidecars are the suffixes stripped before the spec is matched, longest
// first so ".tar.zst.siginfo" is never left holding a partial match.
var sstateSidecars = []string{".siginfo", ".sig", ".done"}

// sstatePkgspec is bitbake's RE_SSTATE_PKGSPEC
// (scripts/sstate-cache-management.py), transcribed:
//
//	sstate:<pn>:<package_target>:<pv>:<pr>:<sstate_pkgarch>:<sstate_version>:<bb_unihash>_<task>...
//
// Every field is [^:]* -- EMPTY IS LEGAL and is what an SSTATE_SWSPEC object looks
// like (sstate:zlib:::1.3.1:r0::14:<hash>_populate_lic.tar.zst). The last two fields
// are [^_]* because the underscore is the separator between the unihash and the task
// name, and the greedy-then-backtrack behaviour of the pair is what lets a version
// field containing ':' resolve correctly.
//
// It is anchored at the start (bitbake uses re.match) and deliberately NOT at the
// end: the extension varies by release (.tgz through Honister, .tar.zst since
// Kirkstone) and nothing downstream cares which one it is.
var sstatePkgspec = regexp.MustCompile(
	`^sstate:[^:]*:[^:]*:[^:]*:[^:]*:[^:]*:[^_]*:([^_]*)_`,
)

// sstateUnihash is the validity rule from spec §5 step 3. Hex-only because the
// unihash lands in a FILENAME, and 1..64 because the release the build pins reports
// 40-hex ones.
var sstateUnihash = regexp.MustCompile(`^[0-9a-f]{1,64}$`)

// deriveUnihash extracts BB_UNIHASH from an sstate object key.
//
// The key is the full path under the mount -- "universal/9f/3c/sstate:..." -- so the
// two-character shard directories and the optional NATIVELSBSTRING prefix are
// stripped by taking the basename. ok=false means "this key names no unihash we can
// see", which the caller must treat as UNREACHABLE and never as an error.
func deriveUnihash(key string) (string, bool) {
	name := key
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}

	// Repeated because the sidecars stack in practice: a rsync'd cache holds
	// <name>.tar.zst.siginfo and, next to it, <name>.tar.zst.done.
	for stripped := true; stripped; {
		stripped = false

		for _, suffix := range sstateSidecars {
			if strings.HasSuffix(name, suffix) {
				name = strings.TrimSuffix(name, suffix)
				stripped = true

				break
			}
		}
	}

	m := sstatePkgspec.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}

	if !sstateUnihash.MatchString(m[1]) {
		return "", false
	}

	return m[1], true
}

// coverage tracks spec §5's observability gate for one sstate backend: the fraction
// of scanned keys whose unihash resolved to a surviving row on the paired hashserv
// backend.
//
// WHY THE FRACTION IS A METRIC AND A GUARD, not just a number: most real
// deployments (rsync'd mirrors, BB_HASHSERVE=auto, no hashserv backend at all) hold
// ZERO unihash rows, which collapses this policy to age-only retention -- silently,
// and while looking completely healthy. That is CORRECT there. It is a broken
// derivation everywhere else, and the two are indistinguishable from coverage
// alone, which is why the guard also asks whether the paired backend holds any rows
// at all.
type coverage struct {
	scanned  int64
	resolved int64
}

func (c *coverage) fraction() float64 {
	if c.scanned == 0 {
		return 0
	}

	return float64(c.resolved) / float64(c.scanned)
}
