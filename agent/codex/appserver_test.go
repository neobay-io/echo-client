package codex

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

// A fake `codex app-server`: this test binary re-run with
// ECHO_FAKE_CODEX_APPSERVER=1 behind a `codex` shim on PATH. It records every
// request it gets (one JSON line each) in $ECHO_FAKE_CODEX_LOG and plays a
// scenario chosen by an @marker in the turn's prompt (the prompt also carries
// echo-client's interaction hint, so plain words could match by accident).
func TestHelperCodexAppServer(t *testing.T) {
	if os.Getenv("ECHO_FAKE_CODEX_APPSERVER") != "1" {
		return
	}
	runFakeAppServer()
	os.Exit(0)
}

func runFakeAppServer() {
	logf, _ := os.OpenFile(os.Getenv("ECHO_FAKE_CODEX_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var outMu sync.Mutex
	send := func(v any) {
		data, _ := json.Marshal(v)
		outMu.Lock()
		_, _ = os.Stdout.Write(append(data, '\n'))
		outMu.Unlock()
	}
	notify := func(method string, params any) { send(map[string]any{"method": method, "params": params}) }
	replies := make(chan map[string]any, 8) // responses to our server requests
	turnInterrupted := make(chan struct{}, 1)
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
		switch method {
		case "initialize":
			send(map[string]any{"id": id, "result": map[string]any{}})
		case "config/read":
			cfg := map[string]any{}
			if raw := os.Getenv("ECHO_FAKE_CODEX_CONFIG"); raw != "" {
				_ = json.Unmarshal([]byte(raw), &cfg)
			}
			send(map[string]any{"id": id, "result": map[string]any{"config": cfg}})
		case "thread/start", "thread/resume":
			if os.Getenv("ECHO_FAKE_CODEX_NO_THREADS") == "1" {
				send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "method not found"}})
				continue
			}
			if method == "thread/resume" && params["excludeTurns"] != true {
				send(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": "test: resume must exclude turns"}})
				continue
			}
			if params["threadId"] == "missing" {
				send(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": "thread not found: missing"}})
				continue
			}
			send(map[string]any{"id": id, "result": map[string]any{
				"thread": map[string]any{"id": "thr_1"}, "model": "gpt-test", "reasoningEffort": "medium",
				"approvalPolicy": "on-request", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "readOnly"},
			}})
		case "turn/start":
			if params["model"] == "bad" {
				send(map[string]any{"id": id, "error": map[string]any{"code": -32600, "message": "unknown model bad"}})
				continue
			}
			input, _ := params["input"].([]any)
			text := ""
			if len(input) > 0 {
				text, _ = input[0].(map[string]any)["text"].(string)
			}
			if strings.Contains(text, "@stall") {
				select {} // stop reading stdin
			}
			send(map[string]any{"id": id, "result": map[string]any{"turn": map[string]any{"id": "turn_1", "status": "inProgress"}}})
			go playTurn(text, notify, send, replies, turnInterrupted)
		case "turn/steer":
			send(map[string]any{"id": id, "result": map[string]any{}})
		case "turn/interrupt":
			send(map[string]any{"id": id, "result": map[string]any{}})
			turnInterrupted <- struct{}{}
		case "account/rateLimits/read":
			send(map[string]any{"id": id, "result": map[string]any{"rateLimits": map[string]any{
				"planType": "pro", "primary": map[string]any{"usedPercent": 9, "windowDurationMins": 10080, "resetsAt": 1795052851},
			}}})
		default:
			send(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "unknown " + method}})
		}
	}
}

func playTurn(prompt string, notify func(string, any), send func(any), replies chan map[string]any, interrupted chan struct{}) {
	turn := map[string]any{"threadId": "thr_1", "turnId": "turn_1"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{"threadId": "thr_1", "turnId": "turn_1"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	complete := func(status string, extra map[string]any) {
		t := map[string]any{"id": "turn_1", "status": status, "items": []any{}}
		for k, v := range extra {
			t[k] = v
		}
		notify("turn/completed", map[string]any{"threadId": "thr_1", "turn": t})
	}
	notify("turn/started", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "turn_1"}})
	switch {
	case strings.Contains(prompt, "@approve"):
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "Running ls.", "phase": "commentary"}}))
		notify("item/started", with(map[string]any{"item": map[string]any{"type": "commandExecution", "id": "c1", "command": "ls"}}))
		send(map[string]any{"id": 900, "method": "item/commandExecution/requestApproval", "params": with(map[string]any{"itemId": "c1", "command": "ls", "reason": "list files"})})
		reply := <-replies
		decision, _ := reply["result"].(map[string]any)["decision"].(string)
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "commandExecution", "id": "c1", "command": "ls", "aggregatedOutput": "decision=" + decision, "status": "completed"}}))
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "All done.", "phase": "final_answer"}}))
		notify("thread/tokenUsage/updated", with(map[string]any{"tokenUsage": map[string]any{
			"total": map[string]any{"totalTokens": 5000}, "last": map[string]any{"totalTokens": 3000, "inputTokens": 2900, "outputTokens": 100}, "modelContextWindow": 100000,
		}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@ask"):
		send(map[string]any{"id": 901, "method": "item/tool/requestUserInput", "params": with(map[string]any{
			"itemId": "q", "isBlocking": true, "questions": []any{map[string]any{
				"id": "db", "header": "DB", "question": "Which database?", "options": []any{
					map[string]any{"label": "Postgres", "description": "relational"}, map[string]any{"label": "SQLite", "description": "file"},
				}}},
		})})
		reply := <-replies
		data, _ := json.Marshal(reply["result"])
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "answers=" + string(data)}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@slow"):
		<-interrupted
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "partial"}}))
		complete("interrupted", nil)
	case strings.Contains(prompt, "@fail"):
		complete("failed", map[string]any{"error": map[string]any{"message": "model overloaded"}})
	case strings.Contains(prompt, "@legacy"):
		send(map[string]any{"id": 902, "method": "execCommandApproval", "params": with(map[string]any{"command": []any{"rm", "-rf", "x"}})})
		reply := <-replies
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "reply=" + string(mustJSON(reply["result"]))}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@bigperms"):
		paths := []any{}
		for i := 0; i < 80; i++ {
			paths = append(paths, fmt.Sprintf("/some/long/path/number/%03d/to/write", i))
		}
		send(map[string]any{"id": 903, "method": "item/permissions/requestApproval", "params": with(map[string]any{"itemId": "p", "cwd": "/w", "permissions": map[string]any{"fileSystem": map[string]any{"write": paths}}})})
		reply := <-replies
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "reply=" + string(mustJSON(reply["result"]))}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@perms"):
		send(map[string]any{"id": 904, "method": "item/permissions/requestApproval", "params": with(map[string]any{"itemId": "p", "cwd": "/w", "permissions": map[string]any{
			"network": map[string]any{"enabled": true}, "fileSystem": map[string]any{"write": []any{"/etc/hosts"}}}})})
		reply := <-replies
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "reply=" + string(mustJSON(reply["result"]))}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@secret"):
		send(map[string]any{"id": 905, "method": "item/tool/requestUserInput", "params": with(map[string]any{"itemId": "q", "isBlocking": true, "questions": []any{
			map[string]any{"id": "pw", "header": "Password", "question": "Your password?", "isSecret": true}}})})
		reply := <-replies
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "reply=" + string(mustJSON(reply))}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@dupq"):
		send(map[string]any{"id": 906, "method": "item/tool/requestUserInput", "params": with(map[string]any{"itemId": "q", "isBlocking": true, "questions": []any{
			map[string]any{"id": "env_a", "header": "App", "question": "Which environment?", "options": []any{map[string]any{"label": "prod", "description": ""}}},
			map[string]any{"id": "env_b", "header": "DB", "question": "Which environment?", "options": []any{map[string]any{"label": "staging", "description": ""}}},
		}})})
		reply := <-replies
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "reply=" + string(mustJSON(reply["result"]))}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@resolve"):
		send(map[string]any{"id": 907, "method": "item/tool/requestUserInput", "params": with(map[string]any{"itemId": "q", "isBlocking": false, "questions": []any{
			map[string]any{"id": "x", "header": "X", "question": "Optional?", "options": []any{map[string]any{"label": "a", "description": ""}}}}})})
		time.Sleep(200 * time.Millisecond)
		notify("serverRequest/resolved", map[string]any{"threadId": "thr_1", "requestId": 907})
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "went on without an answer"}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@stdin"):
		send(map[string]any{"id": 908, "method": "item/commandExecution/requestApproval", "params": with(map[string]any{"itemId": "c", "kind": "", "command": nil})})
		reply := <-replies
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "reply=" + string(mustJSON(reply["result"]))}}))
		complete("completed", nil)
	case strings.Contains(prompt, "@child"):
		notify("item/completed", map[string]any{"threadId": "thr_child", "turnId": "t9", "item": map[string]any{"type": "agentMessage", "text": "from a sub-agent"}})
		notify("turn/completed", map[string]any{"threadId": "thr_child", "turn": map[string]any{"id": "t9", "status": "completed"}})
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "main answer"}}))
		complete("completed", nil)
	default:
		notify("item/completed", with(map[string]any{"item": map[string]any{"type": "agentMessage", "text": "ok"}}))
		complete("completed", nil)
	}
	_ = turn
}

type fakeAppServer struct {
	log string
}

func (f *fakeAppServer) requests(t *testing.T) []map[string]any {
	t.Helper()
	data, _ := os.ReadFile(f.log)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeAppServer) last(t *testing.T, method string) map[string]any {
	t.Helper()
	reqs := f.requests(t)
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i]["method"] == method {
			p, _ := reqs[i]["params"].(map[string]any)
			return p
		}
	}
	return nil
}

func installFakeAppServer(t *testing.T) *fakeAppServer {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell shim")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "requests.jsonl")
	shim := fmt.Sprintf("#!/bin/sh\nexec %q -test.run='^TestHelperCodexAppServer$'\n", os.Args[0])
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ECHO_FAKE_CODEX_APPSERVER", "1")
	t.Setenv("ECHO_FAKE_CODEX_LOG", log)
	return &fakeAppServer{log: log}
}

func startFakeSession(t *testing.T, mode, resume string) (*Agent, *appServerSession) {
	t.Helper()
	raw, err := New(map[string]any{"work_dir": t.TempDir(), "mode": mode})
	if err != nil {
		t.Fatal(err)
	}
	a := raw.(*Agent)
	sess, err := a.StartSession(context.Background(), resume)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := sess.(*appServerSession)
	if !ok {
		t.Fatalf("session is %T, want app-server", sess)
	}
	t.Cleanup(func() { _ = s.Close() })
	return a, s
}

func nextEvent(t *testing.T, s *appServerSession) core.Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatal("events closed")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event")
		return core.Event{}
	}
}

// T4.1/T4.4: a turn with an approval; commentary is thinking, the final
// answer is text; usage and context are reported.
func TestAppServerTurnWithApproval(t *testing.T) {
	fake := installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	if s.CurrentSessionID() != "thr_1" {
		t.Fatalf("thread = %q", s.CurrentSessionID())
	}
	if err := s.Send("please @approve ls", nil, nil); err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		ev := nextEvent(t, s)
		switch ev.Type {
		case core.EventPermissionRequest:
			got = append(got, "perm:"+ev.ToolName+":"+ev.ToolInput)
			if err := s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "allow"}); err != nil {
				t.Fatal(err)
			}
		case core.EventResult:
			got = append(got, "result")
		default:
			got = append(got, fmt.Sprintf("%v:%s%s", ev.Type, ev.ToolName, ev.Content))
		}
		if ev.Type == core.EventResult {
			break
		}
	}
	joined := strings.Join(got, " | ")
	for _, want := range []string{"Running ls.", "perm:Bash:ls\n(list files)", "decision=accept", "All done.", "result"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("events = %s; missing %q", joined, want)
		}
	}
	// Default mode states the thread's own settings back (overrides persist).
	p := fake.last(t, "turn/start")
	if p["model"] != "gpt-test" || p["effort"] != "medium" || p["approvalPolicy"] != "on-request" {
		t.Fatalf("turn/start = %v", p)
	}
	ctxReport, err := s.ContextUsage(context.Background())
	if err != nil || ctxReport.UsedTokens != 3000 || ctxReport.Percent != 3 {
		t.Fatalf("context = %#v, %v", ctxReport, err)
	}
	usage, err := s.SessionUsage(context.Background())
	if err != nil || usage.Plan != "pro" || len(usage.Lines) != 1 || usage.Lines[0].UsedPercent != 9 || usage.Lines[0].Name != "7-day limit" {
		t.Fatalf("usage = %#v, %v", usage, err)
	}
}

// T4.3: settings apply per turn; modes map to approval/sandbox overrides;
// plan uses collaboration mode and is left explicitly.
func TestAppServerSettingsPerTurn(t *testing.T) {
	fake := installFakeAppServer(t)
	a, s := startFakeSession(t, "bypassPermissions", "")
	drain := func() {
		for nextEvent(t, s).Type != core.EventResult {
		}
	}
	if err := s.Send("hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	drain()
	p := fake.last(t, "turn/start")
	if sb, _ := p["sandboxPolicy"].(map[string]any); p["approvalPolicy"] != "never" || sb["type"] != "dangerFullAccess" {
		t.Fatalf("bypass turn = %v", p)
	}
	if err := a.ApplyLiveSettings(context.Background(), s, core.AgentSessionOptions{Model: "gpt-x", ReasoningLevel: "high", Mode: "plan"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	drain()
	p = fake.last(t, "turn/start")
	cm, _ := p["collaborationMode"].(map[string]any)
	if p["model"] != "gpt-x" || p["effort"] != "high" || cm["mode"] != "plan" || p["approvalPolicy"] != "on-request" {
		t.Fatalf("plan turn = %v", p)
	}
	if err := a.ApplyLiveSettings(context.Background(), s, core.AgentSessionOptions{Mode: "default"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Send("hi", nil, nil); err != nil {
		t.Fatal(err)
	}
	drain()
	p = fake.last(t, "turn/start")
	cm, _ = p["collaborationMode"].(map[string]any)
	if cm["mode"] != "default" || p["model"] != "gpt-test" {
		t.Fatalf("leaving plan = %v", p)
	}
}

// T4.2: steer and interrupt; an interrupted turn ends with Interrupted set.
func TestAppServerSteerAndInterrupt(t *testing.T) {
	fake := installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	if err := s.Send("@slow task", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		running := s.turnID != ""
		s.mu.Unlock()
		if running || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Steer("also mention bananas", nil, nil); err != nil {
		t.Fatal(err)
	}
	if p := fake.last(t, "turn/steer"); p["expectedTurnId"] != "turn_1" {
		t.Fatalf("steer = %v", p)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for {
		ev := nextEvent(t, s)
		if ev.Type == core.EventResult {
			if !ev.Interrupted {
				t.Fatalf("result = %#v", ev)
			}
			break
		}
	}
	if err := s.Steer("late", nil, nil); err == nil {
		t.Fatal("steered with no running turn")
	}
}

// T4.4: requestUserInput becomes a question; the answer goes back by id.
func TestAppServerUserInput(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	if err := s.Send("@ask me", nil, nil); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, s)
	if ev.Type != core.EventPermissionRequest || ev.ToolName != core.ToolAskUserQuestion {
		t.Fatalf("event = %#v", ev)
	}
	qs, _ := ev.ToolInputRaw["questions"].([]any)
	if len(qs) != 1 {
		t.Fatalf("questions = %#v", ev.ToolInputRaw)
	}
	if err := s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "allow", UpdatedInput: map[string]any{
		"questions": qs, "answers": map[string]any{"Which database?": "SQLite"},
	}}); err != nil {
		t.Fatal(err)
	}
	for {
		ev := nextEvent(t, s)
		if ev.Type == core.EventText {
			if !strings.Contains(ev.Content, `"db":{"answers":["SQLite"]}`) {
				t.Fatalf("answer = %s", ev.Content)
			}
		}
		if ev.Type == core.EventResult {
			break
		}
	}
}

// A failed turn is an error; a missing thread is "session not found".
func TestAppServerFailuresAndMissingThread(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	if err := s.Send("@fail please", nil, nil); err != nil {
		t.Fatal(err)
	}
	if ev := nextEvent(t, s); ev.Type != core.EventError || !strings.Contains(ev.Error.Error(), "model overloaded") {
		t.Fatalf("event = %#v", ev)
	}
	raw, _ := New(map[string]any{"work_dir": t.TempDir()})
	if _, err := raw.(*Agent).StartSession(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), core.ErrAgentSessionNotFound.Error()) {
		t.Fatalf("resume of a missing thread = %v", err)
	}
}

// A CLI without app-server falls back to codex exec.
func TestAppServerUnavailableFallsBackToExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\necho 'unknown subcommand' >&2\nexit 2\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	raw, _ := New(map[string]any{"work_dir": t.TempDir()})
	sess, err := raw.(*Agent).StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if _, ok := sess.(*codexSession); !ok {
		t.Fatalf("session = %T, want the exec fallback", sess)
	}
}

func collectText(t *testing.T, s *appServerSession, onPerm func(core.Event)) string {
	t.Helper()
	var sb strings.Builder
	for {
		ev := nextEvent(t, s)
		switch ev.Type {
		case core.EventText, core.EventThinking:
			sb.WriteString(ev.Content + "\n")
		case core.EventPermissionRequest:
			if onPerm != nil {
				onPerm(ev)
			}
		case core.EventResult, core.EventError:
			return sb.String()
		}
	}
}

// P4 review: "default" is the user's Codex config, also after bypass and
// after a resume; effort back to automatic is the config's effort; extra
// directories stay writable.
func TestAppServerDefaultsComeFromConfig(t *testing.T) {
	fake := installFakeAppServer(t)
	t.Setenv("ECHO_FAKE_CODEX_CONFIG", `{"approval_policy":"untrusted","sandbox_mode":"workspace-write","model":"gpt-cfg","model_reasoning_effort":"low","sandbox_workspace_write":{"writable_roots":["/cfg/root"]}}`)
	raw, _ := New(map[string]any{"work_dir": t.TempDir(), "mode": "bypassPermissions"})
	a := raw.(*Agent)
	a.SetSessionEnv([]string{"CC_EXTRA_WORK_DIRS=/extra/repo"})
	sess, err := a.StartSession(context.Background(), "thr_1")
	if err != nil {
		t.Fatal(err)
	}
	s := sess.(*appServerSession)
	defer func() { _ = s.Close() }()
	if p := fake.last(t, "thread/resume"); p["excludeTurns"] != true {
		t.Fatalf("resume = %v", p)
	}
	_ = s.Send("hi", nil, nil)
	collectText(t, s, nil)
	if err := a.ApplyLiveSettings(context.Background(), s, core.AgentSessionOptions{Mode: "default", ReasoningLevel: "high"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Send("hi", nil, nil)
	collectText(t, s, nil)
	p := fake.last(t, "turn/start")
	sb, _ := p["sandboxPolicy"].(map[string]any)
	roots := fmt.Sprint(sb["writableRoots"])
	if p["approvalPolicy"] != "untrusted" || sb["type"] != "workspaceWrite" || !strings.Contains(roots, "/cfg/root") || !strings.Contains(roots, "/extra/repo") ||
		p["model"] != "gpt-cfg" || p["effort"] != "high" {
		t.Fatalf("default turn after bypass = %v", p)
	}
	if err := a.ApplyLiveSettings(context.Background(), s, core.AgentSessionOptions{Mode: "default"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Send("hi", nil, nil)
	collectText(t, s, nil)
	if p := fake.last(t, "turn/start"); p["effort"] != "low" {
		t.Fatalf("effort back to automatic = %v", p["effort"])
	}
}

// P4 review: a failed turn/start does not lose the exit from plan mode.
func TestAppServerPlanExitSurvivesFailedStart(t *testing.T) {
	fake := installFakeAppServer(t)
	a, s := startFakeSession(t, "plan", "")
	_ = s.Send("hi", nil, nil)
	collectText(t, s, nil)
	_ = a.ApplyLiveSettings(context.Background(), s, core.AgentSessionOptions{Mode: "default", Model: "bad"})
	if err := s.Send("hi", nil, nil); err == nil {
		t.Fatal("turn/start with a bad model succeeded")
	}
	_ = a.ApplyLiveSettings(context.Background(), s, core.AgentSessionOptions{Mode: "default"})
	_ = s.Send("hi", nil, nil)
	collectText(t, s, nil)
	cm, _ := fake.last(t, "turn/start")["collaborationMode"].(map[string]any)
	if cm["mode"] != "default" {
		t.Fatalf("leaving plan after a failed start = %v", cm)
	}
}

// P4 review: legacy denials use the structured rejection.
func TestAppServerLegacyDenial(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	_ = s.Send("@legacy", nil, nil)
	out := collectText(t, s, func(ev core.Event) {
		_ = s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "deny", Message: "no"})
	})
	if !strings.Contains(out, `reply={"decision":{"denied":{"rejection":"no"}}}`) {
		t.Fatalf("out = %s", out)
	}
}

// P4 review: a permission card shows everything it grants; one too large to
// show is declined.
func TestAppServerPermissionGrants(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	_ = s.Send("@perms", nil, nil)
	out := collectText(t, s, func(ev core.Event) {
		if !strings.Contains(ev.ToolInput, `network: {"enabled":true}`) || !strings.Contains(ev.ToolInput, `write: ["/etc/hosts"]`) {
			t.Errorf("card = %q", ev.ToolInput)
		}
		_ = s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "allow"})
	})
	if !strings.Contains(out, `"network":{"enabled":true}`) {
		t.Fatalf("grant = %s", out)
	}
	_ = s.Send("@bigperms", nil, nil)
	out = collectText(t, s, func(ev core.Event) { t.Errorf("a too-large request reached chat: %d chars", len(ev.ToolInput)) })
	if !strings.Contains(out, "too broad to show") || !strings.Contains(out, `reply={"permissions":{},"scope":"turn"}`) {
		t.Fatalf("out = %s", out)
	}
}

// P4 review: secret questions are declined; duplicate question texts keep
// their own answers.
func TestAppServerSecretAndDuplicateQuestions(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	_ = s.Send("@secret", nil, nil)
	out := collectText(t, s, func(ev core.Event) { t.Error("a secret question reached chat") })
	if !strings.Contains(out, "secret input") || !strings.Contains(out, `"error"`) {
		t.Fatalf("out = %s", out)
	}
	_ = s.Send("@dupq", nil, nil)
	out = collectText(t, s, func(ev core.Event) {
		qs, _ := ev.ToolInputRaw["questions"].([]any)
		a := qs[0].(map[string]any)["question"].(string)
		b := qs[1].(map[string]any)["question"].(string)
		if a == b {
			t.Errorf("questions not told apart: %q", a)
		}
		_ = s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "allow", UpdatedInput: map[string]any{"answers": map[string]any{a: "prod", b: "staging"}}})
	})
	if !strings.Contains(out, `"env_a":{"answers":["prod"]}`) || !strings.Contains(out, `"env_b":{"answers":["staging"]}`) {
		t.Fatalf("out = %s", out)
	}
}

// P4 review: Codex resolving a request itself releases the chat's wait;
// sub-agent threads do not reach the chat.
func TestAppServerResolvedRequestsAndChildThreads(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	_ = s.Send("@resolve", nil, nil)
	out := collectText(t, s, func(ev core.Event) {
		select {
		case <-s.RequestDone(ev.RequestID):
		case <-time.After(3 * time.Second):
			t.Error("resolved request not reported")
		}
	})
	if !strings.Contains(out, "went on without an answer") {
		t.Fatalf("out = %s", out)
	}
	_ = s.Send("@child", nil, nil)
	out = collectText(t, s, nil)
	if strings.Contains(out, "sub-agent") || !strings.Contains(out, "main answer") {
		t.Fatalf("out = %s", out)
	}
}

// P4 review: a write that does not finish in time stops the process (a late
// turn/start must not run).
func TestAppServerStalledWriteStopsProcess(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	prev := rpcCallTimeout
	rpcCallTimeout = 500 * time.Millisecond
	defer func() { rpcCallTimeout = prev }()
	_ = s.Send("@stall", nil, nil) // the fake stops reading
	big := strings.Repeat("x", 1<<20)
	if err := s.Send(big, nil, nil); err == nil {
		t.Fatal("write to a stalled server succeeded")
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.Alive() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.Alive() {
		t.Fatal("session alive after a timed-out write")
	}
}

// P4 review: a late turn/start reply never revives a turn that ended.
func TestAppServerLateStartReplyDoesNotReviveTurn(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	s.turnCompleted(map[string]any{"id": "turn_7", "status": "completed"})
	for range s.Events() {
		break
	}
	s.mu.Lock()
	revived := s.turnID == "" && s.endedTurn != "turn_7"
	s.mu.Unlock()
	if revived {
		t.Fatal("ended turn not recorded")
	}
}

// P4 review: an unavailable app-server never turns a plan request into a
// default (editing) turn.
func TestPlanModeRefusesExecFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell shim")
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nexit 2\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	raw, _ := New(map[string]any{"work_dir": t.TempDir(), "mode": "plan"})
	if _, err := raw.(*Agent).StartSession(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "plan mode needs the app-server") {
		t.Fatalf("err = %v", err)
	}
}

// P4 review (Kimi): the first turn of a new process states the collaboration
// mode, so a resumed thread left in plan mode returns to default.
func TestAppServerFirstTurnStatesCollaborationMode(t *testing.T) {
	fake := installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "thr_1")
	_ = s.Send("hi", nil, nil)
	collectText(t, s, nil)
	cm, _ := fake.last(t, "turn/start")["collaborationMode"].(map[string]any)
	if cm["mode"] != "default" {
		t.Fatalf("first turn after resume = %v", cm)
	}
	_ = s.Send("hi", nil, nil)
	collectText(t, s, nil)
	if _, sent := fake.last(t, "turn/start")["collaborationMode"]; sent {
		t.Fatal("collaboration mode resent once default was confirmed")
	}
}

// P4 review (Kimi): an approval with no command to show is declined.
func TestAppServerDeclinesApprovalWithoutCommand(t *testing.T) {
	installFakeAppServer(t)
	_, s := startFakeSession(t, "default", "")
	_ = s.Send("@stdin", nil, nil)
	out := collectText(t, s, func(core.Event) { t.Error("an empty approval reached chat") })
	if !strings.Contains(out, "without saying what it is") || !strings.Contains(out, `reply={"decision":"decline"}`) {
		t.Fatalf("out = %s", out)
	}
}

// P4 review (Kimi): only a missing thread resets the session; an older CLI
// without thread methods falls back to exec.
func TestThreadNotFoundAndMissingThreadMethods(t *testing.T) {
	for msg, want := range map[string]bool{
		"thread not found: abc": true, "no such rollout file": true,
		"model not found: gpt-9": false, "provider not found": false,
	} {
		if got := isThreadNotFound(errors.New(msg)); got != want {
			t.Errorf("isThreadNotFound(%q) = %v", msg, got)
		}
	}
	installFakeAppServer(t)
	t.Setenv("ECHO_FAKE_CODEX_NO_THREADS", "1")
	raw, _ := New(map[string]any{"work_dir": t.TempDir()})
	sess, err := raw.(*Agent).StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if _, ok := sess.(*codexSession); !ok {
		t.Fatalf("session = %T, want exec fallback", sess)
	}
}
