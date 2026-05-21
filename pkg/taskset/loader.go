package taskset

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind is the value of the top-level kind field in a dicode yaml file.
type Kind string

const (
	KindTaskSet      Kind = "TaskSet"
	KindTask         Kind = "Task"
	KindConfig       Kind = "Config"
	KindPipelineTask Kind = "PipelineTask" // mirrors task.KindPipelineTask; kept as a literal so pkg/taskset stays decoupled from pkg/task's constant
)

// fileHeader peeks at the kind field without decoding the full document.
type fileHeader struct {
	Kind string `yaml:"kind"`
}

// DetectKind reads only the kind field from a yaml file.
// Returns an error if the file cannot be opened or the kind field is missing.
func DetectKind(path string) (Kind, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var h fileHeader
	if err := yaml.NewDecoder(f).Decode(&h); err != nil {
		return "", fmt.Errorf("decode %s: %w", path, err)
	}
	if h.Kind == "" {
		return "", fmt.Errorf("%s: kind field is required", path)
	}
	return Kind(h.Kind), nil
}

// LoadTaskSet parses and validates a file with kind: TaskSet.
func LoadTaskSet(path string) (*TaskSetSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if err := probeLegacyNotify(data, path); err != nil {
		return nil, err
	}

	var ts TaskSetSpec
	if err := yaml.Unmarshal(data, &ts); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if ts.Kind != string(KindTaskSet) {
		return nil, fmt.Errorf("%s: expected kind TaskSet, got %q", path, ts.Kind)
	}
	if err := validateTaskSet(&ts, path); err != nil {
		return nil, err
	}
	return &ts, nil
}

// probeLegacyNotify walks the YAML tree of a TaskSet/Config file and returns
// a clear error if any `notify:` key appears anywhere — the field was removed
// in #279 and yaml.v3's tolerant decode would otherwise drop it silently,
// leading to "alerts went to /dev/null" without warning. Notifications are
// now delivered by tasks via on_failure_chain.
func probeLegacyNotify(data []byte, path string) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// Decode error here will surface again from the typed Unmarshal in
		// the caller with the same data, so just bail without an error.
		return nil
	}
	if hasLegacyNotifyKey(&root) {
		return fmt.Errorf("%s: legacy `notify` block detected. The per-task "+
			"and taskset-level notify field was removed (#279). Use "+
			"`on_failure_chain` to fire a notification task on failure.", path)
	}
	return nil
}

func hasLegacyNotifyKey(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind == yaml.ScalarNode && k.Value == "notify" {
				return true
			}
		}
	}
	for _, child := range n.Content {
		if hasLegacyNotifyKey(child) {
			return true
		}
	}
	return false
}

// LoadConfig parses a file with kind: Config.
// Returns nil, nil if the file does not exist (Config is optional).
func LoadConfig(path string) (*ConfigSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	if err := probeLegacyNotify(data, path); err != nil {
		return nil, err
	}

	var cs ConfigSpec
	if err := yaml.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cs.Kind != string(KindConfig) {
		return nil, fmt.Errorf("%s: expected kind Config, got %q", path, cs.Kind)
	}
	return &cs, nil
}

// LiftEntryEnabled normalises the top-level `enabled` shortcut on every entry
// in the provided map. After the call each entry.Enabled is nil and the value
// lives exclusively in entry.Overrides.Enabled. If both are set on the same
// entry an error is returned so callers never silently pick a winner.
//
// This is extracted as a package-level helper so that both validateTaskSet and
// pkg/config's validate() can call it without duplicating the logic.
func LiftEntryEnabled(entries map[string]*Entry) error {
	for key, entry := range entries {
		if entry == nil || entry.Enabled == nil {
			continue
		}
		if entry.Overrides != nil && entry.Overrides.Enabled != nil {
			return fmt.Errorf("entry %q: top-level `enabled` conflicts with `overrides.enabled` — set one or the other", key)
		}
		if entry.Overrides == nil {
			entry.Overrides = &Overrides{}
		}
		entry.Overrides.Enabled = entry.Enabled
		entry.Enabled = nil
	}
	return nil
}

// ValidateRefURL validates the scheme of a git ref URL.
// It accepts http, https, ssh, and git schemes, plus the SSH shorthand
// form (git@host:path) which url.Parse would misparse as a relative URL
// with scheme "". The SSH shorthand is detected by the SCP-style
// user@host:path pattern: the URL must contain no "://" (which would mean
// it already has an explicit scheme), and the colon separating host from
// path must follow the "@".
func ValidateRefURL(filePath, key, rawURL string) error {
	// Detect the SCP-style SSH shorthand: user@host:path (e.g. git@github.com:org/repo.git).
	// These look like relative paths to url.Parse (Scheme=""), so we handle them
	// explicitly before calling url.Parse.
	// Guard: skip if the URL already has an explicit scheme (contains "://").
	if !strings.Contains(rawURL, "://") {
		if at := strings.Index(rawURL, "@"); at > 0 {
			afterAt := rawURL[at+1:]
			if colon := strings.Index(afterAt, ":"); colon > 0 {
				// Ensure no "/" before the ":" — a slash means it's a path
				// separator, not the SCP host:path separator.
				hostPart := afterAt[:colon]
				if !strings.Contains(hostPart, "/") {
					return nil // valid SSH shorthand
				}
			}
		}
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s: entry %q: invalid ref.url: %w", filePath, key, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "ssh", "git":
		return nil
	default:
		return fmt.Errorf("%s: entry %q: ref.url must use scheme http, https, ssh, or git (got %q)", filePath, key, u.Scheme)
	}
}

func validateTaskSet(ts *TaskSetSpec, path string) error {
	if ts.Spec.Entries == nil {
		return fmt.Errorf("%s: spec.entries is required", path)
	}
	for key, entry := range ts.Spec.Entries {
		if entry == nil {
			return fmt.Errorf("%s: entry %q is nil", path, key)
		}
		if entry.Ref == nil && entry.Inline == nil {
			return fmt.Errorf("%s: entry %q: one of ref or inline is required", path, key)
		}
		if entry.Ref != nil && entry.Inline != nil {
			return fmt.Errorf("%s: entry %q: ref and inline are mutually exclusive", path, key)
		}
		if entry.Ref != nil && entry.Ref.URL == "" && entry.Ref.Path == "" {
			return fmt.Errorf("%s: entry %q: ref.path is required for local refs", path, key)
		}
		if entry.Ref != nil && entry.Ref.URL != "" {
			if err := ValidateRefURL(path, key, entry.Ref.URL); err != nil {
				return err
			}
		}
	}
	// Lift the top-level `enabled` shortcut into overrides.enabled so all
	// downstream code (resolver, override merging) sees one canonical path.
	if err := LiftEntryEnabled(ts.Spec.Entries); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
