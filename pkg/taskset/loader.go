package taskset

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Kind is the value of the top-level kind field in a dicode yaml file.
type Kind string

const (
	KindTaskSet Kind = "TaskSet"
	KindTask    Kind = "Task"
	KindConfig  Kind = "Config"
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

	var ts TaskSetSpec
	if err := yaml.NewDecoder(f).Decode(&ts); err != nil {
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

// LoadConfig parses a file with kind: Config.
// Returns nil, nil if the file does not exist (Config is optional).
func LoadConfig(path string) (*ConfigSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var cs ConfigSpec
	if err := yaml.NewDecoder(f).Decode(&cs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cs.Kind != string(KindConfig) {
		return nil, fmt.Errorf("%s: expected kind Config, got %q", path, cs.Kind)
	}
	return &cs, nil
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
		if entry.Ref != nil && entry.Ref.Path == "" {
			return fmt.Errorf("%s: entry %q: ref.path is required", path, key)
		}
	}
	return nil
}
