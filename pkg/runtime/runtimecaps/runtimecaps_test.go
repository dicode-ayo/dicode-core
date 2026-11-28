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
		{"ruby", Caps{}},
	}
	for _, tc := range cases {
		t.Run(string(tc.rt), func(t *testing.T) {
			if got := For(tc.rt); got != tc.want {
				t.Errorf("For(%q) = %+v, want %+v", tc.rt, got, tc.want)
			}
		})
	}
}
