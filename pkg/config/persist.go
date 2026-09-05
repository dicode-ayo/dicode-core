package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dicode/dicode/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// ErrConcurrentModification is returned by MergeTaskOverride when the
// dicode.yaml file's mtime has changed since the caller read it. Callers
// should surface this as 409 Conflict and prompt the operator to reload.
var ErrConcurrentModification = errors.New("config file modified externally")

// MergeTaskOverride applies a JSON Merge Patch (RFC 7396) to the YAML at
// spec.entries.<source>.overrides.entries.<sub> in dicode.yaml at path.
//
// patch is a JSON object whose keys mirror taskset.Overrides yaml tags.
// Scalars and objects in patch SET keys, JSON null DELETES keys, missing
// keys leave existing values untouched. Maps merge recursively, lists
// replace whole.
//
// The implementation reads the file (rejects if mtime != expectedMtime),
// unmarshals the entire document into map[string]any (consistent with
// existing persistConfig — comments not preserved), applies the merge,
// then writes via temp-file + atomic rename in the same directory.
//
// If after the merge an entry's overrides.entries.<sub> map is empty it
// is pruned; if overrides.entries becomes empty it is pruned; if the
// entry's overrides becomes empty it is pruned. Avoids YAML cruft from
// repeated toggles.
func MergeTaskOverride(path, taskID string, patch json.RawMessage, expectedMtime time.Time) error {
	source, sub, ok := SplitTaskID(taskID)
	if !ok {
		return fmt.Errorf("task ID %q has no source separator", taskID)
	}
	if source == "" || sub == "" {
		return fmt.Errorf("task ID %q has empty source or sub key", taskID)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.ModTime().Equal(expectedMtime) {
		return ErrConcurrentModification
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}

	specBlock := getMap(doc, "spec")
	entries := getMap(specBlock, "entries")
	if entries == nil {
		return fmt.Errorf("spec.entries missing in %s", path)
	}
	entry := getMap(entries, source)
	if entry == nil {
		return fmt.Errorf("source %q not found in spec.entries", source)
	}
	overrides := getMap(entry, "overrides")
	if overrides == nil {
		overrides = map[string]any{}
	}
	overrideEntries := getMap(overrides, "entries")
	if overrideEntries == nil {
		overrideEntries = map[string]any{}
	}
	subOverrides := getMap(overrideEntries, sub)
	if subOverrides == nil {
		subOverrides = map[string]any{}
	}

	var patchObj map[string]any
	if err := json.Unmarshal(patch, &patchObj); err != nil {
		return fmt.Errorf("decode patch: %w", err)
	}
	mergeMap(subOverrides, patchObj)
	pruneEmptyMaps(subOverrides)

	if len(subOverrides) == 0 {
		delete(overrideEntries, sub)
	} else {
		overrideEntries[sub] = subOverrides
	}
	if len(overrideEntries) == 0 {
		delete(overrides, "entries")
	} else {
		overrides["entries"] = overrideEntries
	}
	if len(overrides) == 0 {
		delete(entry, "overrides")
	} else {
		entry["overrides"] = overrides
	}
	entries[source] = entry
	specBlock["entries"] = entries
	doc["spec"] = specBlock

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	return AtomicWriteFile(path, out, fi.Mode().Perm())
}

// AtomicWriteFile writes data to path using a same-directory temp file and
// rename, so a crash mid-write leaves the original untouched. perm is the
// file mode applied before rename — pass the original file's perm to
// preserve it across replacements. perm=0 will leave the written file
// unreadable; callers should pass at minimum 0o400 or the original perm.
//
// Used by MergeTaskOverride and webui's persistConfig / apiSaveConfigRaw.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	return fsutil.WriteFileAtomic(path, data, perm)
}

// getMap returns a typed map from a parent map's key, normalising both
// map[string]any and map[any]any (yaml.v3 yields the latter for nested
// mappings). Returns nil if the key is missing or not a map.
func getMap(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return nil
	}
	v, ok := parent[key]
	if !ok || v == nil {
		return nil
	}
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			out[ks] = val
		}
		return out
	}
	return nil
}

// pruneEmptyMaps drops keys whose value merged down to an empty map, so
// clearing the last param override leaves the file as clean as it was before
// the first one — `params: {}` parses to the same nothing it renders as, and
// the cascade below can only prune an entry that holds no keys at all.
func pruneEmptyMaps(m map[string]any) {
	for k, v := range m {
		switch typed := v.(type) {
		case map[string]any:
			if len(typed) == 0 {
				delete(m, k)
			}
		case map[any]any:
			if len(typed) == 0 {
				delete(m, k)
			}
		}
	}
}

// mergeMap applies JSON Merge Patch (RFC 7396) semantics: keys in patch
// replace dst, JSON null deletes from dst, nested maps merge recursively,
// non-map values replace.
func mergeMap(dst, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(dst, k)
			continue
		}
		patchMap, patchIsMap := v.(map[string]any)
		dstSub := getMap(dst, k)
		if patchIsMap && dstSub != nil {
			mergeMap(dstSub, patchMap)
			dst[k] = dstSub
		} else {
			dst[k] = v
		}
	}
}
