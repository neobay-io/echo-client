package core

import (
	"strings"
	"sync"
)

// AgentCLICheck is one smoke check of an agent CLI: run the binary with Args
// and require every option in Flags to appear in the output (normally the
// help text). Agent adapters register the options they pass, so an upgrade
// that drops one is caught, and rolled back, before chats fail with
// "unknown option".
type AgentCLICheck struct {
	Args  []string
	Flags []string
}

var (
	agentCLIChecksMu sync.RWMutex
	agentCLIChecks   = map[string][]AgentCLICheck{}
)

// RegisterAgentCLIChecks sets the smoke checks for an agent type. Agent
// packages call it from init.
func RegisterAgentCLIChecks(agentType string, checks ...AgentCLICheck) {
	agentCLIChecksMu.Lock()
	defer agentCLIChecksMu.Unlock()
	agentCLIChecks[agentType] = append([]AgentCLICheck(nil), checks...)
}

func agentCLIChecksFor(agentType string) []AgentCLICheck {
	agentCLIChecksMu.RLock()
	defer agentCLIChecksMu.RUnlock()
	return append([]AgentCLICheck(nil), agentCLIChecks[agentType]...)
}

// missingCLIFlags returns the flags help does not list.
func missingCLIFlags(help string, flags []string) []string {
	var missing []string
	for _, flag := range flags {
		if !helpHasFlag(help, flag) {
			missing = append(missing, flag)
		}
	}
	return missing
}

// helpHasFlag reports whether help lists flag as an option of its own, not as
// part of a longer one ("--prompt" does not match "--prompt-file").
func helpHasFlag(help, flag string) bool {
	if flag == "" {
		return true
	}
	for i := 0; i < len(help); {
		j := strings.Index(help[i:], flag)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(flag)
		if (start == 0 || !isCLIFlagChar(help[start-1])) && (end == len(help) || !isCLIFlagChar(help[end])) {
			return true
		}
		i = start + 1
	}
	return false
}

func isCLIFlagChar(c byte) bool {
	return c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
