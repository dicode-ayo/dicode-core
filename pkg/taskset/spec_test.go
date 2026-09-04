package taskset

import (
	"strings"
	"testing"
)

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
// existed, just with the length-prefixed name encoding added.
func TestSourceID_PrefersURLOverPath(t *testing.T) {
	ref := &Ref{URL: "https://github.com/example/repo.git", Path: "should-be-ignored"}
	got := SourceID("infra", ref)
	want := "5:infra:https://github.com/example/repo.git"
	if got != want {
		t.Errorf("SourceID = %q, want %q", got, want)
	}
}

// TestSourceID_ColonInName_StillNoCollision is the regression guard for a gap
// found in code review: a plain "<name>:<target>" concatenation (the
// original implementation) is only collision-free if name can never contain
// a ':'. That precondition holds for API-added sources (validateSourceName
// rejects ':'), but dicode.yaml's spec.entries keys and nested taskset.yaml
// entry names are never charset-validated — only their Ref is. Without
// length-prefixing, name "a:b" + path "c" and name "a" + path "b:c" both
// produced the plain-concatenation string "a:b:c", reproducing the exact
// rc.cancels collision issue #621 fixed, just via a colon-bearing static
// config entry name instead of a duplicate ref. This test would fail against
// the plain "name + \":\" + target" formula.
func TestSourceID_ColonInName_StillNoCollision(t *testing.T) {
	idA := SourceID("a:b", &Ref{Path: "c"})
	idB := SourceID("a", &Ref{Path: "b:c"})
	if idA == idB {
		t.Fatalf("SourceID collided despite different (name, ref) pairs: idA=%q idB=%q", idA, idB)
	}
}

// TestRefKinds_AllComplete guards the table every refKind consumer reads
// through: a kind added without an entry would silently render an empty label,
// build a reference name with no namespace prefix, and hash into another
// kind's clone directory.
func TestRefKinds_AllComplete(t *testing.T) {
	for _, kind := range []refKind{refBranch, refTag} {
		info, ok := refKinds[kind]
		if !ok {
			t.Errorf("refKind %d has no refKinds entry", kind)
			continue
		}
		if info.label == "" {
			t.Errorf("refKind %d has no label", kind)
		}
		if !strings.HasPrefix(info.prefix, "refs/") || !strings.HasSuffix(info.prefix, "/") {
			t.Errorf("refKind %q prefix = %q, want a refs/…/ namespace", info.label, info.prefix)
		}
		if info.untrustedDomain == "" {
			t.Errorf("refKind %q has no untrusted clone-directory domain", info.label)
		}
	}
	// Exactly one bucket may carry the legacy seed; a second empty
	// trustedDomain would collide two kinds onto one directory.
	legacy := 0
	for _, info := range refKinds {
		if info.trustedDomain == "" {
			legacy++
		}
	}
	if legacy != 1 {
		t.Errorf("%d refKinds claim the legacy trusted seed, want exactly 1", legacy)
	}
}
