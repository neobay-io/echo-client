package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// A fake ACP server: this test binary re-run with ACP_FAKE_SERVER=1. It logs
// every request in $ACP_FAKE_LOG and plays a scenario chosen by an @marker
// in the prompt. ACP_FAKE_YOLO=1 adds a "yolo" mode.
func TestHelperACPServer(t *testing.T) {
	if os.Getenv("ACP_FAKE_SERVER") != "1" {
		return
	}
	runFakeACP()
	os.Exit(0)
}

func runFakeACP() {
	logf, _ := os.OpenFile(os.Getenv("ACP_FAKE_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var outMu sync.Mutex
	send := func(v any) {
		data, _ := json.Marshal(v)
		outMu.Lock()
		_, _ = os.Stdout.Write(append(data, '\n'))
		outMu.Unlock()
	}
	update := func(u map[string]any) {
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "s1", "update": u}})
	}
	chunk := func(text string) {
		update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
	}
	replies := make(chan map[string]any, 8)
	cancelled := make(chan struct{}, 4)
	var promptMu sync.Mutex
	var running []any // ids of prompts in flight
	finish := func(id any, stop string) {
		promptMu.Lock()
		for i, r := range running {
			if r == id {
				running = append(running[:i], running[i+1:]...)
				break
			}
		}
		promptMu.Unlock()
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"stopReason": stop}})
	}
	modes := []map[string]any{{"id": "default", "name": "Default"}, {"id": "plan", "name": "Plan"}, {"id": "auto", "name": "Auto"}}
	if os.Getenv("ACP_FAKE_YOLO") == "1" {
		modes = append(modes, map[string]any{"id": "yolo", "name": "YOLO"})
	}
	setup := map[string]any{
		"sessionId": "s1",
		"modes":     map[string]any{"currentModeId": "default", "availableModes": modes},
		"configOptions": []any{
			map[string]any{"id": "model", "category": "model", "currentValue": "m1", "options": []any{map[string]any{"value": "m1"}, map[string]any{"value": "m2"}}},
			map[string]any{"id": "thinking", "category": "thought_level", "currentValue": "low", "options": []any{map[string]any{"value": "low"}, map[string]any{"value": "high"}}},
		},
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Bytes()
		_, _ = logf.Write(append(append([]byte(nil), line...), '\n'))
		var msg map[string]any
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		method, _ := msg["method"].(string)
		id := msg["id"]
		params, _ := msg["params"].(map[string]any)
		if method == "" {
			replies <- msg
			continue
		}
		ok := func(result any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}) }
		switch method {
		case "initialize":
			ok(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "sessionCapabilities": map[string]any{"resume": map[string]any{}}}})
		case "session/new":
			ok(setup)
		case "session/resume":
			if params["sessionId"] == "missing" {
				send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32002, "message": "Session not found: missing"}})
				continue
			}
			ok(setup)
		case "session/set_config_option":
			if params["configId"] == "model" && params["value"] == "m2" {
				// m2 has a deeper thinking level.
				ok(map[string]any{"configOptions": []any{
					map[string]any{"id": "thinking", "category": "thought_level", "currentValue": "low", "options": []any{map[string]any{"value": "low"}, map[string]any{"value": "high"}, map[string]any{"value": "max"}}},
				}})
				continue
			}
			ok(map[string]any{})
		case "session/set_mode", "session/set_model":
			ok(map[string]any{})
		case "session/cancel":
			cancelled <- struct{}{}
		case "session/prompt":
			blocks, _ := params["prompt"].([]any)
			text := ""
			if len(blocks) > 0 {
				text, _ = blocks[0].(map[string]any)["text"].(string)
			}
			if strings.Contains(text, "@reject") {
				// Refused without touching the running prompt.
				send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32602, "message": "unsupported content"}})
				continue
			}
			if text == "/compact" {
				// Like Kimi: the command returns at once, compaction reports later.
				ok(map[string]any{"stopReason": "end_turn"})
				go func() {
					time.Sleep(300 * time.Millisecond)
					chunk("Compaction completed.")
				}()
				continue
			}
			promptMu.Lock()
			earlier := append([]any(nil), running...)
			running = append(running, id)
			promptMu.Unlock()
			// Like agy: a new prompt ends the one running.
			for _, e := range earlier {
				finish(e, "end_turn")
			}
			go playACP(id, text, chunk, update, send, finish, replies, cancelled)
		default:
			send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "unknown " + method}})
		}
	}
}

func playACP(id any, prompt string, chunk func(string), update func(map[string]any), send func(any), finish func(any, string), replies chan map[string]any, cancelled chan struct{}) {
	switch {
	case strings.Contains(prompt, "@tool"):
		chunk("Looking.")
		update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "ls", "kind": "execute", "rawInput": map[string]any{"command": "ls"}})
		update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "title": "ls", "status": "completed",
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "a.txt"}}}})
		chunk("Done.")
		finish(id, "end_turn")
	case strings.Contains(prompt, "@perm"):
		send(map[string]any{"jsonrpc": "2.0", "id": 700, "method": "session/request_permission", "params": map[string]any{
			"sessionId": "s1", "toolCall": map[string]any{"toolCallId": "t2", "title": "rm x", "kind": "execute", "rawInput": map[string]any{"command": "rm x"}},
			"options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}},
		}})
		reply := <-replies
		chunk("outcome=" + string(mustJSON(reply["result"])))
		finish(id, "end_turn")
	case strings.Contains(prompt, "@ask"):
		send(map[string]any{"jsonrpc": "2.0", "id": 701, "method": "elicitation/create", "params": map[string]any{
			"mode": "form", "message": "Pick a database",
			"requestedSchema": map[string]any{"type": "object", "required": []any{"q0"}, "properties": map[string]any{
				"q0": map[string]any{"type": "string", "title": "Which DB?", "oneOf": []any{map[string]any{"const": "pg", "title": "Postgres"}, map[string]any{"const": "sq", "title": "SQLite"}}},
			}},
		}})
		reply := <-replies
		chunk("answer=" + string(mustJSON(reply["result"])))
		finish(id, "end_turn")
	case strings.Contains(prompt, "@slow"):
		select {
		case <-cancelled:
			chunk("partial")
			finish(id, "cancelled")
		case <-time.After(20 * time.Second):
			finish(id, "end_turn")
		}
	case strings.Contains(prompt, "@usage"):
		update(map[string]any{"sessionUpdate": "usage_update", "used": 500, "size": 1000})
		chunk("ok")
		finish(id, "end_turn")
	case strings.Contains(prompt, "@exit"):
		chunk("bye")
		os.Exit(3)
	case strings.Contains(prompt, "@openend"):
		// Asks, then ends the turn without waiting for the answer.
		send(map[string]any{"jsonrpc": "2.0", "id": 702, "method": "session/request_permission", "params": map[string]any{
			"sessionId": "s1", "toolCall": map[string]any{"toolCallId": "t3", "title": "write", "kind": "edit"},
			"options": []any{map[string]any{"optionId": "yes", "kind": "allow_once"}, map[string]any{"optionId": "no", "kind": "reject_once"}},
		}})
		time.Sleep(200 * time.Millisecond)
		finish(id, "end_turn")
	case strings.Contains(prompt, "@hold"):
		time.Sleep(300 * time.Millisecond) // a steered prompt ends this one first
		chunk("held")
		finish(id, "end_turn")
	default:
		chunk("Hello ")
		chunk("world")
		finish(id, "end_turn")
	}
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

type fakeLog struct{ path string }

func (f fakeLog) requests(method string) []map[string]any {
	data, _ := os.ReadFile(f.path)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["method"] == method {
			p, _ := m["params"].(map[string]any)
			out = append(out, p)
		}
	}
	return out
}

func startFake(t *testing.T, cfg Config, yolo bool) (*Session, fakeLog) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test binary as agent")
	}
	log := filepath.Join(t.TempDir(), "acp.log")
	env := append(os.Environ(), "ACP_FAKE_SERVER=1", "ACP_FAKE_LOG="+log)
	if yolo {
		env = append(env, "ACP_FAKE_YOLO=1")
	}
	cfg.Agent = "kimi"
	cfg.Command = os.Args[0]
	cfg.Args = []string{"-test.run=^TestHelperACPServer$"}
	cfg.Env = env
	cfg.WorkDir = t.TempDir()
	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Logf("start: %v", err)
		return nil, fakeLog{log}
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, fakeLog{log}
}

func nextEvent(t *testing.T, s *Session) core.Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
	}
	return core.Event{}
}

// collect reads events until the turn's result (or error).
func collect(t *testing.T, s *Session, onRequest func(core.Event)) []core.Event {
	t.Helper()
	var evs []core.Event
	for {
		ev := nextEvent(t, s)
		evs = append(evs, ev)
		if ev.Type == core.EventPermissionRequest && onRequest != nil {
			onRequest(ev)
		}
		if ev.Type == core.EventResult || ev.Type == core.EventError {
			return evs
		}
	}
}

func describe(evs []core.Event) string {
	var parts []string
	for _, ev := range evs {
		parts = append(parts, fmt.Sprintf("%s:%s%s", ev.Type, ev.ToolName, ev.Content))
	}
	return strings.Join(parts, " | ")
}

func TestTurnsAndTools(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	if s == nil {
		t.Fatal("start failed")
	}
	if err := s.Send("hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := describe(collect(t, s, nil))
	if !strings.Contains(got, "text:Hello world") || !strings.HasSuffix(got, "result:") {
		t.Fatalf("events = %s", got)
	}
	if err := s.Send("please @tool", nil, nil); err != nil {
		t.Fatal(err)
	}
	got = describe(collect(t, s, nil))
	for _, want := range []string{"thinking:Looking.", "tool_use:ls", "tool_result:lsa.txt", "text:Done.", "result:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("events = %s; missing %q", got, want)
		}
	}
}

// Settings are applied at start and between turns; a mode the agent does
// not have fails the start.
func TestSettingsApplied(t *testing.T) {
	s, log := startFake(t, Config{Mode: core.PermissionModePlan, Model: "m2", Effort: "high"}, false)
	if s == nil {
		t.Fatal("start failed")
	}
	if m := log.requests("session/set_mode"); len(m) != 1 || m[0]["modeId"] != "plan" {
		t.Fatalf("set_mode = %v", m)
	}
	opts := log.requests("session/set_config_option")
	if len(opts) != 2 || opts[0]["configId"] != "model" || opts[0]["value"] != "m2" || opts[1]["configId"] != "thinking" || opts[1]["value"] != "high" {
		t.Fatalf("set_config_option = %v", opts)
	}
	if err := s.Apply(context.Background(), "", "", core.PermissionModeAuto); err != nil {
		t.Fatal(err)
	}
	if m := log.requests("session/set_mode"); len(m) != 2 || m[1]["modeId"] != "auto" {
		t.Fatalf("live set_mode = %v", m)
	}
	if err := s.Apply(context.Background(), "nope", "", ""); err == nil {
		t.Fatal("unknown model accepted")
	}
	if bad, _ := startFake(t, Config{Mode: core.PermissionModeAcceptEdits}, false); bad != nil {
		t.Fatal("started without a mode for acceptEdits")
	}
}

func TestPermissionRequest(t *testing.T) {
	s, _ := startFake(t, Config{}, true)
	if err := s.Send("@perm", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := describe(collect(t, s, func(ev core.Event) {
		if ev.ToolInput != "rm x" {
			t.Errorf("tool input = %q", ev.ToolInput)
		}
		if err := s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "deny"}); err != nil {
			t.Error(err)
		}
	}))
	if !strings.Contains(got, `"optionId":"no"`) {
		t.Fatalf("events = %s", got)
	}
}

// Bypass on an agent without a bypass mode: the most permissive mode, and
// the session approves the rest itself.
func TestBypassWithoutBypassMode(t *testing.T) {
	s, log := startFake(t, Config{Mode: core.PermissionModeBypass}, false)
	if m := log.requests("session/set_mode"); len(m) != 1 || m[0]["modeId"] != "auto" {
		t.Fatalf("set_mode = %v", m)
	}
	if err := s.Send("@perm", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := describe(collect(t, s, func(core.Event) { t.Error("asked in bypass") }))
	if !strings.Contains(got, `"optionId":"yes"`) {
		t.Fatalf("events = %s", got)
	}
	// Leaving bypass stops the approvals.
	if err := s.Apply(context.Background(), "", "", core.PermissionModeDefault); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("@perm", nil, nil); err != nil {
		t.Fatal(err)
	}
	asked := false
	collect(t, s, func(ev core.Event) {
		asked = true
		_ = s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "allow"})
	})
	if !asked {
		t.Fatal("default mode did not ask")
	}
	// With a native yolo mode, that mode is used.
	y, ylog := startFake(t, Config{Mode: core.PermissionModeBypass}, true)
	if y == nil {
		t.Fatal("start failed")
	}
	if m := ylog.requests("session/set_mode"); len(m) != 1 || m[0]["modeId"] != "yolo" {
		t.Fatalf("set_mode = %v", m)
	}
}

func TestQuestionForm(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	if err := s.Send("@ask", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := describe(collect(t, s, func(ev core.Event) {
		if ev.ToolName != core.ToolAskUserQuestion {
			t.Errorf("tool = %q", ev.ToolName)
		}
		qs, _ := ev.ToolInputRaw["questions"].([]any)
		q, _ := qs[0].(map[string]any)
		if q["question"] != "Which DB?" || len(q["options"].([]any)) != 2 {
			t.Errorf("questions = %v", qs)
		}
		_ = s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "allow", UpdatedInput: map[string]any{"answers": map[string]any{"Which DB?": "SQLite"}}})
	}))
	if !strings.Contains(got, `"content":{"q0":"sq"}`) {
		t.Fatalf("events = %s", got)
	}
}

func TestInterruptKeepsSession(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	if err := s.Send("@slow", nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, s, nil)
	if last := evs[len(evs)-1]; last.Type != core.EventResult || !last.Interrupted {
		t.Fatalf("events = %s", describe(evs))
	}
	if err := s.Send("again", nil, nil); err != nil || !s.Alive() {
		t.Fatalf("session unusable after interrupt: %v", err)
	}
	collect(t, s, nil)
}

// A message steered into a turn continues it: one result for both.
func TestSteerContinuesTheTurn(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	steering := SteeringSession{Session: s}
	if err := s.Send("@hold", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := steering.Steer("more", nil, nil); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, s, nil)
	results := 0
	for _, ev := range evs {
		if ev.Type == core.EventResult {
			results++
		}
	}
	if results != 1 || !strings.Contains(describe(evs), "Hello world") {
		t.Fatalf("events = %s", describe(evs))
	}
	select {
	case ev := <-s.Events():
		t.Fatalf("extra event after the turn: %v", ev)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestResumeMissingSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test binary as agent")
	}
	log := filepath.Join(t.TempDir(), "acp.log")
	_, err := Start(context.Background(), Config{Agent: "kimi", Command: os.Args[0], Args: []string{"-test.run=^TestHelperACPServer$"},
		Env: append(os.Environ(), "ACP_FAKE_SERVER=1", "ACP_FAKE_LOG="+log), WorkDir: t.TempDir(), ResumeID: "missing"})
	if !errors.Is(err, core.ErrAgentSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestProcessExitEndsTheTurn(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	if err := s.Send("@exit", nil, nil); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, s, nil)
	if evs[len(evs)-1].Type != core.EventError {
		t.Fatalf("events = %s", describe(evs))
	}
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("events not closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("events not closed")
	}
	if s.Alive() {
		t.Fatal("session alive after exit")
	}
}

func TestContextUsage(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	if err := s.Send("@usage", nil, nil); err != nil {
		t.Fatal(err)
	}
	collect(t, s, nil)
	r, err := s.ContextUsage(context.Background())
	if err != nil || r.UsedTokens != 500 || r.MaxTokens != 1000 || r.Percent != 50 {
		t.Fatalf("usage = %+v %v", r, err)
	}
}

// Review: /stop right after Send never overtakes the prompt.
func TestStopRightAfterSend(t *testing.T) {
	s, log := startFake(t, Config{}, false)
	if err := s.Send("@slow", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := collect(t, s, nil)
	if last := evs[len(evs)-1]; last.Type != core.EventResult || !last.Interrupted {
		t.Fatalf("events = %s", describe(evs))
	}
	data, _ := os.ReadFile(log.path)
	if p, c := strings.Index(string(data), `"session/prompt"`), strings.Index(string(data), `"session/cancel"`); p < 0 || c < 0 || p > c {
		t.Fatalf("prompt at %d, cancel at %d", p, c)
	}
}

// Review: a card still open when its turn ends is retired.
func TestOpenRequestRetiredAtTurnEnd(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	if err := s.Send("@openend", nil, nil); err != nil {
		t.Fatal(err)
	}
	var reqID string
	collect(t, s, func(ev core.Event) { reqID = ev.RequestID })
	select {
	case <-s.RequestDone(reqID):
	case <-time.After(2 * time.Second):
		t.Fatal("request still open after the turn")
	}
	if err := s.RespondPermission(reqID, core.PermissionResult{Behavior: "allow"}); err == nil {
		t.Fatal("answered a retired request")
	}
}

// Review: a steered message the agent refuses is reported.
func TestRefusedSteerReported(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	steering := SteeringSession{Session: s}
	if err := s.Send("@hold", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := steering.Steer("@reject", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := describe(collect(t, s, nil))
	if !strings.Contains(got, "not taken") || !strings.HasSuffix(got, "result:") {
		t.Fatalf("events = %s", got)
	}
}

// Review: /compact goes as the command itself and waits for the CLI to
// report the compaction.
func TestNativeCompact(t *testing.T) {
	s, log := startFake(t, Config{}, false)
	var nc core.NativeCompactor = s
	if err := nc.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := describe(collect(t, s, nil))
	if !strings.Contains(got, "Compaction completed.") {
		t.Fatalf("events = %s", got)
	}
	prompts := log.requests("session/prompt")
	blocks, _ := prompts[0]["prompt"].([]any)
	if text, _ := blocks[0].(map[string]any)["text"].(string); text != "/compact" {
		t.Fatalf("compact prompt = %q", text)
	}
}

// Review: the thinking levels of the model just chosen count.
func TestEffortOfNewModel(t *testing.T) {
	s, _ := startFake(t, Config{}, false)
	if err := s.Apply(context.Background(), "m2", "max", ""); err != nil {
		t.Fatal(err)
	}
}

// Review: multi-select labels may contain commas; an answer that fits no
// option declines the form instead of leaving it incomplete.
func TestFormAnswers(t *testing.T) {
	e := &elicitation{questions: []elicitQuestion{{key: "q0", question: "Cities?", multi: true, options: []elicitOption{
		{value: "par", label: "Paris, France"}, {value: "ber", label: "Berlin"}}}}}
	res := e.response(true, map[string]any{"answers": map[string]any{"Cities?": "Paris, France, Berlin"}}).(map[string]any)
	if content, _ := res["content"].(map[string]any); fmt.Sprint(content["q0"]) != "[par ber]" {
		t.Fatalf("response = %v", res)
	}
	single := &elicitation{questions: []elicitQuestion{{key: "q0", question: "DB?", options: []elicitOption{{value: "pg", label: "Postgres"}}}}}
	if res := single.response(true, map[string]any{"answers": map[string]any{"DB?": "MySQL"}}).(map[string]any); res["action"] != "decline" {
		t.Fatalf("response = %v", res)
	}
}

// Kimi review: a user's allow is never a standing grant; capabilities set to
// false are not used.
func TestAllowOnceOnlyAndFalseCapabilities(t *testing.T) {
	onlyAlways := []permissionOption{{OptionID: "always", Kind: "allow_always"}, {OptionID: "no", Kind: "reject_once"}}
	if got := pickOption(onlyAlways, true); got != "" {
		t.Fatalf("user allow picked %q", got)
	}
	if got := pickOptionFrom(onlyAlways, true, true); got != "always" {
		t.Fatalf("bypass picked %q", got)
	}
	for raw, want := range map[string]bool{"": false, "null": false, "false": false, "{}": true, "true": true} {
		if supported(json.RawMessage(raw)) != want {
			t.Errorf("supported(%q) != %v", raw, want)
		}
	}
}
