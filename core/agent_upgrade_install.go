package core

import (
	"context"
	"fmt"
	"strings"
)

// Install routes detected from where an agent CLI actually resolves.
const (
	installRouteStandalone = "standalone" // vendor installer / self-updating native install
	installRouteNative     = "native"     // Claude Code native install
	installRouteNPM        = "npm"
	installRouteBrew       = "brew"
	installRouteUnknown    = "unknown"
)

// codexStandaloneInstallCommand upgrades the standalone Codex install with
// OpenAI's installer. It downloads to a temp file first so a failed download
// returns non-zero (`curl | sh` would hide it). The installer skips the
// download when the latest release is already installed. `codex update`
// cannot be used: it fails to detect installs whose ~/.codex/packages is a
// symlink.
const codexStandaloneInstallCommand = `f=$(mktemp) && curl -fsSL https://chatgpt.com/codex/install.sh -o "$f" && CODEX_NON_INTERACTIVE=1 sh "$f"; rc=$?; rm -f "$f"; exit $rc`

// defaultUpgradeCLIInspector resolves agent CLIs on this process's PATH, the
// same PATH agent sessions are started with. Tests replace it.
var defaultUpgradeCLIInspector = InspectAgentCLI

// detectInstallRoute classifies an install by its resolved path.
func detectInstallRoute(target string, install *AgentCLIInstall) string {
	if install == nil {
		return ""
	}
	paths := []string{install.Path, install.Resolved}
	has := func(fragment string) bool {
		for _, p := range paths {
			if p != "" && strings.Contains(p, fragment) {
				return true
			}
		}
		return false
	}
	switch target {
	case "codex":
		switch {
		case has("/packages/standalone/"):
			return installRouteStandalone
		case has("/node_modules/@openai/codex"):
			return installRouteNPM
		case has("/Caskroom/codex") || has("/Cellar/codex"):
			return installRouteBrew
		}
	case "claudecode":
		switch {
		case has("/.local/share/claude/") || strings.HasSuffix(install.Path, "/.local/bin/claude"):
			return installRouteNative
		case has("/node_modules/@anthropic-ai/claude-code"):
			return installRouteNPM
		case has("/Caskroom/claude-code"):
			return installRouteBrew
		}
	case "gemini":
		if has("/node_modules/@google/gemini-cli") {
			return installRouteNPM
		}
		if has("/Cellar/gemini-cli") {
			return installRouteBrew
		}
	case "qoder":
		return installRouteStandalone // qodercli updates itself
	case "kimi":
		switch {
		case has("/node_modules/@moonshot-ai/kimi-code"):
			return installRouteNPM
		case has("/.kimi-code/"):
			return installRouteStandalone
		}
	case "antigravity":
		return installRouteStandalone // agy updates itself
	}
	return installRouteUnknown
}

// selfUpdateArgs are the update subcommands of CLIs that update themselves.
var selfUpdateArgs = map[string]string{
	"qoder":       "update",
	"kimi":        "upgrade --yes",
	"antigravity": "update",
}

// applyInstallRoute points the version probe at the binary agents run and,
// unless the user configured commands explicitly, picks the update command
// for the detected install route.
func applyInstallRoute(name string, target AgentUpgradeTargetConfig, info AgentCLIInfo, route string) (AgentUpgradeTargetConfig, string) {
	if info.Selected == nil {
		return target, ""
	}
	if !target.VersionCommandSet {
		target.VersionCommand = shellQuote(info.Selected.Path) + " --version"
	}
	if target.UpdateCommandSet || target.Strategy == "custom" {
		return target, ""
	}
	// The route picks the command. It may switch between the two upgrading
	// strategies, but never turns observe or disabled into an upgrade.
	setStrategy := func(strategy string) {
		switch target.Strategy {
		case "", "builtin", "package_manager":
			target.Strategy = strategy
		}
	}
	switch name {
	case "codex":
		switch route {
		case installRouteStandalone:
			if target.PinVersion != "" {
				return target, "pin_version is not supported for the standalone codex install"
			}
			target.UpdateCommand = codexStandaloneInstallCommand
			setStrategy("builtin")
		case installRouteBrew:
			target.UpdateCommand = "brew upgrade --cask codex"
			setStrategy("package_manager")
		case installRouteNPM:
			setStrategy("package_manager")
		}
	case "gemini":
		if route == installRouteBrew && target.Strategy == "package_manager" {
			target.UpdateCommand = "brew upgrade gemini-cli"
		}
	case "kimi":
		if route == installRouteNPM {
			target.UpdateCommand = "npm install -g @moonshot-ai/kimi-code@latest"
			setStrategy("package_manager")
			break
		}
		fallthrough
	case "qoder", "antigravity":
		// Run the self-updater of the binary agents use; it may not be on
		// the service's PATH (Kimi's installer puts it in ~/.kimi-code/bin).
		if route == installRouteStandalone {
			target.UpdateCommand = shellQuote(info.Selected.Path) + " " + selfUpdateArgs[name]
		}
	}
	return target, ""
}

// otherInstallsWarning describes copies of the CLI on PATH that agents do not
// run; upgrading one of those instead of the selected binary is how upgrades
// silently stopped working before.
func otherInstallsWarning(info AgentCLIInfo) string {
	if len(info.Others) == 0 {
		return ""
	}
	return fmt.Sprintf("other copies of %s on PATH are not upgraded: %s", info.Binary, describeInstalls(info.Others))
}

func (m *AgentUpgradeManager) inspectTarget(ctx context.Context, name string) AgentCLIInfo {
	m.mu.Lock()
	inspect := m.inspectCLI
	m.mu.Unlock()
	if inspect == nil {
		inspect = defaultUpgradeCLIInspector
	}
	return inspect(ctx, name)
}
