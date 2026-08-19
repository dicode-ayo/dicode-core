package taskset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/dicode/dicode/internal/fsutil"
	"github.com/dicode/dicode/internal/pathguard"
)

// ErrEntryConflict reports that spec.entries already carries the requested key
// with a different target, so registering would overwrite operator wiring.
var ErrEntryConflict = errors.New("taskset entry already exists")

// entryNameRe constrains a key this package writes into spec.entries. The key
// becomes the last segment of a namespaced task ID and is joined onto a
// filesystem path, so path separators and yaml-significant characters are
// refused rather than escaped.
var entryNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// skeletonTaskSet is the document AddTaskEntry starts from when the taskset
// file does not exist yet.
const skeletonTaskSet = "apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries: {}\n"

// defaultTaskSetMode is the permission applied to a taskset file this package
// creates. An existing file keeps its own mode.
const defaultTaskSetMode = os.FileMode(0644)

// entryWriteMu serialises the read-modify-write of every taskset file in this
// process. Two callers scaffolding into the same source concurrently would
// otherwise both read the pre-change document and the second write would drop
// the first one's entry.
var entryWriteMu sync.Mutex

// taskEntryRefPath is the ref path of a task directory that sits beside the
// taskset file.
func taskEntryRefPath(name string) string {
	return "./" + name + "/task.yaml"
}

// AddTaskEntry registers the task directory named name — a sibling of the
// taskset file at tsPath — as spec.entries[name]. Without the entry the
// resolver never sees the directory: resolution walks spec.entries and nothing
// scans the source tree.
//
// The taskset file is rewritten through its yaml node tree, so comments,
// ordering, and every other entry survive. It is created from a minimal
// skeleton when absent.
//
// Adding an entry that already points at the same directory is a no-op
// success; a key already bound to something else returns ErrEntryConflict.
func AddTaskEntry(tsPath, name string) error {
	if err := validateEntryName(name); err != nil {
		return err
	}
	refPath := taskEntryRefPath(name)

	entryWriteMu.Lock()
	defer entryWriteMu.Unlock()

	doc, mode, err := loadTaskSetDoc(tsPath)
	if err != nil {
		return err
	}
	entries, err := ensureEntriesNode(doc, tsPath)
	if err != nil {
		return err
	}

	if _, val := findMapValue(entries, name); val != nil {
		if refPathOf(val) == refPath {
			return nil
		}
		return fmt.Errorf("%s: entry %q: %w", tsPath, name, ErrEntryConflict)
	}

	var valNode yaml.Node
	if err := valNode.Encode(Entry{Ref: &Ref{Path: refPath}}); err != nil {
		return fmt.Errorf("%s: encode entry %q: %w", tsPath, name, err)
	}
	entries.Content = append(entries.Content, scalarKey(name), &valNode)

	return writeTaskSetDoc(tsPath, doc, mode)
}

// RemoveTaskEntry deletes spec.entries[name] from the taskset file at tsPath
// when its ref resolves inside taskDir, and reports whether it removed
// anything. An inline entry, or a ref pointing elsewhere (a nested taskset, a
// hand-written path into another directory), is left alone: the caller removed
// one task directory and must not drop wiring that outlives it.
//
// A missing file or a missing key is a no-op success — the entry is already
// gone, which is what the caller wanted.
func RemoveTaskEntry(tsPath, name, taskDir string) (bool, error) {
	entryWriteMu.Lock()
	defer entryWriteMu.Unlock()

	if !fsutil.Exists(tsPath) {
		return false, nil
	}
	doc, mode, err := loadTaskSetDoc(tsPath)
	if err != nil {
		return false, err
	}
	entries := entriesNode(doc)
	if entries == nil {
		return false, nil
	}
	idx, val := findMapValue(entries, name)
	if val == nil {
		return false, nil
	}
	ref := refPathOf(val)
	if ref == "" || !refTargets(filepath.Dir(tsPath), ref, taskDir) {
		return false, nil
	}

	entries.Content = append(entries.Content[:idx], entries.Content[idx+2:]...)
	if err := writeTaskSetDoc(tsPath, doc, mode); err != nil {
		return false, err
	}
	return true, nil
}

// refTargets reports whether ref — a local ref path as written in the taskset
// at tsDir — resolves to a file inside taskDir. Both sides are canonicalised
// so a symlinked source root cannot make an unrelated entry look like a match.
func refTargets(tsDir, ref, taskDir string) bool {
	if strings.Contains(ref, "${") {
		return false
	}
	resolved := ref
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(tsDir, resolved)
	}
	within, err := pathguard.WithinResolved(taskDir, resolved)
	return err == nil && within
}

// ValidateEntryName reports whether name is usable as a spec.entries key.
// Callers that create files named after the entry must check this before
// writing anything: a name this package refuses cannot be registered, and an
// unregistered task directory is invisible to the resolver.
func ValidateEntryName(name string) error { return validateEntryName(name) }

func validateEntryName(name string) error {
	if !entryNameRe.MatchString(name) {
		return fmt.Errorf("invalid taskset entry name %q", name)
	}
	return nil
}

// loadTaskSetDoc parses tsPath into its yaml document node and returns the file
// mode to write back with. A missing file yields the skeleton document.
func loadTaskSetDoc(tsPath string) (*yaml.Node, os.FileMode, error) {
	mode := defaultTaskSetMode
	data, err := os.ReadFile(tsPath)
	switch {
	case err == nil:
		if fi, statErr := os.Stat(tsPath); statErr == nil {
			mode = fi.Mode().Perm()
		}
	case os.IsNotExist(err):
		data = []byte(skeletonTaskSet)
	default:
		return nil, 0, fmt.Errorf("read %s: %w", tsPath, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, 0, fmt.Errorf("parse %s: %w", tsPath, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		// An empty or non-mapping document carries nothing worth preserving.
		if err := yaml.Unmarshal([]byte(skeletonTaskSet), &doc); err != nil {
			return nil, 0, fmt.Errorf("parse %s: %w", tsPath, err)
		}
		return &doc, mode, nil
	}
	if kind := scalarValue(doc.Content[0], "kind"); kind != string(KindTaskSet) {
		return nil, 0, fmt.Errorf("%s: expected kind TaskSet, got %q", tsPath, kind)
	}
	return &doc, mode, nil
}

func writeTaskSetDoc(tsPath string, doc *yaml.Node, mode os.FileMode) error {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode %s: %w", tsPath, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode %s: %w", tsPath, err)
	}
	if err := fsutil.WriteFileAtomic(tsPath, []byte(buf.String()), mode); err != nil {
		return fmt.Errorf("write %s: %w", tsPath, err)
	}
	return nil
}

// entriesNode returns the spec.entries mapping node, or nil when the document
// has none.
func entriesNode(doc *yaml.Node) *yaml.Node {
	if doc == nil || len(doc.Content) == 0 {
		return nil
	}
	_, spec := findMapValue(doc.Content[0], "spec")
	if spec == nil || spec.Kind != yaml.MappingNode {
		return nil
	}
	_, entries := findMapValue(spec, "entries")
	if entries == nil || entries.Kind != yaml.MappingNode {
		return nil
	}
	return entries
}

// ensureEntriesNode returns the spec.entries mapping, creating spec and/or
// entries when the document omits them.
func ensureEntriesNode(doc *yaml.Node, tsPath string) (*yaml.Node, error) {
	root := doc.Content[0]
	_, spec := findMapValue(root, "spec")
	if spec == nil {
		spec = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, scalarKey("spec"), spec)
	}
	if spec.Kind == yaml.ScalarNode && spec.Tag == "!!null" {
		*spec = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if spec.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: spec is not a mapping", tsPath)
	}

	_, entries := findMapValue(spec, "entries")
	if entries == nil {
		entries = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		spec.Content = append(spec.Content, scalarKey("entries"), entries)
	}
	if entries.Kind == yaml.ScalarNode && entries.Tag == "!!null" {
		*entries = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if entries.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: spec.entries is not a mapping", tsPath)
	}
	if len(entries.Content) == 0 {
		// `entries: {}` round-trips as a flow mapping, which would render the
		// first entry as a single unreadable line. A non-empty flow mapping is
		// left in the style the operator chose.
		entries.Style = 0
	}
	return entries, nil
}

// findMapValue returns the index of key's node in a mapping's Content and the
// value node that follows it, or (-1, nil) when absent.
func findMapValue(m *yaml.Node, key string) (int, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i, m.Content[i+1]
		}
	}
	return -1, nil
}

// scalarValue returns the string value of key in a mapping node.
func scalarValue(m *yaml.Node, key string) string {
	_, v := findMapValue(m, key)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}

// refPathOf returns the ref.path of an entry node, or "" when the entry has no
// local ref.
func refPathOf(entry *yaml.Node) string {
	if entry == nil || entry.Kind != yaml.MappingNode {
		return ""
	}
	_, ref := findMapValue(entry, "ref")
	if ref == nil || ref.Kind != yaml.MappingNode {
		return ""
	}
	if scalarValue(ref, "url") != "" {
		return ""
	}
	return scalarValue(ref, "path")
}

func scalarKey(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}
