package approval

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// Diff describes the file-level changes between a pending task's
// last-known-approved content snapshot and its current pending content, for
// an operator to review before approving. See Gate.Diff.
type Diff struct {
	TaskID string `json:"task_id"`
	// HasBaseline is false when no prior approved snapshot is cached for
	// this task — e.g. a fresh daemon session that has not yet observed
	// this task at an approved hash (approvedFiles is in-memory only, see
	// Gate's doc comment on the same tradeoff for pending/admitted). When
	// false, Files still reports every pending file, each as "added", so
	// the UI has something useful to show; it should also surface that
	// there is no real "before" to compare against.
	HasBaseline bool `json:"has_baseline"`
	// Files holds one entry per changed file (added, removed, or modified).
	// Unchanged files are omitted.
	Files []FileDiff `json:"files"`
}

// FileDiff is one changed file between the approved and pending snapshots.
type FileDiff struct {
	Path string `json:"path"`
	// Status is "added", "removed", or "modified".
	Status string `json:"status"`
	// UnifiedDiff is a human-readable rendering of the change, "-"/"+"/" "
	// prefixed lines (not byte-perfect POSIX unified diff format — just
	// clear and readable). This instead carries the snapshotPlaceholder
	// note verbatim only when NEITHER side's content was captured by the
	// snapshot (both binary/too-large/beyond-the-per-task-file-cap — see
	// snapshotPlaceholder); when only one side is a placeholder, the other
	// side's real content is still rendered, as a full add/remove against
	// an empty counterpart (see unifiedDiffText).
	UnifiedDiff string `json:"unified_diff"`
	// SecurityRelevant is true when an added or removed line in the diff
	// touches one of the YAML keys matched by securityFieldPattern.
	SecurityRelevant bool `json:"security_relevant"`
	// OldContent and NewContent are the two sides of UnifiedDiff's hunks
	// reconstructed as plain text, for a client-side diff viewer to render
	// itself. Deliberately derived from the hunked diff rather than the whole
	// file: shipping both full sides costs more than the unhunked diff it
	// replaced (measured: 1.2MB -> 2.3MB across ten changed files), and a
	// viewer fed the hunks shows the same changes. Elision markers appear in
	// both sides as ordinary context, so they survive as visible context in
	// the viewer. Empty when the file exceeds maxInlineContentBytes or either
	// side is a placeholder — a client must fall back to UnifiedDiff then.
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

// maxInlineContentBytes bounds a single reconstructed side. Only reachable
// when a file is changed nearly end-to-end (hunking elides nothing), where
// the plain hunked text is the more useful rendering anyway.
const maxInlineContentBytes = 128 * 1024

// diffContextLines is how many unchanged lines are kept either side of a
// change in UnifiedDiff. Without this the whole file is emitted as context:
// a one-line edit to a 3k-line task shipped ~3,005 lines and rendered a
// 56,000px page, which buries the change it exists to surface.
const diffContextLines = 3

// securityFieldPattern matches an added or removed unified-diff line that
// touches one of the YAML keys folded into the approval gate's content hash
// (see the big ContentHash doc comment in gate.go for the authoritative
// list): permissions, env, run, net, fs, sys, dicode (permissions and its
// sub-fields, including permissions.dicode.git_commit_push), and the
// resolved trigger shape (webhook, webhook_auth, cron, manual, daemon,
// chain). A hit on either side of a hunk means the change could widen what
// the task can touch or how/whether it fires, so FileDiff.SecurityRelevant
// flags it for the operator regardless of anywhere else in the diff.
//
// The key is required to be immediately followed (after optional
// whitespace) by a colon, so e.g. "environment:" or "blockchain:" — which
// merely contain "env"/"chain" as a substring — do not false-positive.
var securityFieldPattern = regexp.MustCompile(
	`(?m)^[-+].*\b(permissions|env|run|net|fs|sys|dicode|git_commit_push|webhook|webhook_auth|cron|daemon|manual|chain)[ \t]*:`)

// securityBlockPattern matches the top-level YAML key that opens a block
// whose entire subtree is security-relevant. A changed line anywhere inside
// such a block counts, which securityFieldPattern alone cannot express.
var securityBlockPattern = regexp.MustCompile(`^(permissions|trigger)[ \t]*:`)

// diffLineIndent returns the indentation column of a rendered diff line,
// ignoring its "+ "/"- "/"  " prefix. Elision markers and placeholder notes
// carry no prefix and no meaningful indentation, so they report -1 and are
// skipped by the block scan rather than closing a block they sit inside.
func diffLineIndent(line string) int {
	if len(line) < 2 || (line[0] != '+' && line[0] != '-' && line[0] != ' ') || line[1] != ' ' {
		return -1
	}
	return leadingIndent(line[2:])
}

// touchesSecurityBlock reports whether any added or removed line in diff sits
// inside a `permissions:` or `trigger:` block.
//
// securityFieldPattern only fires on a changed line that itself names a
// security key, so it catches a permissions block being *added* but misses it
// being *widened* — appending a host to an already-approved `net:` allowlist
// changes only a list item, whose line names no key at all. Widening an
// existing block is both the likelier change in a trust-on-change model and
// the stealthier one, so it must flag too.
//
// Block membership is tracked by indentation over the diff's own line
// sequence, including context lines, which is why hunking must run after
// flagging: elided context would otherwise drop the `permissions:` opener
// that puts a changed line in scope.
func touchesSecurityBlock(diff string) bool {
	inBlock := false
	blockIndent := 0
	for _, line := range strings.Split(diff, "\n") {
		indent := diffLineIndent(line)
		if indent < 0 {
			continue
		}
		body := strings.TrimLeft(line[2:], " \t")
		// A comment carries no structure and YAML permits it at any column,
		// so a `# note` at column 0 inside a permissions block must not read
		// as the block ending — that would un-scope every changed line after
		// it, and a comment is trivial to introduce alongside a widening.
		if strings.HasPrefix(body, "#") {
			continue
		}
		if inBlock && indent <= blockIndent && body != "" {
			inBlock = false
		}
		if !inBlock && securityBlockPattern.MatchString(body) && indent == 0 {
			inBlock = true
			blockIndent = indent
			continue
		}
		if inBlock && (line[0] == '+' || line[0] == '-') {
			return true
		}
	}
	return false
}

// snapshotValuesEqual reports whether two snapshotValues for the same path
// represent the same content, for Gate.Diff's added/removed/modified/
// unchanged classification.
//
// Two placeholders compare by Fingerprint rather than always being treated
// as equal — otherwise two different oversized/binary files that both hit
// the same cap would collapse to the identical bare snapshotPlaceholder
// string and this file's real change would silently vanish from
// Diff().Files instead of surfacing as "modified". An empty Fingerprint
// (the stat/hash itself failed, a rare race) cannot vouch for equality, so
// it fails toward "modified" rather than toward hiding a possible change —
// the same over-flag-rather-than-under-flag bias securityFieldPattern and
// redactValueLines already take for this feature.
func snapshotValuesEqual(a, b snapshotValue) bool {
	if a.Placeholder != b.Placeholder {
		return false
	}
	if a.Placeholder {
		if a.Fingerprint == "" || b.Fingerprint == "" {
			return false
		}
		return a.Fingerprint == b.Fingerprint
	}
	return a.Content == b.Content
}

// snapshotDisplayText returns the text to feed unifiedDiffText for v: the
// real content, or the bare snapshotPlaceholder constant for a placeholder
// (never its Fingerprint — that stays internal to Gate.Diff's status
// classification and must never reach a rendering surface).
func snapshotDisplayText(v snapshotValue) string {
	if v.Placeholder {
		return snapshotPlaceholder
	}
	return v.Content
}

// Diff computes the file-level diff between id's cached approved-content
// snapshot (if any) and its current pending-content snapshot. Returns an
// error if id is not currently pending.
func (g *Gate) Diff(id string) (Diff, error) {
	g.mu.Lock()
	ent, isPending := g.pending[id]
	approvedSnap, hasBaseline := g.approvedFiles[id]
	g.mu.Unlock()

	if !isPending {
		return Diff{}, fmt.Errorf("task %q is not pending approval", id)
	}

	pendingSnap := ent.files
	out := Diff{TaskID: id, HasBaseline: hasBaseline}

	// pendingSnap is nil exactly when the task is dir-less (taskDirOf
	// returned "" so the pending snapshot was never captured — see
	// pendingEntry.files) — an in-dir task always gets at least an empty,
	// non-nil map from snapshotDir. Nothing to diff for an inline task.
	if pendingSnap == nil {
		return out, nil
	}

	paths := make(map[string]bool, len(pendingSnap)+len(approvedSnap))
	for p := range pendingSnap {
		paths[p] = true
	}
	for p := range approvedSnap {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	for _, p := range sorted {
		newVal, inPending := pendingSnap[p]
		oldVal, inApproved := approvedSnap[p]

		var status string
		switch {
		case inPending && !inApproved:
			status = "added"
		case !inPending && inApproved:
			status = "removed"
		case inPending && inApproved && !snapshotValuesEqual(oldVal, newVal):
			status = "modified"
		default:
			continue // unchanged
		}

		udiff := unifiedDiffText(snapshotDisplayText(oldVal), snapshotDisplayText(newVal))
		fd := FileDiff{
			Path:             p,
			Status:           status,
			UnifiedDiff:      udiff,
			SecurityRelevant: securityFieldPattern.MatchString(udiff) || touchesSecurityBlock(udiff),
		}
		// Flagging runs against the full diff above, before hunking drops
		// context — elision must never hide a security-relevant line from the
		// check, only from the rendering. touchesSecurityBlock depends on this
		// ordering directly: it needs the unelided context to see which block
		// a changed line sits in.
		fd.UnifiedDiff = hunked(fd.UnifiedDiff)
		if udiff != snapshotPlaceholder {
			fd.OldContent, fd.NewContent = hunkSides(fd.UnifiedDiff)
		}
		out.Files = append(out.Files, fd)
	}
	return out, nil
}

// hunkSides reconstructs the two sides of a hunked diff as plain text: a
// context or elision line belongs to both, a "- " line only to the old side,
// a "+ " line only to the new. Returns ("", "") when either side would exceed
// maxInlineContentBytes, leaving the caller's UnifiedDiff as the rendering.
func hunkSides(hunkedDiff string) (oldSide, newSide string) {
	var o, n strings.Builder
	for _, l := range strings.Split(strings.TrimSuffix(hunkedDiff, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "+ "):
			n.WriteString(l[2:])
			n.WriteString("\n")
		case strings.HasPrefix(l, "- "):
			o.WriteString(l[2:])
			o.WriteString("\n")
		case strings.HasPrefix(l, "  "):
			o.WriteString(l[2:])
			o.WriteString("\n")
			n.WriteString(l[2:])
			n.WriteString("\n")
		default: // elision marker / placeholder note — context on both sides
			o.WriteString(l)
			o.WriteString("\n")
			n.WriteString(l)
			n.WriteString("\n")
		}
	}
	if o.Len() > maxInlineContentBytes || n.Len() > maxInlineContentBytes {
		return "", ""
	}
	return o.String(), n.String()
}

// hunked drops runs of unchanged context longer than 2*diffContextLines from
// a rendered diff, replacing each with a muted "⋯ N unchanged lines" marker.
// The marker matches none of the "+ "/"- "/"  " prefixes, so both renderers
// already class it as a note without needing to know about elision.
//
// A diff with no changed lines at all is returned unmodified: that is the
// snapshotPlaceholder note (or an empty diff), not context worth eliding.
func hunked(diff string) string {
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	keep := make([]bool, len(lines))
	changed := false
	for i, l := range lines {
		if !strings.HasPrefix(l, "+ ") && !strings.HasPrefix(l, "- ") {
			continue
		}
		changed = true
		for j := i - diffContextLines; j <= i+diffContextLines; j++ {
			if j >= 0 && j < len(lines) {
				keep[j] = true
			}
		}
	}
	if !changed {
		return diff
	}

	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		if keep[i] {
			b.WriteString(lines[i])
			b.WriteString("\n")
			continue
		}
		run := 0
		for i+run < len(lines) && !keep[i+run] {
			run++
		}
		if run == 1 {
			b.WriteString(lines[i])
		} else {
			fmt.Fprintf(&b, "⋯ %d unchanged lines", run)
		}
		b.WriteString("\n")
		i += run - 1
	}
	return b.String()
}

// unifiedDiffText renders a simple, readable "-"/"+"/" " prefixed diff of
// oldText -> newText using diffmatchpatch's line-mode diff (DiffLinesToChars
// -> DiffMain -> DiffCharsToLines -> DiffCleanupSemantic — diffing whole
// lines as atomic units, then folding adjacent hunks together for
// readability). Not byte-perfect POSIX unified diff format, just clear text
// for a human operator to review.
//
// Both sides being snapshotPlaceholder (binary / too large / uncaptured)
// short-circuits to the placeholder text itself: there is no meaningful line
// diff to render when neither side was ever read. When only one side is a
// placeholder, that side is treated as empty and the diff still runs, so the
// available side's real content (e.g. pending content that has since shrunk
// under the snapshot cap, even though the approved baseline was captured as
// a placeholder) is rendered as a full add/remove rather than being hidden
// behind the placeholder note.
func unifiedDiffText(oldText, newText string) string {
	if oldText == snapshotPlaceholder && newText == snapshotPlaceholder {
		return snapshotPlaceholder
	}
	if oldText == snapshotPlaceholder {
		oldText = ""
	}
	if newText == snapshotPlaceholder {
		newText = ""
	}

	dmp := diffmatchpatch.New()
	chars1, chars2, lines := dmp.DiffLinesToChars(oldText, newText)
	diffs := dmp.DiffMain(chars1, chars2, false)
	diffs = dmp.DiffCharsToLines(diffs, lines)
	diffs = dmp.DiffCleanupSemantic(diffs)

	var b strings.Builder
	for _, d := range diffs {
		prefix := " "
		switch d.Type {
		case diffmatchpatch.DiffInsert:
			prefix = "+"
		case diffmatchpatch.DiffDelete:
			prefix = "-"
		}
		text := d.Text
		if text == "" {
			continue
		}
		for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
			b.WriteString(prefix)
			b.WriteString(" ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}
