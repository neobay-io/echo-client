package core

import (
	"context"
	"time"
)

// Optional interfaces for agents whose CLI keeps a process with a control
// channel (Claude Code's stream-json protocol, later Codex app-server). The
// engine uses them when present and falls back to restarting the process,
// queueing, or killing it otherwise.

// LiveSettingsApplier switches model, effort and permission mode of a
// running agent session without restarting it, so the conversation keeps
// going in the same process. opts are the chat's settings; the agent
// resolves them against its defaults as it does when starting a session.
type LiveSettingsApplier interface {
	ApplyLiveSettings(ctx context.Context, session AgentSession, opts AgentSessionOptions) error
}

// Steerer delivers a message to the turn that is running, which reads it
// after its current step instead of after the turn ends.
type Steerer interface {
	Steer(prompt string, images []ImageAttachment, files []FileAttachment) error
}

// Interrupter stops the running turn and keeps the session (and its process)
// usable. The turn then ends with an EventResult whose Interrupted is set.
type Interrupter interface {
	Interrupt(ctx context.Context) error
}

// UsageReporter reports plan limits and spending. session is the chat's
// running session, if any; agents may answer without one.
type UsageReporter interface {
	Usage(ctx context.Context, session AgentSession) (*UsageReport, error)
}

// ContextReporter reports how much of the model's context window a running
// session uses.
type ContextReporter interface {
	ContextUsage(ctx context.Context) (*ContextReport, error)
}

// ModelEffortProvider lists the effort levels a model supports ("" is the
// agent's default model); nil when unknown.
type ModelEffortProvider interface {
	EffortsForModel(ctx context.Context, model string) []string
}

// UsageReport is what /usage shows.
type UsageReport struct {
	Plan           string
	Lines          []UsageLine
	SessionCostUSD float64
	HasCost        bool
}

// UsageLine is one limit: how much of it is used and when it resets.
type UsageLine struct {
	Name        string
	UsedPercent int // -1 when unknown
	ResetsAt    time.Time
	Note        string
}

// ContextReport is what /context shows.
type ContextReport struct {
	UsedTokens int
	MaxTokens  int
	Percent    int
	Categories []ContextCategory
}

// ContextCategory is one part of the context (system prompt, tools, messages).
type ContextCategory struct {
	Name   string
	Tokens int
}

// User-decision tools: the agent asks the user something instead of asking
// permission. They are never auto-approved.
const (
	ToolAskUserQuestion = "AskUserQuestion"
	ToolExitPlanMode    = "ExitPlanMode"
)

// IsUserDecisionTool reports whether a permission request is really a
// question or plan for the user.
func IsUserDecisionTool(name string) bool {
	return name == ToolAskUserQuestion || name == ToolExitPlanMode
}
