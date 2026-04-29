// Package-level validator for branch names accepted by dev-mode clone-mode
// and dicode.git.commit_push. Pure function — no I/O.
//
// Rules (spec § 4.6.3): git check-ref-format equivalent + literal-prefix
// match against the per-task branch_prefix. Glob/regex characters in the
// prefix are rejected at config-load via ValidateBranchPrefix.

package taskset

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidBranchName    = errors.New("invalid branch name (git check-ref-format)")
	ErrBranchPrefixMismatch = errors.New("branch does not start with required prefix")
)

// ValidateBranchName enforces git check-ref-format rules plus a literal-prefix
// match against `prefix`. An empty prefix means "no prefix required".
func ValidateBranchName(branch, prefix string) error {
	if branch == "" {
		return fmt.Errorf("%w: empty", ErrInvalidBranchName)
	}
	if branch == "@" {
		return fmt.Errorf("%w: name '@' is not allowed", ErrInvalidBranchName)
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") {
		return fmt.Errorf("%w: leading/trailing slash", ErrInvalidBranchName)
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%w: leading dash", ErrInvalidBranchName)
	}
	if strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.Contains(branch, "@{") {
		return fmt.Errorf("%w: forbidden sequence", ErrInvalidBranchName)
	}
	if strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("%w: trailing .lock", ErrInvalidBranchName)
	}
	for _, r := range branch {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control char", ErrInvalidBranchName)
		}
		switch r {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return fmt.Errorf("%w: forbidden char %q", ErrInvalidBranchName, r)
		}
	}
	for _, comp := range strings.Split(branch, "/") {
		if strings.HasPrefix(comp, ".") {
			return fmt.Errorf("%w: component starts with '.'", ErrInvalidBranchName)
		}
	}
	if prefix != "" && !strings.HasPrefix(branch, prefix) {
		return fmt.Errorf("%w: branch %q does not start with %q", ErrBranchPrefixMismatch, branch, prefix)
	}
	return nil
}

// ValidateBranchPrefix is invoked at config-load on each task's branch_prefix
// to reject glob/regex constructs that would make ValidateBranchName ambiguous.
//
// Currently exported for use by the auto-fix taskset override (#238); not yet
// wired into the live config-load path.
func ValidateBranchPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	for _, r := range prefix {
		switch {
		case 'a' <= r && r <= 'z',
			'A' <= r && r <= 'Z',
			'0' <= r && r <= '9',
			r == '_' || r == '.' || r == '/' || r == '-':
			// allowed
		default:
			return fmt.Errorf("invalid character %q in branch prefix; allowed: [A-Za-z0-9_./-]", r)
		}
	}
	if strings.Contains(prefix, "..") {
		return fmt.Errorf("invalid '..' in branch prefix")
	}
	return nil
}
