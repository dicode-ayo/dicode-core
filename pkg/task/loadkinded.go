package task

import (
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// peekKind reads only the top-level kind field from dir's task manifest
// (task.yaml, falling back to task.yml — see openTaskSpecFile) without a
// full decode. Returns "" when the field is absent (legacy task.yaml).
// Defined here (not via pkg/taskset.DetectKind) to avoid an import cycle:
// pkg/taskset imports pkg/task, not vice versa.
func peekKind(dir string) (string, error) {
	f, specPath, err := openTaskSpecFile(dir, "")
	if err != nil {
		return "", fmt.Errorf("open %s: %w", specPath, err)
	}
	defer f.Close()
	var h struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.NewDecoder(f).Decode(&h); err != nil {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s is empty", specPath)
		}
		return "", fmt.Errorf("read kind from %s: %w", specPath, err)
	}
	return h.Kind, nil
}

// LoadKindedDir loads dir's task manifest as the appropriate kind. A missing
// or "Task" kind loads a *Spec; "PipelineTask" loads a *PipelineTask. Any
// other kind is an error.
func LoadKindedDir(dir string, extras map[string]string) (Kinded, error) {
	kind, err := peekKind(dir)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "", KindTask:
		return LoadDirWithVars(dir, extras)
	case KindPipelineTask:
		return LoadPipelineDir(dir, extras)
	default:
		return nil, fmt.Errorf("task manifest in %s: unknown kind %q (expected %q or %q)", dir, kind, KindTask, KindPipelineTask)
	}
}
