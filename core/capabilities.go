package core

import (
	"fmt"
	"strings"
)

// Auth flows an agent can use for remote login from chat.
const (
	// AuthFlowURLCode: the CLI prints an authorization URL and waits for the
	// user to paste back a one-time code (Claude Code, agy).
	AuthFlowURLCode = "url_code"
	// AuthFlowDeviceCode: the CLI prints a verification URL plus a user code
	// and polls until the user approves it in a browser (Codex, Kimi Code).
	AuthFlowDeviceCode = "device_code"
)

// AuthCaps describes which remote-login operations an agent supports.
type AuthCaps struct {
	Login  bool   `json:"login,omitempty"`
	Status bool   `json:"status,omitempty"`
	Logout bool   `json:"logout,omitempty"`
	Flow   string `json:"flow,omitempty"` // AuthFlowURLCode | AuthFlowDeviceCode
}

// SessionCaps describes agent-side session management support.
type SessionCaps struct {
	List    bool `json:"list,omitempty"`
	Delete  bool `json:"delete,omitempty"`
	History bool `json:"history,omitempty"`
}

// AgentCapabilities is the single place commands, /help, /status and the Echo
// worker registration look at to decide what an agent can do, instead of
// comparing agent names.
//
// Fields backed by an optional interface (Models, Efforts, Modes, Sessions,
// Compact) are derived from the interfaces the agent implements. Fields with
// no interface in core yet (Auth, Live*, Steer, Interrupt, Usage, AskUser,
// EffortViaModel) must be declared through CapabilityProvider.
type AgentCapabilities struct {
	Auth AuthCaps `json:"auth"`

	Models    bool `json:"models,omitempty"`     // ModelSwitcher
	LiveModel bool `json:"live_model,omitempty"` // switch without restarting the agent process

	Efforts []string `json:"efforts,omitempty"` // ReasoningLevelSwitcher option keys
	// EffortViaModel: reasoning depth is chosen through model tiers (e.g. Qoder),
	// so /effort should point users to /model.
	EffortViaModel bool `json:"effort_via_model,omitempty"`

	Modes    []string `json:"modes,omitempty"` // ModeSwitcher keys
	LiveMode bool     `json:"live_mode,omitempty"`

	Sessions SessionCaps `json:"sessions"`

	Steer     bool `json:"steer,omitempty"`     // inject a message into the running turn
	Interrupt bool `json:"interrupt,omitempty"` // graceful interrupt without killing the process
	Compact   bool `json:"compact,omitempty"`   // ContextCompressor with a native command
	Usage     bool `json:"usage,omitempty"`     // usage / quota / context reporting
	AskUser   bool `json:"ask_user,omitempty"`  // structured question / plan approval cards
}

// CapabilityProvider is implemented by agents that declare capabilities which
// cannot be derived from optional interfaces.
type CapabilityProvider interface {
	Capabilities() AgentCapabilities
}

// CapabilitiesOf returns the effective capabilities of agent: the declared
// ones (if any) plus everything derived from implemented optional interfaces.
func CapabilitiesOf(agent Agent) AgentCapabilities {
	var caps AgentCapabilities
	if agent == nil {
		return caps
	}
	if p, ok := agent.(CapabilityProvider); ok {
		caps = p.Capabilities()
	}
	derived := derivedCapabilities(agent)
	caps.Models = derived.Models
	if caps.Efforts == nil {
		caps.Efforts = derived.Efforts
	}
	if caps.Modes == nil {
		caps.Modes = derived.Modes
	}
	caps.Sessions = derived.Sessions
	caps.Compact = derived.Compact
	return caps
}

func derivedCapabilities(agent Agent) AgentCapabilities {
	caps := AgentCapabilities{Sessions: SessionCaps{List: true}}
	if _, ok := agent.(ModelSwitcher); ok {
		caps.Models = true
	}
	if rs, ok := agent.(ReasoningLevelSwitcher); ok {
		for _, level := range rs.AvailableReasoningLevels() {
			if name := strings.TrimSpace(level.Name); name != "" {
				caps.Efforts = append(caps.Efforts, name)
			}
		}
	}
	if ms, ok := agent.(ModeSwitcher); ok {
		for _, mode := range ms.PermissionModes() {
			if key := strings.TrimSpace(mode.Key); key != "" {
				caps.Modes = append(caps.Modes, key)
			}
		}
	}
	if _, ok := agent.(SessionDeleter); ok {
		caps.Sessions.Delete = true
	}
	if _, ok := agent.(HistoryProvider); ok {
		caps.Sessions.History = true
	}
	if cc, ok := agent.(ContextCompressor); ok && strings.TrimSpace(cc.CompressCommand()) != "" {
		caps.Compact = true
	}
	return caps
}

// ValidateCapabilities reports declarations that have no implementation behind
// them. Agent packages call it from their tests so "declared" always means
// "implemented".
func ValidateCapabilities(agent Agent) []error {
	p, ok := agent.(CapabilityProvider)
	if !ok {
		return nil
	}
	declared := p.Capabilities()
	derived := derivedCapabilities(agent)
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{agent.Name()}, args...)...))
	}

	if declared.Auth.Login || declared.Auth.Status || declared.Auth.Logout {
		switch declared.Auth.Flow {
		case AuthFlowURLCode, AuthFlowDeviceCode:
		default:
			fail("auth declared with unknown flow %q", declared.Auth.Flow)
		}
		if !supportsRemoteAuth(agent.Name()) {
			fail("auth declared but no remote login implementation is registered")
		}
	}
	if declared.Models && !derived.Models {
		fail("models declared but ModelSwitcher is not implemented")
	}
	if declared.LiveModel && !derived.Models {
		fail("live_model declared without ModelSwitcher")
	}
	if len(declared.Efforts) > 0 && len(derived.Efforts) == 0 {
		fail("efforts declared but ReasoningLevelSwitcher is not implemented")
	}
	if declared.EffortViaModel && !derived.Models {
		fail("effort_via_model declared without ModelSwitcher")
	}
	if len(declared.Modes) > 0 && len(derived.Modes) == 0 {
		fail("modes declared but ModeSwitcher is not implemented")
	}
	for _, mode := range declared.Modes {
		if !containsFold(derived.Modes, mode) {
			fail("mode %q declared but not listed by PermissionModes()", mode)
		}
	}
	if declared.LiveMode && len(derived.Modes) == 0 {
		fail("live_mode declared without ModeSwitcher")
	}
	if declared.Sessions.Delete && !derived.Sessions.Delete {
		fail("session delete declared but SessionDeleter is not implemented")
	}
	if declared.Sessions.History && !derived.Sessions.History {
		fail("session history declared but HistoryProvider is not implemented")
	}
	if declared.Compact && !derived.Compact {
		fail("compact declared but ContextCompressor is not implemented")
	}
	// No core interface exists for these yet; declaring them would make
	// commands promise behavior nothing delivers.
	if declared.Steer {
		fail("steer declared but core has no steering interface yet")
	}
	if declared.Interrupt {
		fail("interrupt declared but core has no interrupt interface yet")
	}
	if declared.Usage {
		fail("usage declared but core has no usage interface yet")
	}
	if declared.AskUser {
		fail("ask_user declared but core has no ask-user interface yet")
	}
	return errs
}

// supportsRemoteAuth reports whether AgentAuthManager can drive the agent's
// official login. Claude Code is the only implementation until P2 adds a
// per-agent auth provider.
func supportsRemoteAuth(agentName string) bool {
	return agentName == "claudecode"
}

func containsFold(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), target) {
			return true
		}
	}
	return false
}

// formatCapabilitySummary lists supported capabilities for /status.
func formatCapabilitySummary(caps AgentCapabilities) string {
	var parts []string
	if caps.Auth.Login || caps.Auth.Status {
		parts = append(parts, "/auth")
	}
	if caps.Models {
		parts = append(parts, "/model")
	}
	if len(caps.Efforts) > 0 {
		parts = append(parts, "/effort ("+strings.Join(caps.Efforts, "/")+")")
	} else if caps.EffortViaModel {
		parts = append(parts, "/effort → /model")
	}
	if len(caps.Modes) > 0 {
		parts = append(parts, "/mode ("+strings.Join(caps.Modes, "/")+")")
	}
	if caps.Compact {
		parts = append(parts, "/compress")
	}
	if caps.Sessions.Delete {
		parts = append(parts, "/delete purge")
	}
	if caps.Steer {
		parts = append(parts, "steer")
	}
	if caps.Interrupt {
		parts = append(parts, "interrupt")
	}
	if caps.Usage {
		parts = append(parts, "usage")
	}
	return strings.Join(parts, " · ")
}

// capabilityCommands maps capability-gated builtin commands to whether the
// agent supports them. Commands not listed are always available.
func capabilityCommands(caps AgentCapabilities) map[string]bool {
	return map[string]bool{
		"auth":     caps.Auth.Login || caps.Auth.Status,
		"model":    caps.Models,
		"effort":   len(caps.Efforts) > 0 || caps.EffortViaModel,
		"mode":     len(caps.Modes) > 0,
		"compress": caps.Compact,
	}
}
