package approval

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// maxSnapshotFileBytes bounds the content read into a single file's diff
// snapshot. This is much smaller than task/hash.go's maxHashedFileBytes
// because a snapshot is held in memory for the life of the gate (not just
// digested and discarded) and is rendered as human-readable diff text, not
// folded into a hash.
const maxSnapshotFileBytes = 256 * 1024 // 256 KiB

// maxSnapshotFiles bounds the number of files captured per task snapshot.
// Snapshotting runs on every Admit — including every ~30s reconcile poll —
// so a pathologically large task dir must not make that otherwise-cheap
// operation slow or memory-hungry. Files beyond the cap still appear in the
// snapshot (so Diff can report that they exist / changed) but with a
// placeholder value instead of their content.
const maxSnapshotFiles = 200

// snapshotPlaceholder is the value stored for a file whose content was not
// captured — because it is binary (fails UTF-8 validation), larger than
// maxSnapshotFileBytes, or beyond maxSnapshotFiles. Diff never synthesizes a
// line-level unified diff for these; FileDiff.UnifiedDiff carries this
// string verbatim instead.
const snapshotPlaceholder = "binary or file too large to diff"

// snapshotHeavyDirs mirrors task/hash.go's heavyDirs: directories that hold
// no task content and can dominate walk cost. Kept as its own copy (rather
// than reaching into task's unexported map) so this package's walk isn't
// coupled to task's internals — same list, same rationale.
var snapshotHeavyDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
}

// valueLinePattern matches a YAML mapping entry named exactly "value" — e.g.
// `  value: "sk-live-secret"` or `- value: secret` — capturing the key
// portion (leading indentation, optional list-item dash, optional quotes
// around the key name, and the colon) in group 1 and the scalar (or, per
// blockScalarHeaderPattern below, block-scalar header) that follows in
// group 2. See redactValueLines for why this exists.
var valueLinePattern = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]*)?"?value"?[ \t]*:)[ \t]*(.*)$`)

// valueKeyIndentPattern captures just the leading-whitespace-plus-optional-
// list-dash portion of a matched "value:" line (i.e. valueLinePattern's group
// 1 minus the key name and colon) so redactValueLines can measure the
// "value:" key's own column for the block-scalar case, without changing
// valueLinePattern itself (its exact shape is documented and relied on
// elsewhere — see docs/concepts/security.md's Pending-Change Diff section).
var valueKeyIndentPattern = regexp.MustCompile(`^[ \t]*(?:-[ \t]*)?`)

// blockScalarHeaderPattern matches a YAML block-scalar header: a literal
// (`|`) or folded (`>`) indicator, optionally followed by a chomping
// indicator (`-`/`+`), an explicit indentation indicator (a single digit),
// in either order, then optional trailing whitespace and/or a comment. This
// intentionally doesn't distinguish which of the two optional indicators
// came first (YAML permits either order) or validate the digit range — a
// loose match here only ever leads to *more* redaction, never less.
var blockScalarHeaderPattern = regexp.MustCompile(`^[|>][+\-0-9]{0,2}[ \t]*(#.*)?$`)

// redactValueLines blanks every YAML "value:" line in content — both the
// inline-scalar form (`value: "secret"` on one line) and the block-scalar
// form (`value: |` or `value: >`, whose actual content lives on the
// following, more-indented lines) — replacing the secret material with
// redactedEnvValue while keeping enough structure intact that a diff still
// shows *that* the field changed, never *what* it changed to/from.
//
// This exists because task.EnvEntry.Value (pkg/task/spec.go) lets a task's
// permissions.env block carry a literal secret value inline in task.yaml (as
// opposed to Secret, a secrets-store key name reference). ContentHash
// (gate.go's sanitizePermissions/redactedEnvValue) already keeps that literal
// out of the committable dicode.lock; snapshotDir must keep it out of the
// pending-change diff surface (REST endpoint, WebUI panel, and the
// unauthenticated /approve/{token} confirm page) the same way — reusing the
// same redactedEnvValue placeholder for consistency.
//
// Deliberately generic: this redacts any line that looks like a YAML
// "value:" mapping entry in any snapshotted file, not just ones provably
// inside permissions.env — a snapshot has no field-path-aware YAML parse, and
// erring toward over-redaction is the correct tradeoff for a security fix. As
// of this writing task.Spec's YAML schema (pkg/task/spec.go) has exactly one
// top-level "value" key — EnvEntry.Value — so this should not blank any other
// legitimate field in a task.yaml; a non-YAML script file containing a
// literal "value:" line would still get blanked, but that false-positive
// blast radius is negligible next to the alternative of leaking a secret.
//
// Block-scalar handling is a heuristic, not a YAML parse: when a matched
// "value:" line's scalar portion is itself a block-scalar header
// (blockScalarHeaderPattern), the header line is preserved verbatim (so the
// diff still shows the field is present/changed) and every following line
// that is either blank or indented strictly more than the "value:" key's own
// column (valueKeyIndentPattern) is treated as swallowed content — the block
// ends at the first non-blank line at or below that indentation, or at
// end-of-file, matching YAML's indentation-based block-scalar termination
// rule closely enough for a defense-in-depth text scrub. The swallowed run is
// collapsed into a single redactedEnvValue placeholder line rather than
// preserving the original line count exactly — simpler to get right, and the
// diff already communicates "this block changed" via the preserved header
// line and surrounding hunk. Known imprecisions, both of which only ever
// over-redact: an explicit indentation indicator (e.g. "|2") is not decoded
// to find the "true" content column, and a trailing run of blank lines with
// no further content after them is swallowed even though strict YAML would
// attribute at most one trailing newline to the scalar.
func redactValueLines(content string) string {
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		m := valueLinePattern.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		scalar := strings.TrimRight(m[2], " \t\r")
		if !blockScalarHeaderPattern.MatchString(scalar) {
			// Inline scalar on the same line: blank just the value,
			// keep the "key:" prefix (existing, pre-block-scalar-fix
			// behavior).
			lines[i] = m[1] + " " + redactedEnvValue
			continue
		}
		// Block scalar: keep the header line, swallow the indented
		// content that follows into one placeholder line.
		keyIndent := len(valueKeyIndentPattern.FindString(lines[i]))
		j := i + 1
		swallowedAny := false
		for j < len(lines) {
			if strings.TrimSpace(lines[j]) == "" {
				swallowedAny = true
				j++
				continue
			}
			if leadingIndent(lines[j]) > keyIndent {
				swallowedAny = true
				j++
				continue
			}
			break
		}
		if swallowedAny {
			rest := append([]string{redactedEnvValue}, lines[j:]...)
			lines = append(lines[:i+1], rest...)
		}
		// i is left at the header line; the loop's i++ resumes scanning
		// at the placeholder (which cannot itself match valueLinePattern)
		// or, if nothing was swallowed, at the very next line.
	}
	return strings.Join(lines, "\n")
}

// leadingIndent returns the number of leading space/tab characters in s —
// used to compare a line's indentation column against a "value:" block
// scalar's key column. Byte-counted, not display-width-aware (a mix of tabs
// and spaces could in principle be measured differently); acceptable for
// this heuristic, same tradeoff as the rest of redactValueLines.
func leadingIndent(s string) int {
	n := 0
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	return n
}

// snapshotDir walks dir the same way task/hash.go's Hash walker does (same
// heavyDirs skip list, same fs.DirEntry.Info()-based regular-file
// classification) and returns a map of dir-relative slash path to file
// content, for Gate.Diff to compare snapshots taken at different times.
//
// This is a distinct walk from Hash's: the approval lock (pkg/approval/lock.go)
// only ever stores a content hash, never file bytes, so there is no
// "before" snapshot to diff against once a task's hash has changed and the
// working tree already holds the new content. snapshotDir exists to give
// Gate an in-memory "before" to compare the "after" against.
//
// Every file is capped at maxSnapshotFileBytes and the whole dir at
// maxSnapshotFiles; a file over either bound, or one that is not valid
// UTF-8 text (binary), is stored as snapshotPlaceholder instead of its raw
// bytes — so the map's key set still reflects every observed file even
// though not every value is diffable text.
//
// Text file content is passed through redactValueLines before being stored:
// any literal YAML "value:" scalar (e.g. a permissions.env literal secret,
// task.EnvEntry.Value) is blanked to redactedEnvValue right here, at read
// time — never held in the map, never reaching Gate.Diff's output on any
// surface. This is the pending-change-diff analog of gate.go's
// sanitizePermissions/redactedEnvValue, which does the same for the content
// hash.
//
// A missing dir returns an empty map (no error) — callers may race task
// removal, matching Hash's behavior for the same case. Symlinks, sockets,
// devices, and other non-regular entries are skipped entirely (no text
// content to diff), matching Hash's walkTree classification.
func snapshotDir(dir string) (map[string]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && snapshotHeavyDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil // vanished mid-walk (race) — skip, don't fail the scan
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(paths)

	out := make(map[string]string, len(paths))
	for i, rel := range paths {
		if i >= maxSnapshotFiles {
			out[rel] = snapshotPlaceholder
			continue
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		info, statErr := os.Stat(abs)
		if statErr != nil || info.Size() > maxSnapshotFileBytes {
			out[rel] = snapshotPlaceholder
			continue
		}
		data, readErr := os.ReadFile(abs)
		if readErr != nil || !utf8.Valid(data) {
			out[rel] = snapshotPlaceholder
			continue
		}
		// Redact literal YAML "value:" scalars (task.EnvEntry.Value secrets)
		// before the content ever lands in the maps Gate.approvedFiles and
		// pendingEntry.files hold and Gate.Diff renders — see redactValueLines.
		out[rel] = redactValueLines(string(data))
	}
	return out, nil
}
