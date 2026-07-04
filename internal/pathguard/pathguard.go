// Package pathguard is the single audited implementation of path-containment
// checks used across dicode's security guards (webhook asset serving, task
// deletion, taskset resolution, container bind-mount policy, binary cache).
//
// Two levels of strictness are provided:
//
//   - Within: purely lexical containment. Sufficient when the checked path was
//     built by the caller from validated segments and no on-disk symlink can be
//     interposed between check and use.
//   - WithinResolved: canonicalizes symlinks (via the longest existing
//     ancestor) in both root and path before the lexical check. Required
//     whenever untrusted content can plant symlinks under the root — e.g.
//     go-git materializes repo-committed symlinks as real on-disk links.
//
// When in doubt, use WithinResolved: it is the strictest semantics.
package pathguard

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// Within reports whether p lexically lies within root: after cleaning both
// paths, p must equal root or sit inside root's subtree. Prefix confusion is
// rejected ("/etc" does not contain "/etc-evil"). Purely lexical — no
// filesystem access, symlinks are not resolved (use WithinResolved when an
// attacker could plant one). The error is always nil today; it is part of the
// signature so callers are forced to fail closed if resolution steps are ever
// added.
func Within(root, p string) (bool, error) {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	if p == root {
		return true, nil
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(p, prefix), nil
}

// WithinResolved reports whether p lies within root after canonicalizing
// symlinks on both sides. root must exist (it is resolved with
// filepath.EvalSymlinks directly); p may not exist yet — its longest existing
// ancestor is resolved and the missing tail re-appended (see ResolveExisting).
// Any resolution failure returns an error so callers fail closed.
func WithinResolved(root, p string) (bool, error) {
	realRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return false, fmt.Errorf("resolve root %q: %w", root, err)
	}
	realP, err := ResolveExisting(p)
	if err != nil {
		return false, fmt.Errorf("resolve path %q: %w", p, err)
	}
	return Within(realRoot, realP)
}

// ResolveExisting canonicalizes the longest existing prefix of p with
// filepath.EvalSymlinks (resolving every symlink in it) and rejoins the
// trailing components that do not yet exist. The rejoined tail cannot contain
// a traversable symlink because it has no on-disk entry to follow. Stat
// failures other than "not exist" (e.g. permission denied, a file used as a
// directory) are returned as errors so callers fail closed.
func ResolveExisting(p string) (string, error) {
	p = filepath.Clean(p)
	var tail []string
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			if len(tail) == 0 {
				return real, nil
			}
			return filepath.Join(append([]string{real}, tail...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", err
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = parent
	}
}
