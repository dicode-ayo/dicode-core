// Package-level validators for the git ref names dicode accepts: branch names
// (dev-mode clone-mode and dicode.git.commit_push) and the tag a source pins
// itself to. Pure functions — no I/O.
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
	ErrInvalidTagName       = errors.New("invalid tag name (git check-ref-format)")
	ErrBranchPrefixMismatch = errors.New("branch does not start with required prefix")
	ErrInvalidRunID         = errors.New("invalid run ID")
)

// ValidateRunID enforces a safe character set on a run identifier used as a
// path component (e.g., the dev-clones clone-dir name). Allows letters,
// digits, underscore, and dash; length 1-64. Rejects path separators,
// traversal sequences, control characters, and anything else that could
// escape a directory component.
func ValidateRunID(runID string) error {
	if len(runID) == 0 || len(runID) > 64 {
		return fmt.Errorf("%w: length must be 1-64", ErrInvalidRunID)
	}
	for _, r := range runID {
		switch {
		case 'a' <= r && r <= 'z',
			'A' <= r && r <= 'Z',
			'0' <= r && r <= '9',
			r == '_' || r == '-':
			// allowed
		default:
			return fmt.Errorf("%w: forbidden char %q", ErrInvalidRunID, r)
		}
	}
	return nil
}

// ValidateBranchName enforces git check-ref-format rules plus a literal-prefix
// match against `prefix`. An empty prefix means "no prefix required".
func ValidateBranchName(branch, prefix string) error {
	if reason := refNameViolation(branch); reason != "" {
		return fmt.Errorf("%w: %s", ErrInvalidBranchName, reason)
	}
	if prefix != "" && !strings.HasPrefix(branch, prefix) {
		return fmt.Errorf("%w: branch %q does not start with %q", ErrBranchPrefixMismatch, branch, prefix)
	}
	return nil
}

// ValidateTagName enforces the same git check-ref-format rules on the tag a
// git ref pins itself to. Tags carry no prefix convention, so unlike a branch
// there is nothing further to match against.
func ValidateTagName(tag string) error {
	if reason := refNameViolation(tag); reason != "" {
		return fmt.Errorf("%w: %s", ErrInvalidTagName, reason)
	}
	return nil
}

// refNameViolation returns why name is not a legal git ref name, or "" when
// it is one. git check-ref-format applies the same rules to every ref
// namespace, so branch and tag names share this one implementation.
func refNameViolation(name string) string {
	if name == "" {
		return "empty"
	}
	if name == "@" {
		return "name '@' is not allowed"
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return "leading/trailing slash"
	}
	if strings.HasPrefix(name, "-") {
		return "leading dash"
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return "forbidden sequence"
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "control char"
		}
		switch r {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return fmt.Sprintf("forbidden char %q", r)
		}
	}
	for _, comp := range strings.Split(name, "/") {
		if strings.HasPrefix(comp, ".") {
			return "component starts with '.'"
		}
		if strings.HasSuffix(comp, ".") {
			return "component ends with '.'"
		}
		if strings.HasSuffix(comp, ".lock") {
			return "component ends with '.lock'"
		}
	}
	return ""
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
