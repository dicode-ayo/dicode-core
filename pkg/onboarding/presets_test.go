package onboarding

import "testing"

func TestTaskSetPresets_AllThree(t *testing.T) {
	if len(TaskSetPresets) != 3 {
		t.Fatalf("len(TaskSetPresets) = %d; want 3", len(TaskSetPresets))
	}
}

func TestTaskSetPresets_NamesMatchPlan(t *testing.T) {
	want := map[string]bool{"buildin": false, "examples": false, "auth": false}
	for _, p := range TaskSetPresets {
		if _, ok := want[p.Name]; !ok {
			t.Errorf("unexpected preset name %q", p.Name)
		}
		want[p.Name] = true
	}
	for n, seen := range want {
		if !seen {
			t.Errorf("missing preset %q", n)
		}
	}
}

func TestTaskSetPresets_AllFieldsPopulated(t *testing.T) {
	for _, p := range TaskSetPresets {
		if p.Name == "" || p.Label == "" || p.Desc == "" || p.URL == "" || p.Branch == "" || p.EntryPath == "" {
			t.Errorf("preset %+v has an empty required field", p)
		}
	}
}

func TestTaskSetPresets_DefaultOnForAll(t *testing.T) {
	for _, p := range TaskSetPresets {
		if !p.DefaultOn {
			t.Errorf("preset %q has DefaultOn=false; want true", p.Name)
		}
	}
}

func TestTaskSetPresets_NamesUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for _, p := range TaskSetPresets {
		if _, dup := seen[p.Name]; dup {
			t.Errorf("duplicate preset name %q", p.Name)
		}
		seen[p.Name] = struct{}{}
	}
}

func TestTaskSetPresets_EntryPathsDistinct(t *testing.T) {
	seen := map[string]struct{}{}
	for _, p := range TaskSetPresets {
		if _, dup := seen[p.EntryPath]; dup {
			t.Errorf("duplicate entry_path %q across presets", p.EntryPath)
		}
		seen[p.EntryPath] = struct{}{}
	}
}
