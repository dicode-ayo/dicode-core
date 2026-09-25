package deno

import "strconv"

// DefaultVersion is the Deno version downloaded when none is specified.
const DefaultVersion = "2.9.6"

// unixNetPermissionVersion is the first Deno release whose
// Deno.connect({transport:"unix"}) is gated behind --allow-net, scoped as
// "unix:<path>". A version below this rejects that scope syntax outright
// ("invalid port in 'unix:...'"), so the gate must be version-conditional,
// never unconditional.
const unixNetPermissionVersion = "2.9.0"

// RequiresUnixNetScope reports whether version needs an explicit
// "unix:<path>" entry in --allow-net to open a Unix-domain socket.
// Empty version defaults to DefaultVersion.
func RequiresUnixNetScope(version string) bool {
	if version == "" {
		version = DefaultVersion
	}
	return compareVersions(version, unixNetPermissionVersion) >= 0
}

// compareVersions compares two dot-separated numeric version strings
// component-by-component (never lexicographically — "2.10.0" must compare
// greater than "2.9.6"), returning -1/0/1. A missing trailing component
// compares as 0 ("2.9" == "2.9.0"). A non-numeric component compares as 0
// rather than panicking.
func compareVersions(a, b string) int {
	as := splitVersion(a)
	bs := splitVersion(b)
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func splitVersion(v string) []int {
	var out []int
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == '.' {
			part := v[start:i]
			n, err := strconv.Atoi(part)
			if err != nil {
				n = 0
			}
			out = append(out, n)
			start = i + 1
		}
	}
	return out
}
