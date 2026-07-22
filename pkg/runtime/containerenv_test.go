package runtime

import (
	"reflect"
	"testing"
)

func TestBuildContainerEnv_LiteralOnly(t *testing.T) {
	got := BuildContainerEnv(map[string]string{"B": "2", "A": "1"}, nil)
	want := []string{"A=1", "B=2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestBuildContainerEnv_ResolvedOnly(t *testing.T) {
	got := BuildContainerEnv(nil, map[string]string{"TOKEN": "sekret", "HOST": "api"})
	want := []string{"HOST=api", "TOKEN=sekret"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// A key present in both maps takes the resolved value: the declared secret is
// the intended value for that name.
func TestBuildContainerEnv_ResolvedWinsOnCollision(t *testing.T) {
	got := BuildContainerEnv(
		map[string]string{"TOKEN": "placeholder", "KEEP": "lit"},
		map[string]string{"TOKEN": "real-secret"},
	)
	want := []string{"KEEP=lit", "TOKEN=real-secret"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Output order is deterministic (sorted by key) regardless of map iteration.
func TestBuildContainerEnv_Deterministic(t *testing.T) {
	in := map[string]string{"z": "1", "a": "2", "m": "3", "b": "4"}
	first := BuildContainerEnv(in, nil)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(BuildContainerEnv(in, nil), first) {
			t.Fatalf("non-deterministic output: %v", first)
		}
	}
	want := []string{"a=2", "b=4", "m=3", "z=1"}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("got %v want %v", first, want)
	}
}

func TestBuildContainerEnv_Empty(t *testing.T) {
	if got := BuildContainerEnv(nil, nil); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}
