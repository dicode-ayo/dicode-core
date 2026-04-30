package task

import (
	"reflect"
	"strings"
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

func TestOnFailureChainSpec_Validate_ReservedKeyCollision(t *testing.T) {
	cases := []string{"taskID", "runID", "status", "output", "_chain_depth"}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			s := OnFailureChainSpec{
				Task:   "auto-fix",
				Params: map[string]any{key: "x"},
			}
			_, err := s.Validate()
			if err == nil {
				t.Fatal("expected error for reserved key collision")
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Errorf("error %q should mention 'reserved'", err)
			}
		})
	}
}

func TestOnFailureChainSpec_ValidateAtDefaults_RejectsAutonomous(t *testing.T) {
	s := OnFailureChainSpec{
		Task:   "auto-fix",
		Params: map[string]any{"mode": "autonomous"},
	}
	err := s.ValidateAtDefaults()
	if err == nil {
		t.Fatal("expected error for autonomous-at-defaults")
	}
	if !strings.Contains(err.Error(), "autonomous") {
		t.Errorf("error %q should mention 'autonomous'", err)
	}
}

func TestOnFailureChainSpec_Validate_AcceptsAutonomousAtTaskLevel(t *testing.T) {
	s := OnFailureChainSpec{
		Task:   "auto-fix",
		Params: map[string]any{"mode": "autonomous"},
	}
	if _, err := s.Validate(); err != nil {
		t.Errorf("per-task autonomous should be accepted by Validate(); got %v", err)
	}
}

func TestOnFailureChainSpec_ValidateAtDefaults_AcceptsReview(t *testing.T) {
	s := OnFailureChainSpec{
		Task:   "auto-fix",
		Params: map[string]any{"mode": "review"},
	}
	if err := s.ValidateAtDefaults(); err != nil {
		t.Errorf("review at defaults should be accepted; got %v", err)
	}
}

// TestOnFailureChainSpec_StripsGlobalFieldsForPerTask verifies that
// MaxConcurrentGlobal and Storm are zeroed by Validate() when set at the
// per-task level, and that a structured warning is returned for each.
func TestOnFailureChainSpec_StripsGlobalFieldsForPerTask(t *testing.T) {
	s := OnFailureChainSpec{
		Task:                "auto-fix",
		MaxConcurrentGlobal: 5,
		Storm: StormSpec{
			Rate:     10,
			Window:   60 * 1e9, // 60s as nanoseconds
			Suppress: 300 * 1e9,
			Scope:    "global",
		},
	}

	warns, err := s.Validate()
	if err != nil {
		t.Fatalf("Validate() returned unexpected error: %v", err)
	}
	if len(warns) != 2 {
		t.Fatalf("expected 2 warnings (MaxConcurrentGlobal + Storm), got %d: %v", len(warns), warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "operator-policy") {
			t.Errorf("warning should mention 'operator-policy'; got %q", w)
		}
	}
	if s.MaxConcurrentGlobal != 0 {
		t.Errorf("MaxConcurrentGlobal should be zeroed after Validate(); got %d", s.MaxConcurrentGlobal)
	}
	if s.Storm != (StormSpec{}) {
		t.Errorf("Storm should be zeroed after Validate(); got %+v", s.Storm)
	}
}

// TestOnFailureChainSpec_ValidateAtDefaults_KeepsGlobalFields verifies that
// ValidateAtDefaults does NOT strip MaxConcurrentGlobal or Storm — those are
// valid at the defaults site.
func TestOnFailureChainSpec_ValidateAtDefaults_KeepsGlobalFields(t *testing.T) {
	s := OnFailureChainSpec{
		Task:                "auto-fix",
		MaxConcurrentGlobal: 5,
		Storm: StormSpec{
			Rate:   10,
			Window: 60 * 1e9,
		},
	}
	if err := s.ValidateAtDefaults(); err != nil {
		t.Fatalf("ValidateAtDefaults() rejected valid defaults-level spec: %v", err)
	}
	if s.MaxConcurrentGlobal != 5 {
		t.Errorf("ValidateAtDefaults should not zero MaxConcurrentGlobal; got %d", s.MaxConcurrentGlobal)
	}
}
