package gc

import "testing"

// THE DERIVATION TABLE (spec §5). Every row here is a shape that exists in a real
// sstate cache, and three of them are shapes a "tidier" regex silently gets wrong:
// the swspec with empty arch fields (do_populate_lic), the 40-hex unihash Scarthgap
// reports, and the .done sidecar that only appears in an rsync'd mirror -- which is
// exactly the deployment that has no hashserv rows to fall back on.
func TestDeriveUnihash(t *testing.T) {
	t.Parallel()

	const (
		hash64 = "9f3c8a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7"
		hash40 = "2b7e151628aed2a6abf7158809cf4f3c762e7160"
	)

	tests := []struct {
		name string
		key  string
		want string
		ok   bool
	}{
		{
			name: "sharded native object",
			key: "universal/9f/3c/sstate:zlib-native:x86_64-linux:1.3.1:r0:x86_64:14:" +
				hash64 + "_populate_sysroot.tar.zst",
			want: hash64, ok: true,
		},
		{
			name: "unsharded target object",
			key: "2b/7e/sstate:zlib:core2-64-poky-linux:1.3.1:r0:core2-64:14:" +
				hash40 + "_package_write_rpm.tar.zst",
			want: hash40, ok: true,
		},
		{
			// SSTATE_SWSPEC: the arch fields are EMPTY and there is no universal/ prefix.
			// A regex that demanded non-empty fields would call this unparseable and --
			// because the rule is conjunctive -- would still not delete it early, but the
			// coverage metric would read 0 and the guard would refuse the whole backend.
			name: "swspec with empty arch fields",
			key:  "sstate:zlib:::1.3.1:r0::14:" + hash40 + "_populate_lic.tar.zst",
			want: hash40, ok: true,
		},
		{
			name: "siginfo sidecar",
			key: "universal/9f/3c/sstate:zlib-native:x86_64-linux:1.3.1:r0:x86_64:14:" +
				hash64 + "_populate_sysroot.tar.zst.siginfo",
			want: hash64, ok: true,
		},
		{
			name: "gpg signature sidecar",
			key:  "2b/7e/sstate:zlib:core2-64-poky-linux:1.3.1:r0:core2-64:14:" + hash40 + "_do_package.tar.zst.sig",
			want: hash40, ok: true,
		},
		{
			// Never requested from a mirror by bitbake; present whenever somebody rsync'd a
			// whole sstate-cache, which is precisely the deployment shape with no hashserv.
			name: "done stamp sidecar",
			key:  "2b/7e/sstate:zlib:core2-64-poky-linux:1.3.1:r0:core2-64:14:" + hash40 + "_do_package.tar.zst.done",
			want: hash40, ok: true,
		},
		{
			name: "legacy tgz extension",
			key:  "2b/7e/sstate:zlib:core2-64-poky-linux:1.3.1:r0:core2-64:10:" + hash40 + "_populate_sysroot.tgz",
			want: hash40, ok: true,
		},
		{
			name: "not an sstate name at all",
			key:  "universal/ab/cd/README.txt",
			want: "", ok: false,
		},
		{
			name: "too few fields",
			key:  "sstate:zlib:core2-64:1.3.1:" + hash40 + "_do_package.tar.zst",
			want: "", ok: false,
		},
		{
			// The validity rule is hex-only because the value lands in a FILENAME. It is
			// {1,64} and never {64}: Scarthgap -- the release the conformance suite pins --
			// emits 40-hex, and enforcing 64 refuses a legitimate client.
			name: "non-hex unihash is refused",
			key:  "sstate:zlib:::1.3.1:r0::14:NOTAHASH_populate_lic.tar.zst",
			want: "", ok: false,
		},
		{
			name: "unihash longer than 64 hex is refused",
			key:  "sstate:zlib:::1.3.1:r0::14:" + hash64 + "ab_populate_lic.tar.zst",
			want: "", ok: false,
		},
		{
			name: "INVALID placeholder from a build with no unihash",
			key:  "sstate:zlib:::1.3.1:r0::14:INVALID_populate_lic.tar.zst",
			want: "", ok: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := deriveUnihash(tc.key)
			if ok != tc.ok || got != tc.want {
				t.Errorf("deriveUnihash(%q) = %q, %v; want %q, %v", tc.key, got, ok, tc.want, tc.ok)
			}
		})
	}
}
