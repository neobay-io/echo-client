package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/agent/antigravityhook"
	"github.com/chenhg5/cc-connect/core"
)

// Trimmed real output of `agy models` (agy 1.5.10).
const agyModelsOutput = "Fetching available models...\n" +
	"gemini-4-argon-medium\tGemini 4 Argon (Medium)\n" +
	"gemini-4-argon-high\tGemini 4 Argon (High)\n" +
	"gemini-4-argon-low\tGemini 4 Argon (Low)\n" +
	"gemini-pro-orchestrator-g1-pro\tOrchestrator (Argon)\n" +
	"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n" +
	"claude-opus-5-5-low\tClaude Opus 5.5 (Low)\n" +
	"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n" +
	"odd-model-high\tOdd Model\n"

// TestMain lets this test binary stand in for `echo-client
// _agy-permission-hook`, the command the permission bridge registers as
// agy's PreToolUse hook (it runs os.Executable()).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "_agy-permission-hook" {
		if err := antigravityhook.Relay(os.Stdin, os.Stdout, os.Getenv(antigravityhook.EnvAddress), os.Getenv(antigravityhook.EnvToken)); err != nil {
			_, _ = os.Stdout.WriteString(`{"decision":"deny","reason":"relay failed"}` + "\n")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestParseAgyModelsGroupsEffortVariants(t *testing.T) {
	models := parseAgyModels([]byte(agyModelsOutput))
	got := map[string]agyModel{}
	var order []string
	for _, m := range models {
		got[m.ID] = m
		order = append(order, m.ID)
	}
	wantOrder := []string{"gemini-4-argon", "gemini-pro-orchestrator-g1-pro", "claude-opus-5-5", "gpt-oss-120b", "odd-model-high"}
	if !slices.Equal(order, wantOrder) {
		t.Fatalf("order = %q, want %q", order, wantOrder)
	}
	if m := got["gemini-4-argon"]; m.Label != "Gemini 4 Argon" || !slices.Equal(m.Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("argon = %+v", m)
	}
	if m := got["gemini-pro-orchestrator-g1-pro"]; len(m.Efforts) != 0 || m.Label != "Orchestrator (Argon)" {
		t.Fatalf("orchestrator = %+v", m)
	}
	// A suffix the label does not name is part of the model name.
	if m := got["odd-model-high"]; len(m.Efforts) != 0 {
		t.Fatalf("odd model = %+v", m)
	}
	if u := effortUnion(models); !slices.Equal(u, []string{"low", "medium", "high"}) {
		t.Fatalf("effortUnion = %q", u)
	}
}

func TestModelArgsNeverCombineSuffixedIDWithEffort(t *testing.T) {
	cases := []struct {
		model, effort string
		want          []string
	}{
		{"", "", nil},
		{"gemini-3.8-flash", "", []string{"--model", "gemini-3.8-flash"}},
		{"gemini-3.8-flash-low", "", []string{"--model", "gemini-3.8-flash-low"}},
		{"gemini-3.8-flash", "high", []string{"--model", "gemini-3.8-flash", "--effort", "high"}},
		{"gemini-3.8-flash-low", "high", []string{"--model", "gemini-3.8-flash", "--effort", "high"}},
		{"", "low", []string{"--effort", "low"}},
	}
	for _, c := range cases {
		if got := modelArgs(c.model, c.effort); !slices.Equal(got, c.want) {
			t.Errorf("modelArgs(%q,%q) = %q, want %q", c.model, c.effort, got, c.want)
		}
	}
}

func TestModesMapToAgyFlags(t *testing.T) {
	cases := map[string][]string{
		"default":           nil,
		"acceptEdits":       {"--mode", "accept-edits"},
		"plan":              {"--mode", "plan"},
		"auto":              {"--mode", "auto"},
		"bypassPermissions": {"--dangerously-skip-permissions"},
	}
	for mode, want := range cases {
		if got := modeArgs(mode); !slices.Equal(got, want) {
			t.Errorf("modeArgs(%q) = %q, want %q", mode, got, want)
		}
	}
	for raw, want := range map[string]string{"yolo": "bypassPermissions", "accept-edits": "acceptEdits", "": "default", "bogus": "default"} {
		if got := normalizeMode(raw); got != want {
			t.Errorf("normalizeMode(%q) = %q, want %q", raw, got, want)
		}
	}
	if !core.IsBypassPermissionMode("antigravity", "yolo") || core.IsBypassPermissionMode("antigravity", "auto") {
		t.Fatal("bypass equivalence for Echo workers is wrong")
	}
	a := &Agent{}
	if errs := core.ValidateCapabilities(a); len(errs) > 0 {
		t.Fatalf("capabilities: %v", errs)
	}
}

func TestEffortDroppedWhenModelLacksIt(t *testing.T) {
	a := &Agent{}
	a.catalog = newModelCatalog(func(context.Context) ([]agyModel, error) {
		return parseAgyModels([]byte(agyModelsOutput)), nil
	})
	a.catalog.get(context.Background())
	if got := a.effortForModel("gemini-4-argon", "high"); got != "high" {
		t.Fatalf("supported effort dropped: %q", got)
	}
	if got := a.effortForModel("claude-opus-5-5-low", "medium"); got != "" {
		t.Fatalf("unsupported effort kept: %q", got)
	}
	if got := a.effortForModel("gemini-pro-orchestrator-g1-pro", "high"); got != "high" {
		t.Fatalf("model without listed efforts should keep it: %q", got)
	}
}

// fakeAgy writes a script that logs argv, answers like `agy -p
// --output-format stream-json`, and in default mode first asks the
// permission hook the way agy's PreToolUse hook would.
func fakeAgy(t *testing.T, hookBin string, fail bool) (cmd, argsLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake CLI")
	}
	dir := t.TempDir()
	argsLog = filepath.Join(dir, "args.log")
	result := `{"event":"result","result":{"conversation_id":"conv-1","status":"SUCCESS","response":"pong\n"}}`
	if fail {
		result = `{"event":"result","result":{"conversation_id":"conv-1","status":"ERROR","error":"No capacity available for model"}}`
	}
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsLog + `"
echo '{"event":"init","conversation_id":"conv-1","init":{"cwd":"/w"}}'
if [ -n "$CC_CONNECT_AGY_PERMISSION_ADDR" ]; then
  decision=$(echo '{"conversationId":"conv-1","stepIdx":1,"toolCall":{"name":"write_file","args":{"Path":"a.txt"}}}' | "` + hookBin + `" _agy-permission-hook)
  echo "decision=$decision" >> "` + argsLog + `"
fi
echo '{"event":"step_update","step_update":{"conversation_id":"conv-1","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"pong"}}'
echo '` + result + `'
`
	if fail {
		script += "exit 3\n"
	}
	cmd = filepath.Join(dir, "agy")
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, argsLog
}

func collectTurn(t *testing.T, s core.AgentSession, onPermission func(core.Event)) []core.Event {
	t.Helper()
	var events []core.Event
	timeout := time.After(20 * time.Second)
	for {
		select {
		case evt := <-s.Events():
			events = append(events, evt)
			if evt.Type == core.EventPermissionRequest && onPermission != nil {
				onPermission(evt)
			}
			if evt.Type == core.EventResult || evt.Type == core.EventError {
				return events
			}
		case <-timeout:
			t.Fatalf("turn did not finish; events: %+v", events)
		}
	}
}

func TestDefaultModeForwardsApprovalsAndResumes(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the bridge mirrors ~/.gemini into an overlay
	hook, err := os.Executable()  // TestMain answers _agy-permission-hook
	if err != nil {
		t.Fatal(err)
	}
	cmd, argsLog := fakeAgy(t, hook, false)
	a := &Agent{workDir: t.TempDir(), cmd: cmd, mode: "default", model: "gemini-3.8-flash-low", reasoningLevel: "high"}
	a.catalog = newModelCatalog(func(context.Context) ([]agyModel, error) { return nil, nil })
	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Send("ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	var asked core.Event
	events := collectTurn(t, s, func(evt core.Event) {
		asked = evt
		if err := s.RespondPermission(evt.RequestID, core.PermissionResult{Behavior: "deny", Message: "not now"}); err != nil {
			t.Errorf("RespondPermission: %v", err)
		}
	})
	if asked.ToolName != "write_file" {
		t.Fatalf("permission request = %+v", asked)
	}
	last := events[len(events)-1]
	if last.Type != core.EventResult || last.SessionID != "conv-1" {
		t.Fatalf("last = %+v", last)
	}
	logged, _ := os.ReadFile(argsLog)
	for _, want := range []string{"--gemini_dir=", "--model\ngemini-3.8-flash\n--effort\nhigh\n", `"decision":"deny"`, `"reason":"not now"`} {
		if !strings.Contains(string(logged), want) {
			t.Fatalf("first turn log missing %q:\n%s", want, logged)
		}
	}
	if strings.Contains(string(logged), "--conversation") {
		t.Fatalf("first turn must not resume:\n%s", logged)
	}

	if err := s.Send("again", nil, nil); err != nil {
		t.Fatal(err)
	}
	collectTurn(t, s, func(evt core.Event) {
		_ = s.RespondPermission(evt.RequestID, core.PermissionResult{Behavior: "allow"})
	})
	logged, _ = os.ReadFile(argsLog)
	if !strings.Contains(string(logged), "--conversation\nconv-1\n") || !strings.Contains(string(logged), "-p\nagain\n\n(echo-client reminder)\n") {
		t.Fatalf("second turn should resume with the prompt last:\n%s", logged)
	}
}

func TestReadOnlyToolsSkipTheChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan core.Event, 1)
	bridge, err := newAgyPermissionBridge(ctx, events)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	var out bytes.Buffer
	input := `{"toolCall":{"name":"read_file","args":{"Path":"a.txt"}}}`
	if err := antigravityhook.Relay(strings.NewReader(input), &out, bridge.address, bridge.token); err != nil {
		t.Fatal(err)
	}
	var resp antigravityhook.BridgeResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil || resp.Decision != "allow" {
		t.Fatalf("read_file decision = %+v (%v)", resp, err)
	}
	if len(events) != 0 {
		t.Fatalf("read-only tool reached the chat: %+v", <-events)
	}
}

func TestNonDefaultModesSkipTheBridge(t *testing.T) {
	cmd, argsLog := fakeAgy(t, "/nonexistent", false)
	a := &Agent{workDir: t.TempDir(), cmd: cmd, mode: "bypassPermissions"}
	a.catalog = newModelCatalog(func(context.Context) ([]agyModel, error) { return nil, nil })
	s, err := a.StartSession(context.Background(), "existing-conv")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Send("ping", nil, nil)
	collectTurn(t, s, nil)
	logged, _ := os.ReadFile(argsLog)
	if strings.Contains(string(logged), "--gemini_dir") || !strings.Contains(string(logged), "--dangerously-skip-permissions") ||
		!strings.Contains(string(logged), "--conversation\nexisting-conv\n") {
		t.Fatalf("args:\n%s", logged)
	}
	if err := s.RespondPermission("x", core.PermissionResult{Behavior: "allow"}); err == nil {
		t.Fatal("RespondPermission outside default mode should fail")
	}
}

func TestFailedResultBecomesOneError(t *testing.T) {
	cmd, _ := fakeAgy(t, "/nonexistent", true)
	a := &Agent{workDir: t.TempDir(), cmd: cmd, mode: "auto"}
	a.catalog = newModelCatalog(func(context.Context) ([]agyModel, error) { return nil, nil })
	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.Send("ping", nil, nil)
	events := collectTurn(t, s, nil)
	last := events[len(events)-1]
	if last.Type != core.EventError || !strings.Contains(last.Error.Error(), "No capacity") {
		t.Fatalf("last = %+v", last)
	}
	select {
	case extra := <-s.Events():
		t.Fatalf("extra event after error: %+v", extra)
	case <-time.After(200 * time.Millisecond):
	}
}

// Review finding 10: a command approval names the directory it runs in.
func TestCommandApprovalNamesDirectory(t *testing.T) {
	args := map[string]any{"CommandLine": "git clean -fdx"}
	if got := formatAgyApproval(args, "/repo/a"); got != "git clean -fdx\n(in /repo/a)" {
		t.Fatalf("session cwd: %q", got)
	}
	args["Cwd"] = "/repo/b"
	if got := formatAgyApproval(args, "/repo/a"); got != "git clean -fdx\n(in /repo/b)" {
		t.Fatalf("tool cwd should win: %q", got)
	}
	if got := formatAgyApproval(map[string]any{"Path": "a.txt"}, "/repo/a"); strings.Contains(got, "(in ") {
		t.Fatalf("non-command preview changed: %q", got)
	}
}
