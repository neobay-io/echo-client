package core

import (
	"context"
	"strings"
)

// latestVersionCommand is how to look up the newest release of a target, or
// "" when there is no cheap way (agy; its updater checks by itself). npm's
// "latest" tag is the newest release on every channel, so a CLI at or above
// it is up to date whatever channel or install route it uses.
func latestVersionCommand(name string, target AgentUpgradeTargetConfig, selectedPath string) string {
	if cmd := strings.TrimSpace(target.LatestCommand); cmd != "" {
		return cmd
	}
	if pkg := npmPackages[name]; pkg != "" {
		return "npm view " + shellQuote(pkg) + " version --loglevel=error"
	}
	if name == "qoder" && selectedPath != "" {
		return shellQuote(selectedPath) + " update --check"
	}
	return ""
}

// lastAgentVersion returns the last version number in s: the new version in
// "Update available: 1.1.89 -> 1.1.91", the only one in "0.169.1".
func lastAgentVersion(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ','
	})
	for i := len(fields) - 1; i >= 0; i-- {
		v := extractAgentVersion(fields[i])
		if _, ok := parseVersionParts(v); ok && strings.Contains(v, ".") {
			return v
		}
	}
	return ""
}

// latestAgentVersion looks up the newest release of a target; "" if unknown.
// A failed lookup never blocks the upgrade.
func latestAgentVersion(ctx context.Context, runner AgentUpgradeCommandRunner, cmd string) string {
	if cmd == "" {
		return ""
	}
	out, err := runner(ctx, cmd)
	if err != nil {
		return ""
	}
	return lastAgentVersion(out)
}

// atOrAboveVersion reports whether installed is known to be >= latest.
func atOrAboveVersion(installed, latest string) bool {
	ok, cmp := compareVersionStrings(installed, latest)
	return ok && cmp >= 0
}
