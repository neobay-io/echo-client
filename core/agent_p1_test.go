package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

// P1: Kimi's installer puts the CLI in ~/.kimi-code/bin, which launchd and
// systemd services do not have on PATH.
func TestResolveAgentCLIFallsBackToVendorInstallDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable bit")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".kimi-code", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	kimi := filepath.Join(bin, "kimi")
	if err := os.WriteFile(kimi, []byte("#!/bin/sh\necho 2.5.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	agentCLIHomeDir = func() (string, error) { return home, nil }
	defer func() { agentCLIHomeDir = os.UserHomeDir }()
	t.Setenv("PATH", t.TempDir())

	path, err := ResolveAgentCLI("kimi")
	if err != nil || path != kimi {
		t.Fatalf("ResolveAgentCLI = %q, %v; want %q", path, err, kimi)
	}
	info := InspectAgentCLI(context.Background(), "kimi")
	if info.Selected == nil || info.Selected.Path != kimi || !info.Selected.OffPath || info.Version != "2.5.3" {
		t.Fatalf("InspectAgentCLI = %+v", info)
	}
	if !strings.Contains(AgentCLIDoctorResult(info).Detail, "not on PATH") {
		t.Fatalf("doctor detail = %q", AgentCLIDoctorResult(info).Detail)
	}
	if _, err := ResolveAgentCLI("antigravity"); err == nil {
		t.Fatal("agy has no fallback location and is not on PATH")
	}
}

func TestInteractionOptionsWrapRoundTrip(t *testing.T) {
	wrapped := WrapInteractionOptions("deploy\nnow")
	if !strings.HasPrefix(wrapped, strings.TrimSpace(InteractionOptionsPrompt())) {
		t.Fatalf("wrapped = %q", wrapped)
	}
	if got := StripInteractionOptions(wrapped); got != "deploy\nnow" {
		t.Fatalf("strip = %q", got)
	}
	appended := AppendInteractionOptions("deploy\nnow")
	if !strings.HasPrefix(appended, "deploy\nnow\n\n") || StripInteractionOptions(appended) != "deploy\nnow" {
		t.Fatalf("appended = %q, strip = %q", appended, StripInteractionOptions(appended))
	}
	if got := StripInteractionOptions(appended[:20]); got != "deploy\nnow" {
		t.Fatalf("truncated preview strip = %q", got)
	}
	if got := StripInteractionOptions("plain text\n\nUser request:\nx"); got != "plain text\n\nUser request:\nx" {
		t.Fatalf("unwrapped text changed: %q", got)
	}
}

func TestNewAgentsInModeTable(t *testing.T) {
	cases := []struct {
		agent, raw, native, canonical string
		ok                            bool
	}{
		{"antigravity", "accept-edits", "acceptEdits", PermissionModeAcceptEdits, true},
		{"antigravity", "plan", "plan", PermissionModePlan, true},
		{"antigravity", "auto", "auto", PermissionModeAuto, true},
		{"antigravity", "yolo", "bypassPermissions", PermissionModeBypass, true},
		{"kimi", "yolo", "bypassPermissions", PermissionModeBypass, true},
		// kimi acp modes (P6); prompt mode lists only bypass.
		{"kimi", "auto", "auto", PermissionModeAuto, true},
		{"kimi", "plan", "plan", PermissionModePlan, true},
		{"kimi", "dangerously", "", "", false},
	}
	for _, c := range cases {
		native, canonical, ok := ResolvePermissionMode(c.agent, c.raw)
		if native != c.native || canonical != c.canonical || ok != c.ok {
			t.Errorf("ResolvePermissionMode(%s, %s) = %q %q %v", c.agent, c.raw, native, canonical, ok)
		}
	}
}

// T10.6: Kimi and agy update themselves; the upgrader must run the copy the
// agents use, even when it is not on PATH.
func TestSelfUpdatingAgentsUpgradeTheResolvedBinary(t *testing.T) {
	cases := []struct {
		target, path, resolved, route, update string
	}{
		{"kimi", "/u/.kimi-code/bin/kimi", "/u/.kimi-code/bin/kimi", installRouteStandalone, "'/u/.kimi-code/bin/kimi' upgrade --yes"},
		{"kimi", "/opt/homebrew/bin/kimi", "/opt/homebrew/lib/node_modules/@moonshot-ai/kimi-code/bin/kimi.js", installRouteNPM, "npm install -g @moonshot-ai/kimi-code@latest"},
		{"antigravity", "/u/.local/bin/agy", "/u/.local/bin/agy", installRouteStandalone, "'/u/.local/bin/agy' update"},
	}
	for _, c := range cases {
		install := &AgentCLIInstall{Path: c.path, Resolved: c.resolved}
		route := detectInstallRoute(c.target, install)
		if route != c.route {
			t.Errorf("%s route = %q, want %q", c.path, route, c.route)
			continue
		}
		cfg := DefaultAgentUpgradeConfig().Targets[c.target]
		got, reason := applyInstallRoute(c.target, cfg, AgentCLIInfo{Selected: install}, route)
		if reason != "" || got.UpdateCommand != c.update || got.VersionCommand != "'"+c.path+"' --version" {
			t.Errorf("%s: update=%q version=%q reason=%q", c.path, got.UpdateCommand, got.VersionCommand, reason)
		}
	}
	for alias, want := range map[string]string{"agy": "antigravity", "kimi-code": "kimi", "kimi": "kimi"} {
		if got := normalizeAgentUpgradeTarget(alias); got != want {
			t.Errorf("normalizeAgentUpgradeTarget(%q) = %q", alias, got)
		}
	}
	explicit := DefaultAgentUpgradeConfig().Targets["kimi"]
	explicit.UpdateCommand, explicit.UpdateCommandSet = "my-update", true
	if got, _ := applyInstallRoute("kimi", explicit, AgentCLIInfo{Selected: &AgentCLIInstall{Path: "/u/.kimi-code/bin/kimi"}}, installRouteStandalone); got.UpdateCommand != "my-update" {
		t.Fatalf("explicit update command overridden: %q", got.UpdateCommand)
	}
}

// T1.4: Kimi and agy register as Echo workers only in a bypass-equivalent mode.
func TestNewAgentsEchoWorkerModeValidation(t *testing.T) {
	cases := []struct {
		agentType, mode string
		ok              bool
	}{
		{"kimi", "bypassPermissions", true},
		{"kimi", "yolo", true},
		{"kimi", "", false},
		{"antigravity", "bypassPermissions", true},
		{"antigravity", "auto", false},
		{"antigravity", "default", false},
	}
	for _, c := range cases {
		proj := config.ProjectConfig{Name: "p", Agent: config.AgentConfig{Type: c.agentType, Options: map[string]any{"mode": c.mode}}}
		err := validateEchoWorkerMode(proj, "worker", c.agentType)
		if (err == nil) != c.ok {
			t.Errorf("%s mode %q: err = %v, want ok=%v", c.agentType, c.mode, err, c.ok)
		}
	}
}
