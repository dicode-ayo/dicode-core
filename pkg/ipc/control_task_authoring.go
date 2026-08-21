package ipc

import (
	"errors"
	"sort"

	"github.com/dicode/dicode/pkg/task"
)

// taskFileSnapshot maps a task directory's file paths (directory-relative
// slash paths, task.Inventory's own labels) to a digest of everything about
// that file the gate's content hash folds in. Two snapshots taken around an
// authoring turn answer the only question the agent's reply cannot be trusted
// on: did anything on disk actually move (#755).
type taskFileSnapshot map[string]string

// snapshotTaskDir digests every file constituting the task rooted at dir.
// An empty dir is an error rather than an empty snapshot: "the directory could
// not be located" and "the directory is empty" lead to opposite verdicts about
// a turn that wrote nothing.
func snapshotTaskDir(dir string) (taskFileSnapshot, error) {
	if dir == "" {
		return nil, errors.New("task directory unknown")
	}
	metas, err := task.Inventory(dir)
	if err != nil {
		return nil, err
	}
	snap := make(taskFileSnapshot, len(metas))
	for _, m := range metas {
		// Kind separates a regular file from a symlink that happens to
		// carry no digest, and Target is a symlink's whole content.
		snap[m.Path] = m.Kind + "\x00" + m.Hash + "\x00" + m.Target
	}
	return snap, nil
}

// changedTaskFiles returns the paths that differ between two snapshots —
// added, modified and removed alike — sorted. An empty result means the
// directory is byte-identical.
func changedTaskFiles(before, after taskFileSnapshot) []string {
	var changed []string
	for path, digest := range after {
		if prev, ok := before[path]; !ok || prev != digest {
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed
}
