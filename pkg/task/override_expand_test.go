package task

import (
	"path/filepath"
	"testing"
)

// ExpandOverrideLayer returns an expanded copy and leaves the caller's layer
// alone: taskset resolution re-runs on every reconcile tick against override
// objects the caller holds for the daemon's lifetime.
func TestExpandOverrideLayer_LeavesInputUntouched(t *testing.T) {
	const (
		authoredPath   = "${DATADIR}/dev-clones"
		authoredSecret = "${DATADIR}-secret"
		authoredParam  = "${TASK_DIR}/cfg.json"
	)
	in := &Overrides{
		Fs:     []FSEntry{{Path: authoredPath, Permission: "rw"}},
		Env:    []EnvEntry{{Name: "TOKEN", Secret: authoredSecret}},
		Params: ParamOverrides{{Name: "cfg", Default: authoredParam}},
	}

	taskDir := filepath.Join("/srv", "tasks", "agent")
	dataDir := filepath.Join("/var", "lib", "dicode")
	out := ExpandOverrideLayer(in, taskDir, map[string]string{VarDataDir: dataDir})

	if out == in {
		t.Fatal("ExpandOverrideLayer returned the input pointer; must return a copy")
	}

	if want := filepath.Join(dataDir, "dev-clones"); out.Fs[0].Path != want {
		t.Errorf("copy fs path = %q, want %q", out.Fs[0].Path, want)
	}
	if want := filepath.Join(taskDir, "cfg.json"); out.Params[0].Default != want {
		t.Errorf("copy param default = %q, want %q", out.Params[0].Default, want)
	}

	if in.Fs[0].Path != authoredPath {
		t.Errorf("input fs path mutated: %q, want %q", in.Fs[0].Path, authoredPath)
	}
	if in.Env[0].Secret != authoredSecret {
		t.Errorf("input env secret mutated: %q, want %q", in.Env[0].Secret, authoredSecret)
	}
	if in.Params[0].Default != authoredParam {
		t.Errorf("input param default mutated: %q, want %q", in.Params[0].Default, authoredParam)
	}
}

func TestExpandOverrideLayer_NilInput(t *testing.T) {
	if got := ExpandOverrideLayer(nil, "/srv/tasks/agent", nil); got != nil {
		t.Errorf("ExpandOverrideLayer(nil) = %v, want nil", got)
	}
}
