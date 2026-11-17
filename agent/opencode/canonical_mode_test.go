package opencode

import "testing"

// Canonical names must reach the adapter, not only the Echo registration check.
func TestNormalizeModeAcceptsCanonicalNames(t *testing.T) {
	for in, want := range map[string]string{
		"bypassPermissions": "yolo",
		"auto":              "yolo",
		"force":             "yolo",
		"junk":              "default",
	} {
		if got := normalizeMode(in); got != want {
			t.Errorf("normalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}
