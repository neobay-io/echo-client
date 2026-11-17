package core

import "testing"

func TestResolvePermissionMode(t *testing.T) {
	tests := []struct {
		agent, raw        string
		native, canonical string
		ok                bool
	}{
		{"codex", "suggest", "default", PermissionModeDefault, true},
		{"codex", "auto-edit", "acceptEdits", PermissionModeAcceptEdits, true},
		{"codex", "full-auto", "auto", PermissionModeAuto, true},
		{"codex", "AUTO", "auto", PermissionModeAuto, true},
		{"codex", "yolo", "bypassPermissions", PermissionModeBypass, true},
		{"codex", "bypassPermissions", "bypassPermissions", PermissionModeBypass, true},
		{"codex", "plan", "", "", false},
		{"claudecode", "edit", "acceptEdits", PermissionModeAcceptEdits, true},
		{"claudecode", "  DONTASK ", "dontAsk", "", true},
		{"claudecode", "acceptedits", "acceptEdits", PermissionModeAcceptEdits, true},
		{"gemini", "auto", "yolo", PermissionModeBypass, true},
		{"gemini", "acceptEdits", "auto_edit", PermissionModeAcceptEdits, true},
		{"cursor", "ask", "ask", "", true},
		{"unknown-agent", "yolo", "yolo", PermissionModeBypass, true},
		{"unknown-agent", "auto", "", "", false},
		{"codex", "", "", "", false},
		{"codex", "nonsense", "", "", false},
	}
	for _, tt := range tests {
		native, canonical, ok := ResolvePermissionMode(tt.agent, tt.raw)
		if native != tt.native || canonical != tt.canonical || ok != tt.ok {
			t.Errorf("ResolvePermissionMode(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.agent, tt.raw, native, canonical, ok, tt.native, tt.canonical, tt.ok)
		}
	}
	if got := NormalizePermissionMode("codex", "nonsense"); got != "default" {
		t.Errorf("NormalizePermissionMode fallback = %q", got)
	}
}

// Every value the old hard-coded isEchoYoloEquivalentMode accepted must still
// be accepted, and partial-approval modes must not count as bypass.
func TestEchoYoloEquivalentModeKeepsLegacyValues(t *testing.T) {
	accepted := map[string][]string{
		"claudecode": {"yolo", "bypassPermissions", "bypasspermissions"},
		"codex":      {"yolo", "YOLO", "bypassPermissions"},
		"qoder":      {"yolo", "bypass", "dangerously-skip-permissions"},
		"gemini":     {"yolo", "auto", "force", "bypasspermissions"},
		"opencode":   {"yolo", "auto", "force", "bypasspermissions"},
		"iflow":      {"yolo", "auto", "force", "bypasspermissions"},
		"newagent":   {"yolo"},
	}
	for agent, modes := range accepted {
		for _, mode := range modes {
			if !isEchoYoloEquivalentMode(agent, mode) {
				t.Errorf("isEchoYoloEquivalentMode(%q, %q) = false", agent, mode)
			}
		}
	}
	rejected := map[string][]string{
		"claudecode": {"auto", "acceptEdits", "default", "dontAsk", ""},
		"codex":      {"full-auto", "auto", "auto-edit", "suggest", ""},
		"qoder":      {"acceptEdits", "dontAsk"},
		"gemini":     {"auto_edit", "plan"},
	}
	for agent, modes := range rejected {
		for _, mode := range modes {
			if isEchoYoloEquivalentMode(agent, mode) {
				t.Errorf("isEchoYoloEquivalentMode(%q, %q) = true", agent, mode)
			}
		}
	}
}
