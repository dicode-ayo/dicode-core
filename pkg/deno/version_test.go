package deno

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.9.6", "2.9.6", 0},
		{"2.9.0", "2.9.0", 0},
		{"2.10.0", "2.9.6", 1},  // numeric, not lexicographic
		{"2.9.6", "2.10.0", -1}, // numeric, not lexicographic
		{"2.9", "2.9.0", 0},     // missing trailing component compares as 0
		{"2.9.0", "2.9", 0},
		{"2.8.9", "2.9.0", -1},
		{"2.8.99", "2.9.0", -1},
		{"2.9.0", "2.8.9", 1},
		{"3.0.0", "2.9.0", 1},
		{"1.9.9", "2.9.0", -1},
		{"2.x.0", "2.9.0", -1}, // non-numeric component compares as 0, no panic
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestRequiresUnixNetScope(t *testing.T) {
	cases := []struct {
		version string
		want    bool
	}{
		{"2.9.0", true}, // boundary: exactly the threshold
		{"2.9.6", true},
		{"2.10.0", true},
		{"3.0.0", true},
		{"2.8.9", false},
		{"2.8.99", false},
		{"2.3.3", false},
		{"", RequiresUnixNetScope(DefaultVersion)},
	}
	for _, c := range cases {
		if got := RequiresUnixNetScope(c.version); got != c.want {
			t.Errorf("RequiresUnixNetScope(%q) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestRequiresUnixNetScope_EmptyDefaultsToDefaultVersion(t *testing.T) {
	if RequiresUnixNetScope("") != RequiresUnixNetScope(DefaultVersion) {
		t.Errorf("RequiresUnixNetScope(\"\") must match RequiresUnixNetScope(DefaultVersion)")
	}
}
