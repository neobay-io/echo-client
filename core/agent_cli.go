package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// agentBinMap maps agent types to the CLI executable they launch.
var agentBinMap = map[string]string{
	"claudecode":  "claude",
	"cursor":      "agent",
	"codex":       "codex",
	"gemini":      "gemini",
	"iflow":       "iflow",
	"opencode":    "opencode",
	"qoder":       "qodercli",
	"antigravity": "agy",
	"kimi":        "kimi",
}

// DoctorAgentTypes are the agent CLIs `echo-client doctor` inspects by default.
var DoctorAgentTypes = []string{"claudecode", "codex", "antigravity", "kimi", "gemini", "qoder"}

// agentFallbackDirs are vendor install directories, relative to the home
// directory, that service managers (launchd, systemd) usually leave off PATH
// because the vendor installer only edits shell rc files.
var agentFallbackDirs = map[string][]string{
	"kimi": {".kimi-code/bin"},
}

// agentCLIHomeDir is replaced in tests.
var agentCLIHomeDir = os.UserHomeDir

// AgentCLIBinary returns the executable name an agent type runs.
func AgentCLIBinary(agentType string) string {
	if bin, ok := agentBinMap[agentType]; ok {
		return bin
	}
	return agentType
}

// ResolveAgentCLI returns the executable an agent type runs: the first match
// on PATH, else the vendor's default install location. Agent adapters,
// doctor and the upgrader all use it so they agree on which binary that is.
func ResolveAgentCLI(agentType string) (string, error) {
	bin := AgentCLIBinary(agentType)
	if path, err := exec.LookPath(bin); err == nil {
		return path, nil
	}
	if path := fallbackAgentCLI(agentType, bin); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("%q not found in PATH", bin)
}

func fallbackAgentCLI(agentType, bin string) string {
	dirs := agentFallbackDirs[agentType]
	if len(dirs) == 0 {
		return ""
	}
	home, err := agentCLIHomeDir()
	if err != nil || home == "" {
		return ""
	}
	for _, rel := range dirs {
		candidate := filepath.Join(home, rel, bin)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// AgentCLIInstall is one executable with the CLI's name on PATH.
type AgentCLIInstall struct {
	Path     string // entry on PATH
	Resolved string // after following symlinks; empty if broken
	Broken   bool   // dangling symlink or unreadable target
	OffPath  bool   // found in the vendor's install directory, not on PATH
}

// AgentCLIInfo describes how an agent CLI resolves for this process.
type AgentCLIInfo struct {
	Agent    string
	Binary   string
	Selected *AgentCLIInstall // what exec.LookPath picks, i.e. what agents run
	Version  string
	// VersionErr is set when the selected binary fails to report a version
	// (often a half-finished install).
	VersionErr string
	// Others are further PATH entries with the same name that resolve to a
	// different file, or are broken. Upgrading one copy while the service runs
	// another is how auto-upgrades silently stop working.
	Others []AgentCLIInstall
}

// InspectAgentCLI resolves an agent CLI exactly as agents launch it (this
// process's PATH), reads its version and lists other copies on PATH.
func InspectAgentCLI(ctx context.Context, agentType string) AgentCLIInfo {
	info := AgentCLIInfo{Agent: agentType, Binary: AgentCLIBinary(agentType)}
	installs := findOnPath(info.Binary, os.Getenv("PATH"))

	if selectedPath, err := exec.LookPath(info.Binary); err == nil {
		for _, install := range installs {
			if install.Path == selectedPath {
				selected := install
				info.Selected = &selected
				break
			}
		}
		if info.Selected == nil { // e.g. PATH entry spelled differently
			resolved, _ := filepath.EvalSymlinks(selectedPath)
			info.Selected = &AgentCLIInstall{Path: selectedPath, Resolved: resolved}
		}
	} else if fallback := fallbackAgentCLI(agentType, info.Binary); fallback != "" {
		resolved, _ := filepath.EvalSymlinks(fallback)
		info.Selected = &AgentCLIInstall{Path: fallback, Resolved: resolved, OffPath: true}
	}
	for _, install := range installs {
		if info.Selected != nil && install.Path == info.Selected.Path {
			continue
		}
		if install.Broken || info.Selected == nil || install.Resolved != info.Selected.Resolved {
			info.Others = append(info.Others, install)
		}
	}
	if info.Selected == nil {
		return info
	}

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, verr := exec.CommandContext(tctx, info.Selected.Path, "--version").CombinedOutput()
	text := firstLine(strings.TrimSpace(string(out)), 80)
	if verr != nil {
		if text == "" {
			text = verr.Error()
		}
		info.VersionErr = text
	} else {
		info.Version = text
	}
	return info
}

// findOnPath lists every executable named bin in PATH order, de-duplicating
// repeated PATH directories.
func findOnPath(bin, pathEnv string) []AgentCLIInstall {
	var installs []AgentCLIInstall
	seenDirs := make(map[string]bool)
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		clean := filepath.Clean(dir)
		if seenDirs[clean] {
			continue
		}
		seenDirs[clean] = true
		candidate := filepath.Join(clean, bin)
		lst, err := os.Lstat(candidate)
		if err != nil || lst.IsDir() {
			continue
		}
		install := AgentCLIInstall{Path: candidate}
		resolved, rerr := filepath.EvalSymlinks(candidate)
		if rerr != nil {
			install.Broken = true
		} else if st, serr := os.Stat(resolved); serr != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			install.Broken = true
			install.Resolved = resolved
		} else {
			install.Resolved = resolved
		}
		installs = append(installs, install)
	}
	return installs
}

func firstLine(s string, max int) string {
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// AgentCLIDoctorResult renders an AgentCLIInfo as a doctor check.
func AgentCLIDoctorResult(info AgentCLIInfo) DoctorCheckResult {
	result := DoctorCheckResult{Name: fmt.Sprintf("Agent CLI (%s)", info.Binary)}
	if info.Selected == nil {
		result.Status = DoctorFail
		result.Detail = "not found in PATH"
		if len(info.Others) > 0 {
			result.Detail += "; broken: " + describeInstalls(info.Others)
		}
		return result
	}
	parts := []string{}
	if info.Version != "" {
		parts = append(parts, info.Version)
	}
	parts = append(parts, describeInstall(*info.Selected))
	result.Status = DoctorPass
	if info.VersionErr != "" {
		result.Status = DoctorWarn
		parts = append(parts, "version check failed: "+info.VersionErr)
	}
	if len(info.Others) > 0 {
		result.Status = DoctorWarn
		parts = append(parts, "other copies on PATH (not used by the service): "+describeInstalls(info.Others))
	}
	result.Detail = strings.Join(parts, " · ")
	return result
}

func describeInstall(install AgentCLIInstall) string {
	s := install.Path
	if install.Resolved != "" && install.Resolved != install.Path {
		s += " → " + install.Resolved
	}
	if install.Broken {
		s += " (broken)"
	}
	if install.OffPath {
		s += " (not on PATH; vendor install location)"
	}
	return s
}

func describeInstalls(installs []AgentCLIInstall) string {
	parts := make([]string, 0, len(installs))
	for _, install := range installs {
		parts = append(parts, describeInstall(install))
	}
	return strings.Join(parts, ", ")
}
