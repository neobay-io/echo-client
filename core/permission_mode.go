package core

import "strings"

// Canonical permission modes exposed in chat. Every agent maps them to its own
// native flags; aliases keep older config values working.
const (
	PermissionModeDefault     = "default"
	PermissionModeAcceptEdits = "acceptEdits"
	PermissionModePlan        = "plan"
	PermissionModeAuto        = "auto"
	PermissionModeBypass      = "bypassPermissions"
)

// permissionModeEntry binds one native mode key of an agent to its canonical
// meaning. Canonical is empty for agent-specific modes with no shared meaning
// (e.g. Claude's dontAsk, Cursor's ask).
type permissionModeEntry struct {
	Native    string
	Canonical string
	Aliases   []string // lower-case, in addition to Native and Canonical
}

// permissionModeTables is the single source of truth for mode aliases. Agent
// adapters normalize through it, and Echo worker registration uses it to tell
// whether a configured mode is bypass-equivalent.
var permissionModeTables = map[string][]permissionModeEntry{
	"claudecode": {
		{Native: "default", Canonical: PermissionModeDefault},
		{Native: "acceptEdits", Canonical: PermissionModeAcceptEdits, Aliases: []string{"accept-edits", "accept_edits", "edit"}},
		{Native: "plan", Canonical: PermissionModePlan},
		{Native: "auto", Canonical: PermissionModeAuto},
		{Native: "bypassPermissions", Canonical: PermissionModeBypass, Aliases: []string{"bypass-permissions", "bypass_permissions", "yolo"}},
		{Native: "dontAsk", Aliases: []string{"dont-ask", "dont_ask"}},
	},
	// Codex exec has no plan mode; it arrives with the app-server migration.
	"codex": {
		{Native: "default", Canonical: PermissionModeDefault, Aliases: []string{"suggest"}},
		{Native: "acceptEdits", Canonical: PermissionModeAcceptEdits, Aliases: []string{"accept-edits", "accept_edits", "auto-edit", "autoedit", "auto_edit", "edit"}},
		{Native: "auto", Canonical: PermissionModeAuto, Aliases: []string{"full-auto", "fullauto", "full_auto", "approve-for-me"}},
		{Native: "bypassPermissions", Canonical: PermissionModeBypass, Aliases: []string{"bypass-permissions", "bypass_permissions", "yolo", "bypass", "dangerously-bypass"}},
	},
	// agy: default forwards approvals to chat through a PreToolUse hook.
	"antigravity": {
		{Native: "default", Canonical: PermissionModeDefault},
		{Native: "acceptEdits", Canonical: PermissionModeAcceptEdits, Aliases: []string{"accept-edits", "accept_edits", "edit"}},
		{Native: "plan", Canonical: PermissionModePlan},
		{Native: "auto", Canonical: PermissionModeAuto},
		{Native: "bypassPermissions", Canonical: PermissionModeBypass, Aliases: []string{"bypass-permissions", "bypass_permissions", "yolo", "bypass", "dangerously-skip-permissions"}},
	},
	// Kimi Code prompt mode (-p) runs every tool without asking and rejects
	// --plan/--yolo/--auto, so bypass is the only honest mode until ACP (P6).
	// "auto" is deliberately not an alias: nothing reviews the actions.
	"kimi": {
		{Native: "bypassPermissions", Canonical: PermissionModeBypass, Aliases: []string{"bypass-permissions", "bypass_permissions", "yolo", "bypass"}},
	},
	"gemini": {
		{Native: "default", Canonical: PermissionModeDefault},
		{Native: "auto_edit", Canonical: PermissionModeAcceptEdits, Aliases: []string{"autoedit", "edit"}},
		{Native: "plan", Canonical: PermissionModePlan},
		{Native: "yolo", Canonical: PermissionModeBypass, Aliases: []string{"auto", "force"}},
	},
	"qoder": {
		{Native: "default", Canonical: PermissionModeDefault},
		{Native: "acceptEdits", Canonical: PermissionModeAcceptEdits, Aliases: []string{"accept-edits", "accept_edits", "edit"}},
		{Native: "plan", Canonical: PermissionModePlan},
		{Native: "dontAsk", Aliases: []string{"dont-ask", "dont_ask"}},
		{Native: "yolo", Canonical: PermissionModeBypass, Aliases: []string{"bypass", "dangerously-skip-permissions", "bypass-permissions", "bypass_permissions"}},
	},
	"opencode": {
		{Native: "default", Canonical: PermissionModeDefault},
		{Native: "yolo", Canonical: PermissionModeBypass, Aliases: []string{"auto", "force"}},
	},
	"iflow": {
		{Native: "default", Canonical: PermissionModeDefault},
		{Native: "auto-edit", Canonical: PermissionModeAcceptEdits, Aliases: []string{"auto_edit", "autoedit", "edit"}},
		{Native: "plan", Canonical: PermissionModePlan},
		{Native: "yolo", Canonical: PermissionModeBypass, Aliases: []string{"force", "auto"}},
	},
	"cursor": {
		{Native: "default", Canonical: PermissionModeDefault},
		{Native: "ask"},
		{Native: "plan", Canonical: PermissionModePlan},
		{Native: "force", Canonical: PermissionModeBypass, Aliases: []string{"yolo", "auto"}},
	},
}

// ResolvePermissionMode maps a user- or config-supplied mode to the agent's
// native key and its canonical meaning. ok is false for unknown values; callers
// must not silently fall back to default.
func ResolvePermissionMode(agentType, raw string) (native, canonical string, ok bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if key == "" {
		return "", "", false
	}
	table, known := permissionModeTables[strings.ToLower(strings.TrimSpace(agentType))]
	if !known {
		// Unknown agent types only get the universal bypass aliases.
		switch key {
		case "yolo", "bypasspermissions":
			return key, PermissionModeBypass, true
		}
		return "", "", false
	}
	for _, entry := range table {
		if strings.EqualFold(entry.Native, key) || (entry.Canonical != "" && strings.EqualFold(entry.Canonical, key)) {
			return entry.Native, entry.Canonical, true
		}
		for _, alias := range entry.Aliases {
			if alias == key {
				return entry.Native, entry.Canonical, true
			}
		}
	}
	return "", "", false
}

// NormalizePermissionMode resolves raw for agentType and falls back to the
// agent's default mode for unknown or empty values. Use it for config values
// at startup; chat commands must use ResolvePermissionMode and report errors.
func NormalizePermissionMode(agentType, raw string) string {
	if native, _, ok := ResolvePermissionMode(agentType, raw); ok {
		return native
	}
	return PermissionModeDefault
}

// IsBypassPermissionMode reports whether mode makes agentType skip all
// approvals (what Echo workers require).
func IsBypassPermissionMode(agentType, mode string) bool {
	_, canonical, ok := ResolvePermissionMode(agentType, mode)
	return ok && canonical == PermissionModeBypass
}

// resolveModeForSwitcher resolves a chat argument against the modes an agent
// actually offers. Agents without a table entry are matched on their listed
// keys only.
func resolveModeForSwitcher(agentType string, switcher ModeSwitcher, raw string) (string, bool) {
	listed := switcher.PermissionModes()
	native, _, ok := ResolvePermissionMode(agentType, raw)
	if !ok {
		native = strings.TrimSpace(raw)
	}
	for _, mode := range listed {
		if strings.EqualFold(mode.Key, native) {
			return mode.Key, true
		}
	}
	return "", false
}
