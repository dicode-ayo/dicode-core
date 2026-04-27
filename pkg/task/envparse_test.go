package task

import "testing"

func TestParseFrom(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantKind FromKind
		wantTgt  string
	}{
		{"empty", "", FromKindEnv, ""},
		{"bare name", "FOO", FromKindEnv, "FOO"},
		{"explicit env", "env:FOO", FromKindEnv, "FOO"},
		{"task prefix", "task:doppler", FromKindTask, "doppler"},
		{"task prefix with hyphen", "task:secret-providers/doppler", FromKindTask, "secret-providers/doppler"},
		{"trim whitespace bare", "  FOO  ", FromKindEnv, "FOO"},
		{"trim whitespace prefix", "  task:doppler  ", FromKindTask, "doppler"},
		{"unknown prefix → bare", "foo:bar", FromKindEnv, "foo:bar"},
		{"empty env target", "env:", FromKindEnv, ""},
		{"empty task target", "task:", FromKindTask, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotKind, gotTgt := parseFrom(tt.in)
			if gotKind != tt.wantKind || gotTgt != tt.wantTgt {
				t.Errorf("parseFrom(%q) = (%d, %q), want (%d, %q)",
					tt.in, gotKind, gotTgt, tt.wantKind, tt.wantTgt)
			}
		})
	}
}
