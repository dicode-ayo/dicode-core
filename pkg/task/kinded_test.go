package task

import "testing"

func TestSpecImplementsKinded(t *testing.T) {
	var k Kinded = &Spec{ID: "a", Name: "A", Enabled: true}
	if k.KindOf() != KindTask {
		t.Fatalf("KindOf = %q, want %q", k.KindOf(), KindTask)
	}
	if k.TaskID() != "a" {
		t.Fatalf("TaskID = %q, want a", k.TaskID())
	}
	k.SetTaskID("b")
	if k.TaskID() != "b" {
		t.Fatalf("SetTaskID failed: %q", k.TaskID())
	}
	if !k.IsEnabled() {
		t.Fatal("IsEnabled = false, want true")
	}
	k.SetEnabled(false)
	if k.IsEnabled() {
		t.Fatal("SetEnabled(false) failed")
	}
	if ws := k.LoadWarnings(); ws != nil {
		t.Fatalf("LoadWarnings: want nil, got %v", ws)
	}
}
