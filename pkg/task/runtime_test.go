package task

import "testing"

func TestNormalizeRuntime(t *testing.T) {
	cases := []struct {
		in, want Runtime
	}{
		{"", RuntimeDeno},
		{"js", RuntimeDeno},
		{RuntimeDeno, RuntimeDeno},
		{RuntimeDocker, RuntimeDocker},
		{RuntimePodman, RuntimePodman},
		{"python", "python"},
		{"JS", "JS"},
		{" js", " js"},
	}
	for _, c := range cases {
		if got := NormalizeRuntime(c.in); got != c.want {
			t.Errorf("NormalizeRuntime(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
