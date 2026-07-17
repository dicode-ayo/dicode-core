package task

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dicode/dicode/internal/pathguard"
	"gopkg.in/yaml.v3"
)

// maxHashedFileBytes bounds the contents read into the digest per file. The
// hash runs on every reconciler poll (~30s), so a committed large asset would
// re-read its full bytes each cycle. Files at or below this size are hashed by
// content; larger files fold in only a size+mtime descriptor — code files are
// small, so change-detection for task logic stays exact while bulk assets cost
// a stat instead of a full read.
const maxHashedFileBytes = 1 << 20 // 1 MiB

// heavyDirs are skipped wholesale during the walk: they hold no task code and
// can dominate walk cost. node_modules in particular can be a committed tree of
// thousands of files.
var heavyDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
}

// hashEntry is one file or symlink folded into a Hash digest. label is the
// string written into the digest to identify the entry — for in-dir entries
// it's the dir-relative slash path; for hash_include entries it's prefixed
// with "include:" (see Hash) so an include is extremely unlikely to collide
// with an in-dir path of the same name (only possible if a real file's own
// name literally starts with "include:", which the label+kind-separated
// digest still doesn't corrupt — it just means both entries fold into the
// same label bucket). abs is the absolute filesystem path read to fill in
// file content (unused for symlinks, whose target string is the content).
type hashEntry struct {
	label  string
	abs    string
	isLink bool
	target string
	// missing marks an include path that doesn't exist. Folded into the
	// digest as a sentinel (rather than silently contributing nothing) so
	// deleting or mistyping a hash_include entry still changes the hash.
	missing bool
	// info, when non-nil, is the os.FileInfo already obtained while
	// classifying a hash_include entry (via os.Lstat, on a confirmed
	// non-symlink path — so it's identical to what os.Stat would return).
	// Reused by the hashing loop below to avoid a second stat syscall on the
	// same path. In-dir entries from walkTree leave this nil (WalkDir's
	// fs.DirEntry doesn't carry a size), so they still take that loop's
	// single os.Stat.
	info os.FileInfo
}

// walkTree walks root recursively (skipping heavyDirs, same rules as the
// top-level dir walk) and appends one hashEntry per regular file or symlink,
// labelling each with labelPrefix + the root-relative slash path. Shared by
// Hash's own dir walk (labelPrefix "") and by hash_include directory entries
// (labelPrefix "include:<path>/").
func walkTree(root, labelPrefix string) ([]hashEntry, error) {
	var entries []hashEntry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && heavyDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		label := labelPrefix + filepath.ToSlash(rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", path, err)
			}
			entries = append(entries, hashEntry{label: label, isLink: true, target: target})
		case d.Type().IsRegular():
			entries = append(entries, hashEntry{label: label, abs: path})
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return entries, nil
}

// resolveInclude validates and resolves a single hash_include entry against
// absDir (dir's absolute form) and boundary (absDir's parent — see Hash),
// returning the absolute path Hash may safely read.
//
// includes are meant to reach a *sibling* task's file (see the Hash doc
// comment's "shared buildin helper library" example) — never an arbitrary
// host path, and never dir's own parent directory itself (which would pull
// every OTHER sibling task in the taskset into this one's digest, not just
// the intended sibling). So the resolved path is required to be a STRICT
// descendant of boundary: this bounds "../" traversal to exactly the
// sibling-task scope the feature is for, instead of letting a task.yaml
// author's hash_include value walk arbitrarily far up the filesystem (e.g.
// "../../../../etc/shadow", or bare ".." to pull in the whole taskset) to
// read and fold host files having nothing to do with the taskset into the
// digest.
//
// The lexical check alone isn't sufficient: go-git materializes
// repo-committed symlinks as real on-disk links (see internal/pathguard's
// doc comment), so a git-committed symlink *inside* the boundary could
// physically redirect an in-bounds-looking include somewhere else entirely
// (e.g. tasks/buildin/evil-link -> /etc, then hash_include:
// ["../evil-link/passwd"] is lexically inside tasks/buildin/ but physically
// reads /etc/passwd). pathguard.WithinResolved — the same helper
// pkg/trigger/webhook.go and pkg/webui/task_delete.go already use for this
// exact git-sourced-symlink class of bug — canonicalizes symlinks on both
// sides before the containment check, closing that gap.
//
// Both checks run before any filesystem read touches the resolved path, so
// an out-of-bounds entry is rejected rather than stat'd/read/walked.
func resolveInclude(absDir, boundary, inc string) (string, error) {
	candidate := filepath.Clean(filepath.Join(absDir, filepath.FromSlash(inc)))

	rel, err := filepath.Rel(boundary, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("hash_include %q escapes %s — hash_include may only reference paths strictly within the task's parent directory (sibling tasks/libraries), not arbitrary host paths", inc, boundary)
	}

	within, err := pathguard.WithinResolved(boundary, candidate)
	if err != nil {
		return "", fmt.Errorf("hash_include %q: %w", inc, err)
	}
	if !within {
		return "", fmt.Errorf("hash_include %q resolves outside %s through a symlink", inc, boundary)
	}
	return candidate, nil
}

// Hash computes a content hash over the regular files under dir, recursively,
// in sorted dir-relative path order. The runtime allows the task script to
// import any sibling file (the Deno sandbox allow-reads the whole task dir;
// Python imports sibling modules), so in-dir files are reachable code and form
// the fingerprint the approval gate (dicode.lock) keys re-approval off.
//
// Two bounded exclusions keep the per-poll cost flat without losing
// change-detection for task logic, which lives in small, top-level files:
//   - Files larger than maxHashedFileBytes fold in a size+mtime descriptor
//     instead of their contents (see the const); a content edit that preserves
//     both size and mtime is therefore not detected.
//   - The node_modules and .git subtrees are skipped wholesale (see heavyDirs).
//
// Consequently, code reachable only through those exclusions can change without
// re-triggering approval — acceptable under the trusted-author model, where the
// operator approves the source, not every transitive vendored file.
//
// Symlinks are never read through: the link's target string is folded in
// instead, so retargeting a link changes the hash without ever reading a
// file outside dir. Symlinked directories are not descended. Non-regular,
// non-symlink entries (sockets, devices) are skipped — reading them can
// block and they carry no task content.
//
// A missing dir hashes like an empty one (callers race task removal).
//
// includes (task.yaml's hash_include field) name additional files or
// directories, each resolved relative to dir, whose content is folded into
// the same digest — for a task that imports a shared module living outside
// its own dir (e.g. a sibling buildin task's helper library), editing that
// module would otherwise never perturb this task's hash and so would never
// re-trip the reconciler reload or the #392 approval-gate re-pend (#585). A
// directory include is walked recursively like dir itself; a file include is
// hashed as a single entry. Each include is labelled "include:<path>[/...]"
// in the digest so it is extremely unlikely to collide with an in-dir path
// of the same name (see hashEntry's doc comment), and a missing include
// folds in a sentinel rather than silently
// contributing nothing, so a mistyped or deleted include path still changes
// the hash instead of degrading to a no-op.
//
// Each include is resolved via resolveInclude, which bounds it to a STRICT
// descendant of dir's parent directory (the sibling-task scope this feature
// is for) — an entry that tries to escape that boundary (e.g.
// "../../../../etc/shadow", or bare ".." itself, which would otherwise pull
// in every sibling task in the taskset) is rejected with an error rather
// than read. Content folded in via hash_include still flows into the hash
// the approval gate persists in dicode.lock (a file its own doc comment
// documents as committable/shared — see pkg/approval/lock.go), so
// hash_include must never be pointed at a path containing secret material:
// prefer permissions.dicode.crypto / the secrets store for anything
// sensitive.
//
// hash_include is deliberately scoped to exactly one directory level above
// dir (immediate siblings) — the design doc's own motivating example (a
// sibling buildin task's shared helper module) is one hop, and it keeps the
// boundary a single, auditable rule instead of an arbitrary depth. A task
// nested more than one level below its taskset root that needs to reach a
// shared module further up is out of scope for this feature today.
func Hash(dir string, includes ...string) (string, error) {
	entries, err := walkTree(dir, "")
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", dir, err)
	}

	if len(includes) > 0 {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", dir, err)
		}
		boundary := filepath.Dir(absDir)

		for _, inc := range includes {
			if inc == "" {
				continue
			}
			label := "include:" + filepath.ToSlash(inc)
			incAbs, err := resolveInclude(absDir, boundary, inc)
			if err != nil {
				return "", err
			}
			info, err := os.Lstat(incAbs)
			if err != nil {
				if os.IsNotExist(err) {
					entries = append(entries, hashEntry{label: label, missing: true})
					continue
				}
				return "", fmt.Errorf("hash include %s: %w", inc, err)
			}
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(incAbs)
				if err != nil {
					return "", fmt.Errorf("hash include %s: readlink: %w", inc, err)
				}
				entries = append(entries, hashEntry{label: label, isLink: true, target: target})
			case info.IsDir():
				sub, err := walkTree(incAbs, label+"/")
				if err != nil {
					return "", fmt.Errorf("hash include %s: %w", inc, err)
				}
				entries = append(entries, sub...)
			case info.Mode().IsRegular():
				entries = append(entries, hashEntry{label: label, abs: incAbs, info: info})
			default:
				// Socket, device, FIFO, etc. — skip, matching walkTree's
				// handling of the same types encountered during a normal
				// dir walk: reading them can block indefinitely (e.g. a
				// FIFO with no writer), and they carry no task content.
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].label < entries[j].label })

	h := sha256.New()
	for _, e := range entries {
		// Label and kind act as separators so hash(A+B) != hash(AB) and a
		// regular file can never collide with a link whose target holds the
		// same bytes.
		kind := byte('f')
		switch {
		case e.isLink:
			kind = 'l'
		case e.missing:
			kind = 'm'
		}
		fmt.Fprintf(h, "%s\x00%c\x00", e.label, kind)
		switch {
		case e.isLink:
			h.Write([]byte(e.target))
		case e.missing:
			// No content to fold in beyond the label+kind separator above —
			// that alone is enough to make a missing include distinguishable
			// from both "not configured" and "present with any content".
		default:
			// Reuse the FileInfo already obtained while classifying a
			// hash_include entry (identical to what os.Stat would return,
			// since we confirmed it's not a symlink) instead of stat'ing the
			// same path a second time. In-dir entries from walkTree never
			// have one cached — they take the os.Stat below as before.
			info := e.info
			if info == nil {
				var statErr error
				info, statErr = os.Stat(e.abs)
				if statErr != nil {
					return "", fmt.Errorf("hash %s: %w", e.abs, statErr)
				}
			}
			if info.Size() > maxHashedFileBytes {
				fmt.Fprintf(h, "%d\x00%d", info.Size(), info.ModTime().UnixNano())
			} else {
				data, err := os.ReadFile(e.abs)
				if err != nil {
					return "", fmt.Errorf("hash %s: %w", e.abs, err)
				}
				h.Write(data)
			}
		}
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// ScanDir scans the tasks/ directory in a repo and returns a map of
// taskID → content hash for all valid task directories.
//
// This is the change-detection primitive for the flat local/git source
// types (pkg/source/local, pkg/source/git) — unlike pkg/taskset/source.go's
// resolver-based Source, it never fully loads a task.Spec (LoadDirWithVars),
// only checking that task.yaml exists. So a task's hash_include list (#585)
// has to be read separately here via readHashInclude — a best-effort partial
// parse, not the full loader — or hash_include would silently do nothing for
// any task registered through these two source types.
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
		yamlPath := filepath.Join(dir, "task.yaml")
		// skip directories that don't contain task.yaml
		if _, err := os.Stat(yamlPath); os.IsNotExist(err) {
			continue
		}
		hash, err := Hash(dir, readHashInclude(yamlPath)...)
		if err != nil {
			return nil, err
		}
		result[e.Name()] = hash
	}
	return result, nil
}

// readHashInclude does a lenient, minimal parse of yamlPath's hash_include
// field only — every other field, and any parse/read failure, is ignored
// (returns nil). ScanDir must tolerate an in-progress or otherwise invalid
// edit without aborting the whole reconciler scan the way the full loader
// (LoadDirWithVars) is allowed to fail loudly; a task.yaml this function
// can't parse just means "no includes counted this poll" — the task's own
// dir hash still reflects the edit, and the full loader reports the real
// error once the reconciler actually tries to load it.
func readHashInclude(yamlPath string) []string {
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil
	}
	var probe struct {
		HashInclude []string `yaml:"hash_include"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}
	return probe.HashInclude
}
