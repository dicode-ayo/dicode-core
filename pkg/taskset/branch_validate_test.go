package taskset

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateBranchName_Valid(t *testing.T) {
	cases := []struct{ branch, prefix string }{
		{"fix/abc-123", "fix/"},
		{"fix/process-payment_2026-04-29", "fix/"},
		{"main", "main"}, // exact match for autonomous-mode push
		{"feature/x.y.z", "feature/"},
	}
	for _, c := range cases {
		if err := ValidateBranchName(c.branch, c.prefix); err != nil {
			t.Errorf("ValidateBranchName(%q, %q) = %v, want nil", c.branch, c.prefix, err)
		}
	}
}

func TestValidateBranchName_RejectsBadRefFormat(t *testing.T) {
	cases := []struct {
		name, branch, prefix string
	}{
		{"double-dot", "fix/abc..def", "fix/"},
		{"leading-dash", "-fix/abc", "fix/"},
		{"control-char", "fix/abc\x01def", "fix/"},
		{"caret", "fix/abc^def", "fix/"},
		{"tilde", "fix/abc~def", "fix/"},
		{"colon", "fix/abc:def", "fix/"},
		{"question", "fix/abc?def", "fix/"},
		{"asterisk", "fix/abc*def", "fix/"},
		{"open-bracket", "fix/abc[def", "fix/"},
		{"backslash", "fix/abc\\def", "fix/"},
		{"space", "fix/abc def", "fix/"},
		{"leading-slash", "/fix/abc", "fix/"},
		{"trailing-slash", "fix/abc/", "fix/"},
		{"double-slash", "fix//abc", "fix/"},
		{"reflog-sequence", "fix/@{0}", "fix/"},
		{"trailing-lock", "fix/abc.lock", "fix/"},
		{"at-sign-only", "@", ""},
		{"empty", "", ""},
		{"dot-leading-component", "fix/.hidden", "fix/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateBranchName(c.branch, c.prefix)
			if !errors.Is(err, ErrInvalidBranchName) {
				t.Errorf("got %v, want ErrInvalidBranchName", err)
			}
		})
	}
}

func TestValidateBranchName_RejectsPrefixMismatch(t *testing.T) {
	err := ValidateBranchName("hotfix/abc", "fix/")
	if !errors.Is(err, ErrBranchPrefixMismatch) {
		t.Errorf("got %v, want ErrBranchPrefixMismatch", err)
	}
}

func TestValidateBranchName_RejectsBadPrefix(t *testing.T) {
	cases := []string{"fix/*", "fix/[a-z]", "fix/?", "fix/{x,y}", "../fix/", "fix\\"}
	for _, prefix := range cases {
		err := ValidateBranchPrefix(prefix)
		if err == nil {
			t.Errorf("ValidateBranchPrefix(%q) = nil, want error", prefix)
		}
		if !strings.Contains(err.Error(), "prefix") {
			t.Errorf("error %q does not mention 'prefix'", err)
		}
	}
}
