package runtime

import (
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func TestIsWildcardEnvEntry(t *testing.T) {
	cases := []struct {
		name string
		e    task.EnvEntry
		want bool
	}{
		{"bare prefix glob", task.EnvEntry{Name: "GITHUB_*"}, true},
		{"bare exact name", task.EnvEntry{Name: "GITHUB_TOKEN"}, false},
		{"lone star", task.EnvEntry{Name: "*"}, false},
		{"pattern with from", task.EnvEntry{Name: "GITHUB_*", From: "env:X"}, false},
		{"pattern with secret", task.EnvEntry{Name: "GITHUB_*", Secret: "k"}, false},
		{"pattern with value", task.EnvEntry{Name: "GITHUB_*", Value: "v"}, false},
		{"empty", task.EnvEntry{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsWildcardEnvEntry(c.e); got != c.want {
				t.Errorf("IsWildcardEnvEntry(%+v) = %v, want %v", c.e, got, c.want)
			}
		})
	}
}

func TestWildcardEnvNames_MatchesPrefix(t *testing.T) {
	// A collision-proof prefix so the exact-equality assertion holds regardless
	// of ambient env (CI hosts carry many GITHUB_*/DICODE_* vars of their own).
	t.Setenv("WILDTEST_A", "1")
	t.Setenv("WILDTEST_B", "2")
	t.Setenv("WILDOTHER_C", "3") // different prefix → not matched

	spec := &task.Spec{}
	spec.Permissions.Env = []task.EnvEntry{{Name: "WILDTEST_*"}}

	got := WildcardEnvNames(spec)
	want := []string{"WILDTEST_A", "WILDTEST_B"} // sorted, de-duplicated
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WildcardEnvNames = %v, want %v", got, want)
	}
}

// A pattern that prefix-matches the daemon credentials / IPC vars must exclude
// every one of them. Asserted as an absence check (not exact equality) so it
// holds even when CI sets other DICODE_* vars.
func TestWildcardEnvNames_ExcludesDenylistAndIPC(t *testing.T) {
	t.Setenv("DICODE_MASTER_KEY", "root")
	t.Setenv("DICODE_API_KEY", "admin")
	t.Setenv("DICODE_MCP_API_KEY", "mcp")
	t.Setenv("DICODE_SOCKET", "/sock")
	t.Setenv("DICODE_TOKEN", "tok")

	spec := &task.Spec{}
	spec.Permissions.Env = []task.EnvEntry{{Name: "DICODE_*"}}

	got := make(map[string]bool)
	for _, n := range WildcardEnvNames(spec) {
		got[n] = true
	}
	for _, blocked := range []string{
		"DICODE_MASTER_KEY", "DICODE_API_KEY", "DICODE_MCP_API_KEY",
		"DICODE_SOCKET", "DICODE_TOKEN",
	} {
		if got[blocked] {
			t.Errorf("DICODE_* leaked denylisted/IPC var %q", blocked)
		}
	}
}

func TestWildcardEnvNames_NoPatternsReturnsNil(t *testing.T) {
	spec := &task.Spec{}
	spec.Permissions.Env = []task.EnvEntry{{Name: "GITHUB_TOKEN"}}
	if got := WildcardEnvNames(spec); got != nil {
		t.Errorf("WildcardEnvNames with no patterns = %v, want nil", got)
	}
	if got := WildcardEnvNames(nil); got != nil {
		t.Errorf("WildcardEnvNames(nil) = %v, want nil", got)
	}
}
