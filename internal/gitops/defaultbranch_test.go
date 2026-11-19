package gitops

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultBranch_RefusesInternalHost(t *testing.T) {
	for _, u := range []string{"https://127.0.0.1/repo.git", "git@localhost:repo.git"} {
		if _, err := DefaultBranch(context.Background(), u); !errors.Is(err, ErrBlockedHost) {
			t.Errorf("DefaultBranch(%q) err = %v, want ErrBlockedHost", u, err)
		}
	}
}
