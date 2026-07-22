package taskset

import "testing"

// TestSourceID_NameQualified_NoCollisionOnSharedPath is the regression guard
// for issue #621: two different dicode.yaml entries (or an existing entry
// and a dynamically-added one via apiAddSource) that happen to reference the
// identical local path or git URL must never produce the same Source.ID().
// Before SourceID existed, the id was just ref.URL (or ref.Path when URL was
// empty) with no name component, so two names sharing a ref collided.
func TestSourceID_NameQualified_NoCollisionOnSharedPath(t *testing.T) {
	sharedPath := "/tmp/shared/taskset.yaml"
	idA := SourceID("e2e-tests", &Ref{Path: sharedPath})
	idB := SourceID("e2e-add-local-123", &Ref{Path: sharedPath})

	if idA == idB {
		t.Fatalf("SourceID collided for two different names sharing a path: idA=%q idB=%q", idA, idB)
	}

	// Same name + same ref must still be stable/deterministic (apiAddSource
	// and apiRemoveSource compute it independently and must agree).
	if got := SourceID("e2e-tests", &Ref{Path: sharedPath}); got != idA {
		t.Errorf("SourceID not deterministic: got %q, want %q", got, idA)
	}
}

// TestSourceID_GitURL_NameQualified mirrors the local-path case for git refs.
func TestSourceID_GitURL_NameQualified(t *testing.T) {
	sharedURL := "https://github.com/example/repo.git"
	idA := SourceID("infra-a", &Ref{URL: sharedURL})
	idB := SourceID("infra-b", &Ref{URL: sharedURL})

	if idA == idB {
		t.Fatalf("SourceID collided for two different names sharing a git URL: idA=%q idB=%q", idA, idB)
	}
}

// TestSourceID_PrefersURLOverPath mirrors the pre-existing "id := ref.URL; if
// empty, ref.Path" precedence the two call sites used before this helper
// existed, just with the name prefix added.
func TestSourceID_PrefersURLOverPath(t *testing.T) {
	ref := &Ref{URL: "https://github.com/example/repo.git", Path: "should-be-ignored"}
	got := SourceID("infra", ref)
	want := "infra:https://github.com/example/repo.git"
	if got != want {
		t.Errorf("SourceID = %q, want %q", got, want)
	}
}
