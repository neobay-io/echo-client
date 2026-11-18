package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// /usage and /context (T3.6).

// SessionUsageReporter reports what a running session has spent so far
// (shown by /status). Unlike UsageReporter it never starts a process.
type SessionUsageReporter interface {
	SessionUsage(ctx context.Context) (*UsageReport, error)
}

// NativeCompactor is an agent session that compacts its context through
// its own protocol instead of a "/compact" message (T5.6). Compact starts
// the compaction, which then ends like a turn (EventResult or EventError).
type NativeCompactor interface {
	Compact(ctx context.Context) error
}

// ErrCompactUnsupported: the session cannot compact (e.g. Codex in exec
// mode); /compact says so and leaves the session as it is.
var ErrCompactUnsupported = errors.New("this agent session cannot compact its context")

// compactStartTimeout bounds starting a native compaction.
var compactStartTimeout = 30 * time.Second

// usageTimeout covers agents that start a short-lived CLI to answer.
var usageTimeout = 60 * time.Second

func (e *Engine) chatAgentSession(sessionKey string) AgentSession {
	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state == nil || state.agentSession == nil || !state.agentSession.Alive() {
		return nil
	}
	return state.agentSession
}

// cmdUsage answers in the background: the agent may start a CLI to find
// out, and some platforms deliver one update at a time.
func (e *Engine) cmdUsage(p Platform, msg *Message) {
	ur, ok := e.agent.(UsageReporter)
	if !ok {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("usage_unsupported")))
		return
	}
	sess, replyCtx := e.chatAgentSession(msg.SessionKey), msg.ReplyCtx
	go func() {
		ctx, cancel := context.WithTimeout(e.ctx, usageTimeout)
		defer cancel()
		report, err := ur.Usage(ctx, sess)
		if err != nil {
			e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgKey("usage_failed")), err))
			return
		}
		e.reply(p, replyCtx, e.formatUsage(report))
	}()
}

func (e *Engine) formatUsage(r *UsageReport) string {
	var sb strings.Builder
	sb.WriteString(e.i18n.T(MsgKey("usage_title")))
	if r.Plan != "" {
		fmt.Fprintf(&sb, " (%s)", r.Plan)
	}
	for _, l := range r.Lines {
		sb.WriteString("\n- " + l.Name + ": ")
		if l.UsedPercent >= 0 {
			fmt.Fprintf(&sb, e.i18n.T(MsgKey("usage_used")), l.UsedPercent)
		} else {
			sb.WriteString("?")
		}
		if !l.ResetsAt.IsZero() {
			sb.WriteString(", ")
			fmt.Fprintf(&sb, e.i18n.T(MsgKey("usage_resets")), l.ResetsAt.Local().Format("01-02 15:04"))
		}
		if l.Note != "" {
			sb.WriteString(" (" + l.Note + ")")
		}
	}
	if r.HasCost {
		sb.WriteString("\n")
		fmt.Fprintf(&sb, e.i18n.T(MsgKey("usage_session_cost")), r.SessionCostUSD)
	}
	if len(r.Lines) == 0 && !r.HasCost {
		sb.WriteString("\n" + e.i18n.T(MsgKey("usage_empty")))
	}
	return sb.String()
}

func (e *Engine) cmdContext(p Platform, msg *Message) {
	sess := e.chatAgentSession(msg.SessionKey)
	cr, ok := sess.(ContextReporter)
	if sess == nil || !ok {
		key := "context_no_session"
		if sess != nil {
			key = "context_unsupported"
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey(key)))
		return
	}
	replyCtx := msg.ReplyCtx
	go func() {
		ctx, cancel := context.WithTimeout(e.ctx, usageTimeout)
		defer cancel()
		report, err := cr.ContextUsage(ctx)
		if err != nil {
			e.reply(p, replyCtx, fmt.Sprintf(e.i18n.T(MsgKey("usage_failed")), err))
			return
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, e.i18n.T(MsgKey("context_title")), formatTokens(report.UsedTokens), formatTokens(report.MaxTokens), report.Percent)
		for _, c := range report.Categories {
			fmt.Fprintf(&sb, "\n- %s: %s", c.Name, formatTokens(c.Tokens))
		}
		e.reply(p, replyCtx, sb.String())
	}()
}

// sessionCostLine is the /status line with what the chat's running process
// has spent, or "" (never starts a process).
func (e *Engine) sessionCostLine(sessionKey string) string {
	sur, ok := e.chatAgentSession(sessionKey).(SessionUsageReporter)
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	r, err := sur.SessionUsage(ctx)
	if err != nil || !r.HasCost {
		return ""
	}
	return fmt.Sprintf(e.i18n.T(MsgKey("status_cost")), r.SessionCostUSD)
}

// formatTokens writes 28561 as 28,561.
func formatTokens(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}
