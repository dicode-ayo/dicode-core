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
}

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
		newContent, inPending := pendingSnap[p]
		oldContent, inApproved := approvedSnap[p]

		var status string
		switch {
		case inPending && !inApproved:
			status = "added"
		case !inPending && inApproved:
			status = "removed"
		case inPending && inApproved && newContent != oldContent:
			status = "modified"
		default:
			continue // unchanged
		}

		udiff := unifiedDiffText(oldContent, newContent)
		out.Files = append(out.Files, FileDiff{
			Path:             p,
			Status:           status,
			UnifiedDiff:      udiff,
			SecurityRelevant: securityFieldPattern.MatchString(udiff),
		})
	}
	return out, nil
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
