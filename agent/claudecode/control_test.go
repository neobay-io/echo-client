package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// fakeControlCLI stands in for the Claude process: it reads what the session
// writes to stdin, records it, and answers control requests with respond.
type fakeControlCLI struct {
	mu       sync.Mutex
	requests []map[string]any // control requests' "request" objects
	messages []map[string]any // everything else written to stdin
}

func newControlSession(t *testing.T, respond func(req map[string]any) (map[string]any, string)) (*claudeSession, *fakeControlCLI) {
	t.Helper()
	r, w := io.Pipe()
	cs := &claudeSession{
		stdin:  w,
		events: make(chan core.Event, 8),
		ctx:    context.Background(),
		done:   make(chan struct{}),
	}
	cs.alive.Store(true)
	fake := &fakeControlCLI{}
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var msg map[string]any
			if json.Unmarshal(sc.Bytes(), &msg) != nil {
				continue
			}
			if msg["type"] != "control_request" {
				fake.mu.Lock()
				fake.messages = append(fake.messages, msg)
				fake.mu.Unlock()
				continue
			}
			req, _ := msg["request"].(map[string]any)
			fake.mu.Lock()
			fake.requests = append(fake.requests, req)
			fake.mu.Unlock()
			resp, errMsg := respond(req)
			out := map[string]any{"subtype": "success", "request_id": msg["request_id"], "response": resp}
			if errMsg != "" {
				out = map[string]any{"subtype": "error", "request_id": msg["request_id"], "error": errMsg}
			}
			cs.handleControlResponse(map[string]any{"type": "control_response", "response": out})
		}
	}()
	t.Cleanup(func() { _ = w.Close() })
	return cs, fake
}

func (f *fakeControlCLI) subtypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		s, _ := r["subtype"].(string)
		out = append(out, s)
	}
	return out
}

func okResponder(req map[string]any) (map[string]any, string) { return map[string]any{}, "" }

func TestControlRequestRoundTripAndErrors(t *testing.T) {
	cs, _ := newControlSession(t, func(req map[string]any) (map[string]any, string) {
		if req["subtype"] == "set_model" && req["model"] == "nope" {
			return nil, "unknown model"
		}
		return map[string]any{"model": "claude-opus-5-5"}, ""
	})
	resp, err := cs.controlRequest(context.Background(), map[string]any{"subtype": "set_model", "model": "opus"})
	if err != nil || resp["model"] != "claude-opus-5-5" {
		t.Fatalf("resp = %v, %v", resp, err)
	}
	if _, err := cs.controlRequest(context.Background(), map[string]any{"subtype": "set_model", "model": "nope"}); err == nil || err.Error() != "unknown model" {
		t.Fatalf("error response = %v", err)
	}
	// A process that exits fails pending requests at once.
	silent, _ := newControlSession(t, func(map[string]any) (map[string]any, string) { select {} })
	go func() { time.Sleep(50 * time.Millisecond); close(silent.done) }()
	if _, err := silent.controlRequest(context.Background(), map[string]any{"subtype": "interrupt"}); !errors.Is(err, errSessionClosed) {
		t.Fatalf("closed session = %v", err)
	}
}

// T3.1: /model, /effort and /mode switch the running process; only changes
// are sent, and bypass mode follows the live mode.
func TestApplyLiveSettingsSendsOnlyChanges(t *testing.T) {
	cs, fake := newControlSession(t, okResponder)
	a := &Agent{}
	opts := core.AgentSessionOptions{Model: "opus", ReasoningLevel: "high", Mode: "plan"}
	if err := a.ApplyLiveSettings(context.Background(), cs, opts); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.subtypes(), ","); got != "set_model,apply_flag_settings,set_permission_mode" {
		t.Fatalf("requests = %s", got)
	}
	if err := a.ApplyLiveSettings(context.Background(), cs, opts); err != nil || len(fake.subtypes()) != 3 {
		t.Fatalf("unchanged settings sent requests: %v, %v", fake.subtypes(), err)
	}
	if err := a.ApplyLiveSettings(context.Background(), cs, core.AgentSessionOptions{Model: "opus", ReasoningLevel: "high", Mode: "bypassPermissions"}); err != nil {
		t.Fatal(err)
	}
	if !cs.autoApprove.Load() {
		t.Fatal("bypass mode did not enable auto-approval")
	}
	// Clearing the effort cannot be done live: the engine restarts instead.
	if err := a.ApplyLiveSettings(context.Background(), cs, core.AgentSessionOptions{Model: "opus", Mode: "bypassPermissions"}); err == nil {
		t.Fatal("clearing the effort reported a live switch")
	}
	// A refused switch is an error (the engine then restarts the process).
	refusing, _ := newControlSession(t, func(map[string]any) (map[string]any, string) { return nil, "not allowed" })
	if err := a.ApplyLiveSettings(context.Background(), refusing, core.AgentSessionOptions{Mode: "plan"}); err == nil {
		t.Fatal("refused switch reported success")
	}
}

// T3.3: a steered message carries priority "next".
func TestSteerWritesPriorityNext(t *testing.T) {
	cs, fake := newControlSession(t, okResponder)
	cs.workDir = t.TempDir()
	if err := cs.Steer("also check the tests", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		n := len(fake.messages)
		var msg map[string]any
		if n > 0 {
			msg = fake.messages[0]
		}
		fake.mu.Unlock()
		if n > 0 {
			inner, _ := msg["message"].(map[string]any)
			if msg["type"] != "user" || msg["priority"] != "next" || inner["content"] != "also check the tests" {
				t.Fatalf("steer message = %v", msg)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// T3.4: after an interrupt the turn's result is an interruption, not an
// error, so the process is kept.
func TestInterruptedResultIsNotAnError(t *testing.T) {
	cs, fake := newControlSession(t, okResponder)
	if err := cs.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fake.subtypes(); len(got) != 1 || got[0] != "interrupt" {
		t.Fatalf("requests = %v", got)
	}
	cs.handleResult(map[string]any{"subtype": "error_during_execution", "is_error": true, "result": ""})
	ev := <-cs.events
	if ev.Type != core.EventResult || !ev.Interrupted || ev.Error != nil {
		t.Fatalf("event = %#v", ev)
	}
	// The next turn's errors are errors again.
	cs.handleResult(map[string]any{"subtype": "error_during_execution", "is_error": true, "result": "boom"})
	if ev := <-cs.events; ev.Type != core.EventError {
		t.Fatalf("next event = %#v", ev)
	}
	// An interrupt that lands after its turn ended does not mark the next
	// turn, which starts with a new message.
	if err := cs.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	cs.workDir = t.TempDir()
	if err := cs.Send("next question", nil, nil); err != nil {
		t.Fatal(err)
	}
	cs.handleResult(map[string]any{"subtype": "error_during_execution", "is_error": true, "result": "real failure"})
	if ev := <-cs.events; ev.Type != core.EventError || ev.Interrupted {
		t.Fatalf("late interrupt marked the next turn: %#v", ev)
	}
}

// T3.5: bypass mode approves tools, but questions and plans reach the user.
func TestBypassStillAsksQuestionsAndPlans(t *testing.T) {
	cs, fake := newControlSession(t, okResponder)
	cs.autoApprove.Store(true)
	cs.handleControlRequest(map[string]any{"request_id": "r1", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"},
	}})
	cs.handleControlRequest(map[string]any{"request_id": "r2", "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": core.ToolAskUserQuestion, "input": map[string]any{"questions": []any{}},
	}})
	ev := <-cs.events
	if ev.Type != core.EventPermissionRequest || ev.ToolName != core.ToolAskUserQuestion || ev.RequestID != "r2" {
		t.Fatalf("event = %#v", ev)
	}
	_ = fake
	if len(cs.events) != 0 {
		t.Fatal("Bash was not auto-approved")
	}
}

// T3.6: get_usage, get_context_usage and list_models parsing.
func TestParseUsageContextAndModels(t *testing.T) {
	usage := parseClaudeUsage(map[string]any{
		"session":           map[string]any{"total_cost_usd": 0.42},
		"subscription_type": "max",
		"rate_limits": map[string]any{
			"five_hour": map[string]any{"utilization": float64(11), "resets_at": "2026-11-18T05:50:00.149117+00:00"},
			"seven_day": map[string]any{"utilization": float64(25), "resets_at": "2026-11-21T03:00:00+00:00"},
		},
	})
	if usage.Plan != "max" || !usage.HasCost || usage.SessionCostUSD != 0.42 || len(usage.Lines) != 2 ||
		usage.Lines[0].UsedPercent != 11 || usage.Lines[0].ResetsAt.IsZero() {
		t.Fatalf("usage = %#v", usage)
	}

	cs, _ := newControlSession(t, func(map[string]any) (map[string]any, string) {
		return map[string]any{"totalTokens": float64(28561), "maxTokens": float64(1000000), "percentage": float64(3),
			"categories": []any{
				map[string]any{"name": "System prompt", "tokens": float64(3016), "kind": "used"},
				map[string]any{"name": "System tools (deferred)", "tokens": float64(25368), "kind": "deferred"},
			}}, ""
	})
	ctxReport, err := cs.ContextUsage(context.Background())
	if err != nil || ctxReport.UsedTokens != 28561 || ctxReport.Percent != 3 || len(ctxReport.Categories) != 1 {
		t.Fatalf("context = %#v, %v", ctxReport, err)
	}

	models := parseClaudeModels(map[string]any{"models": []any{
		map[string]any{"value": "default", "displayName": "Default (recommended)", "supportedEffortLevels": []any{"low", "high", "xhigh", "max"}},
		map[string]any{"value": "fable", "displayName": "Fable 5.2", "description": "Most capable"},
		map[string]any{"value": ""},
	}})
	if len(models) != 2 || models[0].Efforts[2] != "xhigh" || models[1].Display != "Fable 5.2" {
		t.Fatalf("models = %#v", models)
	}
	a := &Agent{listModelsFn: func(context.Context) ([]claudeModel, error) { return models, nil }}
	// The first call never waits for the CLI; the list arrives in the
	// background.
	_ = a.EffortsForModel(context.Background(), "")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := a.EffortsForModel(context.Background(), ""); strings.Join(got, ",") == "low,high,xhigh,max" {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("efforts for default = %v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	opts := a.AvailableModels(context.Background())
	if len(opts) != 2 || opts[1].Name != "fable" || opts[1].Desc != "Fable 5.2 · Most capable" {
		t.Fatalf("model options = %#v", opts)
	}
}

// P3 review 3: auto-approval stops before the CLI is asked to leave bypass,
// and stays off if the switch fails.
func TestLeavingBypassStopsAutoApprovalFirst(t *testing.T) {
	var sawAuto []bool
	var cs *claudeSession
	cs, _ = newControlSession(t, func(req map[string]any) (map[string]any, string) {
		sawAuto = append(sawAuto, cs.autoApprove.Load())
		return nil, "busy"
	})
	cs.mode = "bypassPermissions"
	cs.autoApprove.Store(true)
	if err := cs.applyLive(context.Background(), "", "", "default"); err == nil {
		t.Fatal("refused switch reported success")
	}
	if len(sawAuto) != 1 || sawAuto[0] || cs.autoApprove.Load() {
		t.Fatalf("auto-approve during switch = %v, after = %v", sawAuto, cs.autoApprove.Load())
	}
}

// P3 review 10: effort levels from the model list survive normalization.
func TestNormalizeReasoningLevelKeepsCatalogLevels(t *testing.T) {
	for in, want := range map[string]string{"ultracode": "ultracode", "XHigh": "xhigh", "auto": "", "bad level!": "", "": ""} {
		if got := normalizeReasoningLevel(in); got != want {
			t.Errorf("normalizeReasoningLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
