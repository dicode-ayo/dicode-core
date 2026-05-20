package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKindedYAML(t *testing.T, dir, src string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKindedDir_Pipeline(t *testing.T) {
	dir := t.TempDir()
	writeKindedYAML(t, dir, `apiVersion: dicode/v1
kind: PipelineTask
name: P
subtype: sequential
trigger: { manual: true }
stages: [ { task: buildin/template } ]
`)
	k, err := LoadKindedDir(dir, nil)
	if err != nil {
		t.Fatalf("LoadKindedDir: %v", err)
	}
	if k.KindOf() != KindPipelineTask {
		t.Fatalf("KindOf = %q", k.KindOf())
	}
	if _, ok := k.(*PipelineTask); !ok {
		t.Fatalf("want *PipelineTask, got %T", k)
	}
}

func TestLoadKindedDir_TaskExplicitAndMissing(t *testing.T) {
	// Missing kind defaults to Task (existing task.yaml files have no kind field).
	dir := t.TempDir()
	writeKindedYAML(t, dir, `name: T
runtime: deno
trigger: { manual: true }
`)
	if err := os.WriteFile(filepath.Join(dir, "task.ts"),
		[]byte("export default async function main(){return 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	k, err := LoadKindedDir(dir, nil)
	if err != nil {
		t.Fatalf("LoadKindedDir: %v", err)
	}
	if _, ok := k.(*Spec); !ok {
		t.Fatalf("want *Spec, got %T", k)
	}

	// Explicit kind: Task also routes to *Spec.
	dir2 := t.TempDir()
	writeKindedYAML(t, dir2, `kind: Task
name: T2
runtime: deno
trigger: { manual: true }
`)
	if err := os.WriteFile(filepath.Join(dir2, "task.ts"),
		[]byte("export default async function main(){return 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	k2, err := LoadKindedDir(dir2, nil)
	if err != nil {
		t.Fatalf("LoadKindedDir (explicit Task): %v", err)
	}
	if _, ok := k2.(*Spec); !ok {
		t.Fatalf("want *Spec, got %T", k2)
	}
}

func TestLoadKindedDir_UnknownKind(t *testing.T) {
	dir := t.TempDir()
	writeKindedYAML(t, dir, `kind: Frobnicator
name: X
`)
	if _, err := LoadKindedDir(dir, nil); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestLoadKindedDir_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeKindedYAML(t, dir, "")
	_, err := LoadKindedDir(dir, nil)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("expected empty-file error, got %v", err)
	}
}
