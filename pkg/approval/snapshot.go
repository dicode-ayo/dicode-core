package approval

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
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

// snapshotPlaceholder is the display text for a file whose content was not
// captured — because it is binary (fails UTF-8 validation), larger than
// maxSnapshotFileBytes, or beyond maxSnapshotFiles. Diff never synthesizes a
// line-level unified diff for these; FileDiff.UnifiedDiff carries this
// string verbatim instead.
const snapshotPlaceholder = "binary or file too large to diff"

// snapshotValue is one file's captured state. When Placeholder is false,
// Content holds the (redacted) text. When Placeholder is true, Content is
// unused and Fingerprint carries an opaque per-version marker — see
// snapshotFingerprint — so Gate.Diff can tell "capped but identical" apart
// from "capped and different" without ever holding two full oversized/binary
// files in memory to compare byte-for-byte.
//
// Without Fingerprint, two different files that both hit the same cap would
// both collapse to the bare snapshotPlaceholder string and compare equal —
// a real change in an oversized file would silently vanish from Diff().Files
// instead of surfacing as "modified", which is exactly the file an attacker
// would pick to hide a payload from this review surface.
type snapshotValue struct {
	Content     string
	Placeholder bool
	Fingerprint string
	// Digest is a hash of the file's RAW bytes, taken before redaction. It,
	// not Content, decides whether a file changed.
	//
	// Comparing redacted text conflates "identical" with "identically
	// scrubbed", and redaction is driven by file content the attacker
	// controls. A block-scalar marker planted at column 0 makes
	// redactValueLines swallow everything indented below it into one
	// <redacted> line, so two entirely different versions of a task script
	// scrub to the same string, compare equal, and drop out of Diff().Files
	// altogether — the file becomes permanently invisible to review while
	// the content hash still pends the task. Deciding on raw bytes means
	// nothing the scrub does can make a changed file look unchanged.
	Digest string
}

// snapshotHeavyDirs mirrors task/hash.go's heavyDirs: directories that hold
// no task content and can dominate walk cost. Kept as its own copy (rather
// than reaching into task's unexported map) so this package's walk isn't
// coupled to task's internals — same list, same rationale.
var snapshotHeavyDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
}

// valueLinePattern matches a YAML mapping entry naming a field that can carry
// an inline secret — "value", "default" or "webhook_secret" — e.g.
// `  value: "sk-live-secret"` or `- default: secret` —
// capturing the key portion (leading indentation, optional list-item dash,
// optional quotes around the key name, and the colon) in group 1 and the
// scalar (or, per blockScalarHeaderPattern below, block-scalar header) that
// follows in group 2. See redactValueLines for why this exists.
var valueLinePattern = regexp.MustCompile(`(?m)^([ \t]*(?:-[ \t]*)?"?(?:value|default|webhook_secret)"?[ \t]*:)[ \t]*(.*)$`)

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
// This exists because task.EnvEntry.Value and task.EnvEntry.Default
// (pkg/task/spec.go) both let a task's permissions.env block carry a literal
// secret inline in task.yaml (as opposed to Secret, a secrets-store key name
// reference). Default is injected as the env var's value whenever the named
// secret is absent from the store (pkg/runtime/envresolve/resolver.go), and
// its documented use is exactly a credential fallback — so it carries the
// same class of material as Value and must be blanked on the same surfaces. ContentHash
// (gate.go's sanitizePermissions/redactedEnvValue) already keeps that literal
// out of the committable dicode.lock; snapshotDir must keep it out of the
// pending-change diff surface (REST endpoint, WebUI panel, and the
// unauthenticated /approve/{token} confirm page) the same way — reusing the
// same redactedEnvValue placeholder for consistency.
//
// Deliberately generic: this redacts any line that looks like a YAML
// "value:"/"default:" mapping entry in any snapshotted file, not just ones
// provably inside permissions.env — a snapshot has no field-path-aware YAML
// parse, and erring toward over-redaction is the correct tradeoff for a
// security fix. This does blank param defaults (task.Param.Default), which
// are not secrets; that cost is accepted, since a param default is
// reconstructible from the task source while a leaked credential is not.
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
// classification) and returns a map of dir-relative slash path to
// snapshotValue, for Gate.Diff to compare snapshots taken at different times.
//
// This is a distinct walk from Hash's: the approval lock (pkg/approval/lock.go)
// only ever stores a content hash, never file bytes, so there is no
// "before" snapshot to diff against once a task's hash has changed and the
// working tree already holds the new content. snapshotDir exists to give
// Gate an in-memory "before" to compare the "after" against.
//
// Every file is capped at maxSnapshotFileBytes and the whole dir at
// maxSnapshotFiles; a file over either bound, or one that is not valid
// UTF-8 text (binary), is stored as a placeholder snapshotValue (see its doc
// comment for why it still carries a per-version Fingerprint rather than
// collapsing every capped file to one indistinguishable value) instead of
// its raw bytes — so the map's key set still reflects every observed file
// even though not every value is diffable text.
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
func snapshotDir(dir string) (map[string]snapshotValue, error) {
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

	out := make(map[string]snapshotValue, len(paths))
	for i, rel := range paths {
		if i >= maxSnapshotFiles {
			// Beyond the file-count cap: stat only (no content read) for a
			// size+mtime fingerprint — cheap, and this file was never going
			// to be read anyway.
			abs := filepath.Join(dir, filepath.FromSlash(rel))
			out[rel] = snapshotValue{Placeholder: true, Fingerprint: statFingerprint(abs)}
			continue
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		info, statErr := os.Stat(abs)
		if statErr != nil || info.Size() > maxSnapshotFileBytes {
			// Oversized (or vanished mid-scan). Fingerprint by streaming the
			// content, not size+mtime: maxSnapshotFileBytes bounds how much
			// is held in memory for the gate's lifetime and rendered as diff
			// text, not how much may be read, and a streamed hash is O(1)
			// memory over I/O task.Hash already performs on the same
			// schedule. Size+mtime here would leave the diff blind exactly
			// where the hash is not — see maxFingerprintReadBytes.
			var fp string
			if statErr == nil {
				fp = streamFingerprint(abs, info)
			}
			out[rel] = snapshotValue{Placeholder: true, Fingerprint: fp}
			continue
		}
		data, readErr := os.ReadFile(abs)
		if readErr != nil || !utf8.Valid(data) {
			// Binary (or a read race): the bytes are already in memory (or
			// weren't, on a read error), so hash them directly rather than
			// falling back to size+mtime — strictly stronger, and free here.
			var fp string
			if readErr == nil {
				fp = contentFingerprint(data)
			} else {
				fp = fileInfoFingerprint(info)
			}
			out[rel] = snapshotValue{Placeholder: true, Fingerprint: fp}
			continue
		}
		// Redact literal YAML "value:" scalars (task.EnvEntry.Value secrets)
		// before the content ever lands in the maps Gate.approvedFiles and
		// pendingEntry.files hold and Gate.Diff renders — see redactValueLines.
		out[rel] = snapshotValue{
			Content: redactSecrets(string(data)),
			Digest:  contentFingerprint(data),
		}
	}
	return out, nil
}

// statFingerprint stats path and returns a size+mtime descriptor, or "" if
// the stat itself fails (file vanished mid-scan — matches snapshotDir's
// existing tolerant handling of that race elsewhere).
func statFingerprint(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fileInfoFingerprint(info)
}

// fileInfoFingerprint renders a stat result as a fingerprint distinguishing
// this version of a capped file from a different one. Not a content hash —
// two different files of the same size written at the same mtime would
// collide — but it is the same tradeoff task/hash.go's content hash already
// makes for oversized files (size+mtime, not full content), so this closes
// the gap to that existing, accepted risk model rather than exceeding it.
func fileInfoFingerprint(info fs.FileInfo) string {
	return fmt.Sprintf("size=%d mtime=%d", info.Size(), info.ModTime().UnixNano())
}

// contentFingerprint hashes bytes already held in memory (the binary-file
// case, where snapshotDir has already paid the read cost) into a strong,
// version-distinguishing fingerprint — no reason to fall back to the weaker
// size+mtime descriptor when the real bytes are right there.
func contentFingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256=%x", sum)
}

// maxFingerprintReadBytes mirrors task/hash.go's maxHashedFileBytes. The
// content hash reads a file this far before falling back to a size+mtime
// descriptor, so the snapshot must be sensitive to content over exactly the
// same range: any narrower and a change the hash notices — one that pends the
// task and puts an operator in front of the diff — could be invisible in that
// diff. Verified reachable before this bound existed: a same-size edit to a
// 320 KiB file with mtime restored (touch -r) held the task pending and
// produced a diff listing zero changed files.
const maxFingerprintReadBytes = 1 << 20 // 1 MiB

// streamFingerprint hashes path's content without holding it in memory. Past
// maxFingerprintReadBytes it falls back to size+mtime, which is precisely
// where task.Hash stops reading too — beyond that bound the hash cannot
// detect a same-size, same-mtime change either, so the diff is no blinder
// than the gate that feeds it.
func streamFingerprint(path string, info fs.FileInfo) string {
	if info.Size() > maxFingerprintReadBytes {
		return fileInfoFingerprint(info)
	}
	f, err := os.Open(path)
	if err != nil {
		return fileInfoFingerprint(info)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fileInfoFingerprint(info)
	}
	return fmt.Sprintf("sha256=%x", h.Sum(nil))
}

// redactSecrets scrubs literal env secrets out of content before it is stored
// in a snapshot and rendered to the diff surfaces — including the
// unauthenticated /approve/{token} page.
//
// Two passes, because neither alone is sufficient:
//
//  1. A node-tree walk (collectRedactionTargets/applyNodeRedactions) decodes
//     content as YAML and, for every `value:`/`default:`/`webhook_secret:`
//     mapping value (scalar or alias) and every `env:` sequence item written
//     as bare `KEY=VALUE` shorthand, blanks that node's own line/column range
//     in the ORIGINAL bytes — not a value-substring search. That makes it
//     immune to the gap a substring sweep has: a multiline plain or quoted
//     scalar folds to a single space at parse time (`"part-one\n  cont"` →
//     `"part-one cont"`), so its parsed value never appears verbatim in the
//     raw bytes for a substring match to find. Working from node positions
//     instead of parsed values closes that gap by construction. An alias
//     target is resolved to its anchor's definition node — the alias site
//     itself (`*name`) carries no secret text.
//  2. redactValueLines then runs on the result as defense in depth: it is
//     line-anchored regex, no YAML parse required, so it still catches a
//     `value:`-shaped line in content that fails to decode as YAML at all
//     (a script — see its doc comment) or anything the node walk missed.
//
// A decode failure (malformed YAML, e.g. most real multi-line JS/TS task
// scripts containing braces/colons — verified empirically: they routinely do
// not decode as a single valid YAML document) yields no node-walk targets
// beyond whatever documents decoded successfully before the failure, exactly
// as before this change. Blanking the entire file's content on any decode
// error was considered (and is what closes the gap most conservatively) but
// rejected: task.yaml itself always parses (pkg/task validates it before a
// spec ever reaches Admit, so a task.yaml that fails a generic yaml.Node
// decode could never have been loaded as a task.Spec in the first place —
// the same syntax layer underlies both), so the secret-carrying structural
// forms this function targets only ever occur in content that DOES decode.
// The files that routinely fail to decode are ordinary scripts, which were
// never YAML-structured to begin with; blanking them wholesale would hide
// real, benign script diffs behind a placeholder — confirmed against
// tests/e2e/approval-diff.spec.ts, whose fixture task.js fails a generic
// yaml.Node decode (its multi-line body trips "mapping values are not
// allowed in this context") yet several of that file's tests assert the
// literal added script line IS visible in the rendered diff. Falling back to
// pass 2 alone for a decode failure — this function's pre-existing, already
// -accepted-risk behavior for non-YAML content — keeps that working.
func redactSecrets(content string) string {
	var targets []redactionTarget
	seen := make(map[*yaml.Node]bool)
	dec := yaml.NewDecoder(strings.NewReader(content))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			break // EOF or malformed — keep whatever earlier documents yielded
		}
		collectRedactionTargets(&doc, &targets, seen)
	}
	return redactValueLines(applyNodeRedactions(content, targets))
}

// redactionTarget is one YAML node whose raw byte range redactSecrets must
// blank: either the value half of a `value:`/`default:`/`webhook_secret:`
// mapping pair (isEnvShorthand false — Node is the scalar to blank, already
// alias-resolved if it started life as an AliasNode), or a bare `KEY=VALUE`
// sequence item under an `env:` key (isEnvShorthand true — Node is the whole
// "KEY=VALUE" scalar; only the substring after the first '=' is secret).
type redactionTarget struct {
	node           *yaml.Node
	isEnvShorthand bool
}

// collectRedactionTargets walks n (a decoded document, or any node reached
// while recursing) collecting every redactionTarget reachable from it, across
// arbitrarily deep nesting. Key matching is case-insensitive: yaml.v3 field
// binding is, so `Value:` reaches EnvEntry.Value just as `value:` does.
//
// webhook_secret is the HMAC key authenticating inbound webhook requests
// (task.TriggerSpec.WebhookSecret). ContentHash already strips it so it never
// reaches the committable lock; without the same treatment here an inline
// literal renders verbatim on the session-less /approve/{token} page, handing
// anyone who saw an approve link the ability to forge authenticated triggers
// for that task.
//
// seen dedupes by node pointer: multiple aliases can resolve to the same
// anchor definition, and it must be blanked only once (blanking twice at
// the same location would be a harmless no-op, but tracking it here keeps
// applyNodeRedactions' target list free of exact duplicates).
func collectRedactionTargets(n *yaml.Node, out *[]redactionTarget, seen map[*yaml.Node]bool) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			switch strings.ToLower(k.Value) {
			case "value", "default", "webhook_secret":
				if v.Kind == yaml.ScalarNode || v.Kind == yaml.AliasNode {
					resolved := v
					if v.Kind == yaml.AliasNode {
						// The alias reference (*name) has no secret text of
						// its own — the real bytes live at the anchor's
						// definition site, elsewhere in the document.
						resolved = v.Alias
					}
					if resolved != nil && resolved.Kind == yaml.ScalarNode && !seen[resolved] {
						seen[resolved] = true
						*out = append(*out, redactionTarget{node: resolved})
					}
				}
			case "env":
				if v.Kind == yaml.SequenceNode {
					for _, item := range v.Content {
						// The documented `- TOKEN=VALUE` shorthand
						// (task.EnvEntry.UnmarshalYAML) is a bare scalar
						// sequence item, not a mapping under a value:/
						// default: key, so it needs its own detection here.
						if item.Kind == yaml.ScalarNode && strings.Contains(item.Value, "=") && !seen[item] {
							seen[item] = true
							*out = append(*out, redactionTarget{node: item, isEnvShorthand: true})
						}
					}
				}
			}
			// Recurse into every value regardless of whether this pair
			// itself matched above — a value:/default: key can nest.
			collectRedactionTargets(v, out, seen)
		}
		return
	}
	for _, c := range n.Content {
		collectRedactionTargets(c, out, seen)
	}
}

// applyNodeRedactions blanks every target's node location in content and
// returns the result. Targets are processed in descending (line, column)
// order so that swallowing a multiline target's continuation lines (which
// only ever removes lines AFTER its own line) never shifts the line numbers
// a not-yet-processed, lower-line-number target relies on. Two distinct
// targets' blanked regions can never overlap: only leaf ScalarNode values
// become targets, and a target's own line is never itself inside another
// target's swallowed continuation — YAML's indentation rule means a sibling
// key can only start at or below the reference indentation a swallow stops
// at, which is exactly the condition under which a genuinely separate
// (nested) target could exist in the tree at all.
func applyNodeRedactions(content string, targets []redactionTarget) string {
	if len(targets) == 0 {
		return content
	}
	sort.SliceStable(targets, func(i, j int) bool {
		if targets[i].node.Line != targets[j].node.Line {
			return targets[i].node.Line > targets[j].node.Line
		}
		return targets[i].node.Column > targets[j].node.Column
	})
	lines := strings.Split(content, "\n")
	for _, t := range targets {
		lines = blankNodeTarget(lines, t)
	}
	return strings.Join(lines, "\n")
}

// blankNodeTarget blanks t's node at its (Line, Column) in lines (1-indexed
// line, 1-indexed rune column — yaml.Node's documented units) and returns the
// possibly-shorter result. It generalizes redactValueLines' block-scalar
// swallow heuristic to every scalar style a value:/default:/webhook_secret:
// value or an env: KEY=VALUE shorthand item can have:
//
//   - Block scalar (Style has LiteralStyle or FoldedStyle — `value: |`/`value:
//     >`): the header line is kept intact, exactly as redactValueLines already
//     does, so the diff still shows the field's presence.
//   - KEY=VALUE shorthand: only the substring after the line's first '=' at or
//     after the node's own column is secret; the "KEY=" prefix is kept.
//   - Everything else (plain/quoted/flow scalars, and an alias's resolved
//     anchor definition): the line is blanked from the node's start column
//     to end of line. This is what closes the plain-multiline-scalar gap —
//     TestRedactSecretsCoversMultilineAndShorthand's `value: part-one\n  sk-live-...` folds to
//     a single parsed value at parse time, but the node's own (Line, Column)
//     still names exactly where "part-one" starts in the raw bytes,
//     independent of the fold.
//
// In every case, once the target line itself is handled, every following
// line that is blank or indented more than the reference indentation (the
// node's own line's leading whitespace/list-dash, via valueKeyIndentPattern —
// the same measure redactValueLines' block-scalar case already uses) is
// swallowed into the same single placeholder, matching YAML's own rule that a
// scalar's continuation must be indented deeper than its containing key.
func blankNodeTarget(lines []string, t redactionTarget) []string {
	lineIdx := t.node.Line - 1
	if lineIdx < 0 || lineIdx >= len(lines) {
		return lines // defensive: a node position outside the split content
	}
	rawLine := lines[lineIdx]
	keyIndent := len(valueKeyIndentPattern.FindString(rawLine))

	runes := []rune(rawLine)
	col := t.node.Column - 1 // yaml.Node.Column is 1-indexed
	if col < 0 {
		col = 0
	}
	if col > len(runes) {
		col = len(runes)
	}

	isBlock := t.node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0
	headerKept := false
	switch {
	case t.isEnvShorthand:
		eq := -1
		for i := col; i < len(runes); i++ {
			if runes[i] == '=' {
				eq = i
				break
			}
		}
		if eq == -1 {
			// Defensive: caller only creates this target when Value
			// contains '=', so this should be unreachable. Blank from the
			// node's own start rather than leave anything unredacted.
			eq = col - 1
		}
		lines[lineIdx] = string(runes[:eq+1]) + redactedEnvValue
	case isBlock:
		headerKept = true // header line ("value: |") is left untouched below
	default:
		lines[lineIdx] = string(runes[:col]) + redactedEnvValue
	}

	j := lineIdx + 1
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
	if !swallowedAny {
		return lines
	}
	if headerKept {
		rest := append([]string{redactedEnvValue}, lines[j:]...)
		return append(lines[:lineIdx+1], rest...)
	}
	return append(lines[:lineIdx+1], lines[j:]...)
}
