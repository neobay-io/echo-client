package acp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// Questions (ACP elicitation/create, form mode) become the chat's question
// cards (the AskUserQuestion flow of P3); the answers go back as the form's
// content.

type elicitation struct {
	questions []elicitQuestion
}

type elicitQuestion struct {
	key      string // property in the form
	question string
	multi    bool
	options  []elicitOption
	free     bool // free text (no options)
}

type elicitOption struct {
	value, label, desc string
}

type elicitProperty struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Enum        []string
	OneOf       []struct {
		Const       string `json:"const"`
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"oneOf"`
	Items *struct {
		Enum  []string `json:"enum"`
		AnyOf []struct {
			Const       string `json:"const"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"anyOf"`
	} `json:"items"`
}

func parseElicitation(params json.RawMessage) (*elicitation, string, error) {
	var p struct {
		Mode    string `json:"mode"`
		Message string `json:"message"`
		Schema  struct {
			Properties map[string]elicitProperty `json:"properties"`
			Required   []string                  `json:"required"`
		} `json:"requestedSchema"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, "", err
	}
	if p.Mode != "" && p.Mode != "form" {
		return nil, "", fmt.Errorf("unsupported elicitation mode %q", p.Mode)
	}
	keys := make([]string, 0, len(p.Schema.Properties))
	seen := map[string]bool{}
	for _, k := range p.Schema.Required {
		if _, ok := p.Schema.Properties[k]; ok && !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range p.Schema.Properties {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	e := &elicitation{}
	for _, k := range keys {
		prop := p.Schema.Properties[k]
		q := elicitQuestion{key: k, question: strings.TrimSpace(prop.Title)}
		if q.question == "" {
			q.question = strings.TrimSpace(prop.Description)
		}
		if q.question == "" {
			q.question = k
		}
		switch prop.Type {
		case "string":
			for _, o := range prop.OneOf {
				q.options = append(q.options, elicitOption{value: o.Const, label: firstNonEmpty(o.Title, o.Const), desc: o.Description})
			}
			for _, v := range prop.Enum {
				q.options = append(q.options, elicitOption{value: v, label: v})
			}
			q.free = len(q.options) == 0
		case "array":
			q.multi = true
			if prop.Items != nil {
				for _, o := range prop.Items.AnyOf {
					q.options = append(q.options, elicitOption{value: o.Const, label: firstNonEmpty(o.Title, o.Const), desc: o.Description})
				}
				for _, v := range prop.Items.Enum {
					q.options = append(q.options, elicitOption{value: v, label: v})
				}
			}
			if len(q.options) == 0 {
				return nil, "", fmt.Errorf("question %q has no options", k)
			}
		default:
			// Numbers and booleans have no place on a chat card.
			return nil, "", fmt.Errorf("question %q has unsupported type %q", k, prop.Type)
		}
		e.questions = append(e.questions, q)
	}
	if len(e.questions) == 0 {
		return nil, "", fmt.Errorf("form has no questions")
	}
	// Questions with the same text would share one answer: tell them apart.
	count := map[string]int{}
	for i := range e.questions {
		count[e.questions[i].question]++
		if n := count[e.questions[i].question]; n > 1 {
			e.questions[i].question = fmt.Sprintf("%s (%d)", e.questions[i].question, n)
		}
	}
	return e, p.Message, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// askInput is the AskUserQuestion tool input the chat's question cards read.
func (e *elicitation) askInput(message string) map[string]any {
	qs := make([]any, 0, len(e.questions))
	for _, q := range e.questions {
		opts := make([]any, 0, len(q.options))
		for _, o := range q.options {
			opts = append(opts, map[string]any{"label": o.label, "description": o.desc})
		}
		header := ""
		if len(e.questions) == 1 {
			header = truncate(message, 60)
		}
		qs = append(qs, map[string]any{"question": q.question, "header": header, "options": opts, "multiSelect": q.multi})
	}
	return map[string]any{"questions": qs}
}

// response builds the form answer from the card's answers
// (updatedInput.answers: question -> label(s), comma separated for multi).
func (e *elicitation) response(allow bool, updated map[string]any) any {
	if !allow {
		return map[string]any{"action": "decline"}
	}
	answers, _ := updated["answers"].(map[string]any)
	content := map[string]any{}
	for _, q := range e.questions {
		raw, _ := answers[q.question].(string)
		raw = strings.TrimSpace(raw)
		switch {
		case raw == "":
		case q.multi:
			// The cards join picked labels with ", "; labels may contain
			// commas themselves, so match whole labels.
			var values []string
			joined := ", " + raw + ", "
			for _, o := range q.options {
				if strings.Contains(joined, ", "+o.label+", ") || strings.EqualFold(raw, o.label) {
					values = append(values, o.value)
				}
			}
			if len(values) > 0 {
				content[q.key] = values
			}
		default:
			if v, ok := q.valueOf(raw); ok {
				content[q.key] = v
			} else if q.free {
				content[q.key] = raw
			}
		}
		if _, ok := content[q.key]; !ok {
			// An answer that fits none of the options leaves the form
			// incomplete: decline rather than send it without the answer.
			return map[string]any{"action": "decline"}
		}
	}
	return map[string]any{"action": "accept", "content": content}
}

// valueOf maps a label (or value) the user picked to the option value.
// Free text that matches no option is not accepted for a choice question.
func (q elicitQuestion) valueOf(answer string) (string, bool) {
	for _, o := range q.options {
		if strings.EqualFold(o.label, answer) || o.value == answer {
			return o.value, true
		}
	}
	return "", false
}

func (s *Session) handleElicitation(id json.RawMessage, params json.RawMessage) {
	e, message, err := parseElicitation(params)
	if err != nil {
		// Kimi then falls back to asking through a permission request.
		s.rpc.respondError(id, -32602, err.Error())
		return
	}
	key := strings.TrimSpace(string(id))
	req := &pendingRequest{id: id, elicitation: e, done: make(chan struct{})}
	s.mu.Lock()
	s.requests[key] = req
	s.mu.Unlock()
	s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: key, ToolName: core.ToolAskUserQuestion,
		ToolInput: e.questions[0].question, ToolInputRaw: e.askInput(message)})
}
