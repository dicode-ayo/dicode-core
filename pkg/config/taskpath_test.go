package config

import "testing"

func TestSplitTaskID(t *testing.T) {
	cases := []struct {
		id         string
		wantSource string
		wantSub    string
		wantOK     bool
	}{
		{"buildin/temp-cleanup", "buildin", "temp-cleanup", true},
		{"infra/platform/nginx", "infra", "platform/nginx", true},
		{"a/b/c/d/e", "a", "b/c/d/e", true},
		{"buildin", "", "", false},
		{"", "", "", false},
		{"/leading-slash", "", "leading-slash", true}, // empty source key — caller decides
		{"trailing/", "trailing", "", true},           // empty sub — caller decides
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			src, sub, ok := SplitTaskID(tc.id)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if src != tc.wantSource {
				t.Errorf("source = %q, want %q", src, tc.wantSource)
			}
			if sub != tc.wantSub {
				t.Errorf("sub = %q, want %q", sub, tc.wantSub)
			}
		})
	}
}
