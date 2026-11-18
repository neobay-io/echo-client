package core

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// Questions and plans (T3.5). Claude Code asks the user through permission
// requests for its AskUserQuestion and ExitPlanMode tools. Instead of an
// allow/deny prompt, the chat gets the question with one button per option,
// or the plan with Approve / Keep planning; the answer goes back as the
// tool's input (AskUserQuestion: {"answers": {question: label}}) or as a
// denial carrying the user's feedback.

// pendingDecision is a question or plan waiting for the user's answer.
// Replies can arrive concurrently (button presses, typed answers); mu
// serializes them and done makes the answer final once.
type pendingDecision struct {
	mu        sync.Mutex
	done      bool
	kind      string // ToolAskUserQuestion | ToolExitPlanMode
	questions []askQuestion
	index     int               // question being asked
	answers   map[string]string // question text → answer
	plan      string
}

type askQuestion struct {
	Question    string
	Header      string
	Options     []askOption
	MultiSelect bool
}

type askOption struct {
	Label       string
	Description string
}

func newPendingDecision(toolName string, input map[string]any) *pendingDecision {
	d := &pendingDecision{kind: toolName, answers: map[string]string{}}
	switch toolName {
	case ToolAskUserQuestion:
		d.questions = parseAskUserQuestions(input)
		if len(d.questions) == 0 {
			return nil
		}
	case ToolExitPlanMode:
		d.plan, _ = input["plan"].(string)
	default:
		return nil
	}
	return d
}

func parseAskUserQuestions(input map[string]any) []askQuestion {
	list, _ := input["questions"].([]any)
	var out []askQuestion
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		q := askQuestion{}
		q.Question, _ = m["question"].(string)
		q.Header, _ = m["header"].(string)
		q.MultiSelect, _ = m["multiSelect"].(bool)
		if strings.TrimSpace(q.Question) == "" {
			continue
		}
		opts, _ := m["options"].([]any)
		for _, o := range opts {
			om, _ := o.(map[string]any)
			if om == nil {
				continue
			}
			label, _ := om["label"].(string)
			desc, _ := om["description"].(string)
			if strings.TrimSpace(label) != "" {
				q.Options = append(q.Options, askOption{Label: label, Description: desc})
			}
		}
		out = append(out, q)
	}
	return out
}

// sendDecisionPrompt shows the current question, or the plan.
func (e *Engine) sendDecisionPrompt(sessionKey string, p Platform, replyCtx any, d *pendingDecision) {
	if d.kind == ToolExitPlanMode {
		plan := strings.TrimSpace(d.plan)
		if plan == "" {
			plan = e.i18n.T(MsgKey("plan_empty"))
		}
		text := fmt.Sprintf(e.i18n.T(MsgKey("plan_prompt")), truncateIf(plan, 3500))
		e.replyWithInteraction(sessionKey, p, replyCtx, text, [][]interactionChoice{{
			{ID: "plan_approve", Label: e.i18n.T(MsgKey("plan_btn_approve")), SendText: "approve plan", MatchTexts: []string{"approve plan", "approve", "yes"}},
			{ID: "plan_keep", Label: e.i18n.T(MsgKey("plan_btn_keep")), SendText: "keep planning", MatchTexts: []string{"keep planning", "no"}},
		}}, false)
		return
	}
	q := d.questions[d.index]
	var sb strings.Builder
	if len(d.questions) > 1 {
		fmt.Fprintf(&sb, "(%d/%d) ", d.index+1, len(d.questions))
	}
	if q.Header != "" {
		fmt.Fprintf(&sb, "**%s** — ", q.Header)
	}
	sb.WriteString(q.Question)
	sb.WriteString("\n")
	rows := make([][]interactionChoice, 0, len(q.Options))
	for i, o := range q.Options {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, o.Label)
		if o.Description != "" {
			fmt.Fprintf(&sb, " — %s", o.Description)
		}
		// The button sends the option's number: a label is the agent's text
		// and could look like a bridge command ("/mode yolo").
		rows = append(rows, []interactionChoice{{
			ID: fmt.Sprintf("ask_%d", i+1), Label: o.Label, SendText: strconv.Itoa(i + 1),
			MatchTexts: []string{strconv.Itoa(i + 1)},
		}})
	}
	hint := "ask_hint"
	if q.MultiSelect {
		hint = "ask_hint_multi"
	}
	sb.WriteString("\n\n")
	sb.WriteString(e.i18n.T(MsgKey(hint)))
	e.replyWithInteraction(sessionKey, p, replyCtx, sb.String(), rows, false)
}

// answerFor turns a reply into an answer to q: option numbers ("2", or
// "1,3" for multi-select), option labels, or free text (the "Other" answer).
func (q askQuestion) answerFor(reply string) string {
	reply = strings.TrimSpace(reply)
	pick := func(token string) (string, bool) {
		token = strings.TrimSpace(token)
		if n, err := strconv.Atoi(token); err == nil && n >= 1 && n <= len(q.Options) {
			return q.Options[n-1].Label, true
		}
		for _, o := range q.Options {
			if strings.EqualFold(o.Label, token) {
				return o.Label, true
			}
		}
		return "", false
	}
	if label, ok := pick(reply); ok {
		return label
	}
	if q.MultiSelect {
		var labels []string
		for _, part := range strings.FieldsFunc(reply, func(r rune) bool { return r == ',' || r == '，' || r == ' ' }) {
			label, ok := pick(part)
			if !ok {
				return reply
			}
			labels = append(labels, label)
		}
		if len(labels) > 0 {
			return strings.Join(labels, ", ")
		}
	}
	return reply
}

// handleDecisionReply records the user's reply to a question or plan. It
// reports whether the pending request was resolved.
func (e *Engine) handleDecisionReply(p Platform, msg *Message, state *interactiveState, pending *pendingPermission, content string) bool {
	d := pending.decision
	d.mu.Lock()
	if d.done {
		d.mu.Unlock()
		return false // already answered by a concurrent reply
	}
	result, final := e.decisionResult(p, msg, pending, content)
	if !final {
		e.sendDecisionPrompt(msg.SessionKey, p, msg.ReplyCtx, d)
		d.mu.Unlock()
		return false
	}
	d.done = true
	d.mu.Unlock()
	if err := state.agentSession.RespondPermission(pending.RequestID, result); err != nil {
		slog.Error("failed to send the answer", "tool", pending.ToolName, "error", err)
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
	}
	return true
}

// decisionResult applies a reply to the decision (d.mu held). It returns
// the response once the decision is complete, or final=false while more
// questions remain.
func (e *Engine) decisionResult(p Platform, msg *Message, pending *pendingPermission, content string) (PermissionResult, bool) {
	d := pending.decision
	lower := strings.ToLower(strings.TrimSpace(content))
	var result PermissionResult
	switch d.kind {
	case ToolExitPlanMode:
		switch {
		case lower == "approve plan" || isAllowResponse(lower):
			result = PermissionResult{Behavior: "allow", UpdatedInput: pending.ToolInput}
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("plan_approved")))
		case lower == "keep planning" || isDenyResponse(lower):
			result = PermissionResult{Behavior: "deny", Message: "The user wants to keep planning. Stay in plan mode and revise the plan."}
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("plan_kept")))
		default:
			result = PermissionResult{Behavior: "deny", Message: "The user wants changes to the plan before approving it: " + strings.TrimSpace(content)}
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("plan_feedback_sent")))
		}
	default:
		if lower == "cancel" || lower == "取消" {
			result = PermissionResult{Behavior: "deny", Message: "The user declined to answer."}
			break
		}
		q := d.questions[d.index]
		d.answers[q.Question] = q.answerFor(content)
		d.index++
		if d.index < len(d.questions) {
			return PermissionResult{}, false
		}
		input := make(map[string]any, len(pending.ToolInput)+1)
		for k, v := range pending.ToolInput {
			input[k] = v
		}
		answers := make(map[string]any, len(d.answers))
		for k, v := range d.answers {
			answers[k] = v
		}
		input["answers"] = answers
		result = PermissionResult{Behavior: "allow", UpdatedInput: input}
	}
	return result, true
}
