package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// hashedScriptFiles are the script filenames folded into the content hash
// alongside task.yaml. Kept deterministic (alphabetical) so Hash is stable
// across invocations. Must cover every extension ScriptPath may resolve —
// missing one means script edits of that kind go undetected by the
// reconciler (see #157 for the historic miss on task.ts, the Deno default).
var hashedScriptFiles = []string{
	"task.js",
	"task.mjs",
	"task.ts",
}

// Hash computes a content hash over task.yaml and the task's script file
// (any of the extensions in hashedScriptFiles).
// Used by the reconciler to detect task changes.
func Hash(dir string) (string, error) {
	h := sha256.New()

	names := append([]string{"task.yaml"}, hashedScriptFiles...)
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // a given task only uses one script extension; missing ones are expected
			}
			return "", fmt.Errorf("hash %s: %w", path, err)
		}
		// include filename as separator so hash(A+B) != hash(AB)
		fmt.Fprintf(h, "%s\x00", name)
		h.Write(data)
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// ScanDir scans the tasks/ directory in a repo and returns a map of
// taskID → content hash for all valid task directories.
func ScanDir(tasksDir string) (map[string]string, error) {
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("scan tasks dir %s: %w", tasksDir, err)
	}

	result := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(tasksDir, e.Name())
		// skip directories that don't contain task.yaml
		if _, err := os.Stat(filepath.Join(dir, "task.yaml")); os.IsNotExist(err) {
			continue
		}
		hash, err := Hash(dir)
		if err != nil {
			return nil, err
		}
		result[e.Name()] = hash
	}
	return result, nil
}
