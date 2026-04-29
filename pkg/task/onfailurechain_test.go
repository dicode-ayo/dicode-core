package task

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOnFailureChainSpec_BareString(t *testing.T) {
	src := []byte(`on_failure_chain: auto-fix
`)
	var w struct {
		OnFailureChain OnFailureChainSpec `yaml:"on_failure_chain"`
	}
	if err := yaml.Unmarshal(src, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.OnFailureChain.Task != "auto-fix" {
		t.Errorf("Task = %q, want auto-fix", w.OnFailureChain.Task)
	}
	if len(w.OnFailureChain.Params) != 0 {
		t.Errorf("Params = %v, want empty", w.OnFailureChain.Params)
	}
}

func TestOnFailureChainSpec_Structured(t *testing.T) {
	src := []byte(`
on_failure_chain:
  task: auto-fix
  params:
    mode: review
    max_iterations: 3
`)
	var w struct {
		OnFailureChain OnFailureChainSpec `yaml:"on_failure_chain"`
	}
	if err := yaml.Unmarshal(src, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.OnFailureChain.Task != "auto-fix" {
		t.Errorf("Task = %q", w.OnFailureChain.Task)
	}
	wantParams := map[string]any{"mode": "review", "max_iterations": 3}
	if !reflect.DeepEqual(w.OnFailureChain.Params, wantParams) {
		t.Errorf("Params = %v, want %v", w.OnFailureChain.Params, wantParams)
	}
}

func TestOnFailureChainSpec_Empty(t *testing.T) {
	src := []byte(`{}
`)
	var w struct {
		OnFailureChain OnFailureChainSpec `yaml:"on_failure_chain"`
	}
	if err := yaml.Unmarshal(src, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if w.OnFailureChain.Task != "" {
		t.Errorf("Task = %q, want empty", w.OnFailureChain.Task)
	}
}

func TestOnFailureChainSpec_IsZero(t *testing.T) {
	zero := OnFailureChainSpec{}
	if !zero.IsZero() {
		t.Error("zero value should be zero")
	}
	nonZero := OnFailureChainSpec{Task: "auto-fix"}
	if nonZero.IsZero() {
		t.Error("non-empty Task should not be zero")
	}
}
