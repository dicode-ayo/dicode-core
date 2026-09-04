package taskset

import (
	"github.com/go-git/go-git/v5/plumbing"
)

// refName is the fully-qualified git reference a target names.
func (t gitTarget) refName() plumbing.ReferenceName {
	if t.Kind == refTag {
		return plumbing.NewTagReferenceName(t.Name)
	}
	return plumbing.NewBranchReferenceName(t.Name)
}
