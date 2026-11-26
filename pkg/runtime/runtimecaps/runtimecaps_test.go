package runtimecaps

import (
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func TestFor(t *testing.T) {
	cases := []struct {
		rt   task.Runtime
		want Caps
	}{
		{task.RuntimeDeno, Caps{Suspend: true, SecretOutput: true}},
		{"", Caps{Suspend: true, SecretOutput: true}},
		{"js", Caps{Suspend: true, SecretOutput: true}},
		{RuntimePython, Caps{Suspend: true, SecretOutput: true}},
		{task.RuntimeDocker, Caps{LocalImageBuild: true}},
		{task.RuntimePodman, Caps{LocalImageBuild: true}},
		{"ruby", Caps{Suspend: true}},
	}
	for _, tc := range cases {
		t.Run(string(tc.rt), func(t *testing.T) {
			got := For(tc.rt)
			if got.Suspend != tc.want.Suspend {
				t.Errorf("Suspend = %v, want %v", got.Suspend, tc.want.Suspend)
			}
			if got.SecretOutput != tc.want.SecretOutput {
				t.Errorf("SecretOutput = %v, want %v", got.SecretOutput, tc.want.SecretOutput)
			}
			if got.LocalImageBuild != tc.want.LocalImageBuild {
				t.Errorf("LocalImageBuild = %v, want %v", got.LocalImageBuild, tc.want.LocalImageBuild)
			}
		})
	}
}
