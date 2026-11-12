package approval

import "testing"

// TestHeld: Held is true for a pending task and for a gated task not yet
// admitted, false once it is approved or for a trusted task.
func TestHeld(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/ui", "export default () => {}")
	if !g.Held("repo/ui") {
		t.Fatal("a task the gate has not admitted must be held")
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit: armed=%v err=%v; want pending", armed, err)
	}
	if !g.Held("repo/ui") {
		t.Fatal("a pending task must be held")
	}
	if err := g.Approve("repo/ui"); err != nil {
		t.Fatal(err)
	}
	if g.Held("repo/ui") {
		t.Fatal("an approved task must not be held")
	}
	if g.Held("buildin/webui") {
		t.Fatal("a trusted builtin must not be held")
	}
}
