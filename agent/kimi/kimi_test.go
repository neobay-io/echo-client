package kimi

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestBuildArgsNewAndResumedTurns(t *testing.T) {
	ks := newKimiSession(context.Background(), sessionConfig{
		extraArgs: []string{"--agent", "coder"},
		model:     "kimi-code/k3",
		extraDirs: []string{"/extra/one", " "},
	}, "")
	args := ks.buildArgs("-starts with a dash")
	want := []string{"--agent", "coder", "--output-format", "stream-json", "--model", "kimi-code/k3", "--add-dir", "/extra/one", "--prompt=-starts with a dash"}
	if !slices.Equal(args, want) {
		t.Fatalf("new turn args = %q, want %q", args, want)
	}
	for _, a := range args {
		if a == "--plan" || a == "--yolo" || a == "--auto" {
			t.Fatalf("permission flag %q must never be passed with -p", a)
		}
	}

	ks.sessionID.Store("session_abc")
	args = ks.buildArgs("next")
	if i := slices.Index(args, "-S"); i < 0 || args[i+1] != "session_abc" {
		t.Fatalf("resumed turn args = %q, want -S session_abc", args)
	}
}

func TestEffortIsPassedThroughEnv(t *testing.T) {
	ks := newKimiSession(context.Background(), sessionConfig{effort: "low", extraEnv: []string{"FOO=bar"}}, "")
	env := ks.buildEnv()
	if !slices.Contains(env, effortEnv+"=low") || !slices.Contains(env, "FOO=bar") {
		t.Fatalf("env missing effort or extra env: %q", env)
	}
	none := newKimiSession(context.Background(), sessionConfig{}, "").buildEnv()
	for _, kv := range none {
		if strings.HasPrefix(kv, effortEnv+"=") && os.Getenv(effortEnv) == "" {
			t.Fatalf("effort env set without an effort: %q", kv)
		}
	}
}

func TestParseKimiModels(t *testing.T) {
	// Shape of `kimi provider list --json`; providers carry credentials that
	// must not survive parsing.
	out := []byte(`{
		"providers": {"managed:kimi-code": {"type": "kimi", "apiKey": "sk-secret", "baseUrl": "https://x"}},
		"models": {
			"kimi-code/k3": {"displayName": "K3", "supportEfforts": ["low", "high", "max"], "defaultEffort": "high", "provider": "managed:kimi-code"},
			"kimi-code/kimi-for-coding-highspeed": {"displayName": "K2.7 Code Highspeed"},
			"kimi-code/raw": {"displayName": "kimi-code/raw"}
		}}`)
	models, err := parseKimiModels(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0].Alias != "kimi-code/k3" || models[1].Alias != "kimi-code/kimi-for-coding-highspeed" {
		t.Fatalf("models = %+v", models)
	}
	if !slices.Equal(models[0].Efforts, []string{"low", "high", "max"}) || models[0].DefaultEffort != "high" {
		t.Fatalf("k3 efforts = %+v", models[0])
	}
	if models[2].DisplayName != "" {
		t.Fatalf("display name equal to alias should be dropped: %+v", models[2])
	}
	if got := effortUnion(models); !slices.Equal(got, []string{"low", "high", "max"}) {
		t.Fatalf("effortUnion = %q", got)
	}
}

func newTestAgent(t *testing.T, models []kimiModel) *Agent {
	t.Helper()
	a := &Agent{workDir: t.TempDir()}
	a.catalog = newModelCatalog(func(context.Context) ([]kimiModel, error) { return models, nil })
	a.catalog.get(context.Background())
	return a
}

func TestEffortDroppedForModelsWithoutIt(t *testing.T) {
	a := newTestAgent(t, []kimiModel{
		{Alias: "kimi-code/k3", Efforts: []string{"low", "high", "max"}},
		{Alias: "kimi-code/highspeed"},
	})
	if got := a.effortForModel("kimi-code/k3", "low"); got != "low" {
		t.Fatalf("supported effort dropped: %q", got)
	}
	if got := a.effortForModel("kimi-code/highspeed", "low"); got != "" {
		t.Fatalf("unsupported effort kept: %q", got)
	}
	if got := a.effortForModel("unknown/model", "max"); got != "max" {
		t.Fatalf("unknown model should keep the effort: %q", got)
	}
}

func TestAvailableReasoningLevelsDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	a := &Agent{}
	a.catalog = newModelCatalog(func(ctx context.Context) ([]kimiModel, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, ctx.Err()
	})
	start := time.Now()
	levels := a.AvailableReasoningLevels()
	if time.Since(start) > time.Second {
		t.Fatal("AvailableReasoningLevels blocked on model discovery")
	}
	if len(levels) != 3 || levels[0].Name != "low" || levels[2].Name != "max" {
		t.Fatalf("fallback levels = %+v", levels)
	}
}

func TestOnlyBypassModeIsOffered(t *testing.T) {
	a := &Agent{}
	modes := a.PermissionModes()
	if len(modes) != 1 || modes[0].Key != core.PermissionModeBypass {
		t.Fatalf("modes = %+v, want only bypassPermissions", modes)
	}
	a.SetMode("plan")
	if a.GetMode() != core.PermissionModeBypass {
		t.Fatalf("GetMode = %q", a.GetMode())
	}
	// "auto" is Kimi's own reviewed mode (ACP backend), never a bypass alias;
	// prompt mode does not list it.
	if core.IsBypassPermissionMode("kimi", "auto") {
		t.Fatal("auto must not mean bypass for kimi")
	}
	if !core.IsBypassPermissionMode("kimi", "yolo") || !core.IsBypassPermissionMode("kimi", "bypassPermissions") {
		t.Fatal("yolo/bypassPermissions must be bypass for kimi (Echo workers)")
	}
	if errs := core.ValidateCapabilities(a); len(errs) > 0 {
		t.Fatalf("capabilities: %v", errs)
	}
}

func TestStreamEventsMapToCoreEvents(t *testing.T) {
	ks := newKimiSession(context.Background(), sessionConfig{}, "")
	ks.handleEvent(map[string]any{"role": "meta", "type": "system.version", "version": "2.5.3"})
	ks.handleEvent(map[string]any{"role": "assistant", "content": "Let me look.", "tool_calls": []any{
		map[string]any{"id": "tool_1", "type": "function", "function": map[string]any{"name": "Write", "arguments": `{"path":"a.txt"}`}},
	}})
	ks.handleEvent(map[string]any{"role": "tool", "tool_call_id": "tool_1", "content": "Wrote 7 bytes"})
	ks.handleEvent(map[string]any{"role": "assistant", "content": "Done."})
	ks.handleEvent(map[string]any{"role": "meta", "type": "session.resume_hint", "session_id": "session_xyz"})
	ks.flushPendingAsText()

	var got []string
	for len(ks.events) > 0 {
		evt := <-ks.events
		got = append(got, string(evt.Type)+":"+evt.Content+evt.ToolName+evt.ToolResult)
	}
	want := []string{
		string(core.EventThinking) + ":Let me look.",
		string(core.EventToolUse) + ":Write",
		string(core.EventToolResult) + ":Wrote 7 bytes",
		string(core.EventText) + ":Done.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if ks.CurrentSessionID() != "session_xyz" {
		t.Fatalf("session id = %q", ks.CurrentSessionID())
	}
}

// fakeKimi writes a script that records its argv and the effort env, then
// prints a Kimi Code stream-json turn.
func fakeKimi(t *testing.T, exitCode int) (cmd, argsLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake CLI")
	}
	dir := t.TempDir()
	argsLog = filepath.Join(dir, "args.log")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsLog + `"
echo "effort=$` + effortEnv + `" >> "` + argsLog + `"
echo '{"role":"meta","type":"system.version","version":"2.5.3"}'
echo '{"role":"assistant","content":"pong"}'
echo '{"role":"meta","type":"session.resume_hint","session_id":"session_fake1"}'
if [ ` + string(rune('0'+exitCode)) + ` -ne 0 ]; then echo "boom: quota exhausted" >&2; exit ` + string(rune('0'+exitCode)) + `; fi
`
	cmd = filepath.Join(dir, "kimi")
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, argsLog
}

func collectTurn(t *testing.T, s core.AgentSession) []core.Event {
	t.Helper()
	var events []core.Event
	timeout := time.After(waitTimeout)
	for {
		select {
		case evt := <-s.Events():
			events = append(events, evt)
			if evt.Type == core.EventResult || evt.Type == core.EventError {
				return events
			}
		case <-timeout:
			t.Fatalf("turn did not finish; events so far: %+v", events)
		}
	}
}

func TestSessionRunsCLIAndResumes(t *testing.T) {
	cmd, argsLog := fakeKimi(t, 0)
	a := &Agent{workDir: t.TempDir(), cmd: cmd, model: "kimi-code/k3", reasoningLevel: "max"}
	a.catalog = newModelCatalog(func(context.Context) ([]kimiModel, error) { return nil, nil })
	a.SetSessionOptions(core.AgentSessionOptions{ReasoningLevel: "low"})

	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	events := collectTurn(t, s)
	last := events[len(events)-1]
	if last.Type != core.EventResult || last.SessionID != "session_fake1" {
		t.Fatalf("last event = %+v", last)
	}
	var text string
	for _, e := range events {
		if e.Type == core.EventText {
			text += e.Content
		}
	}
	if text != "pong" {
		t.Fatalf("text = %q", text)
	}
	logged, _ := os.ReadFile(argsLog)
	if !strings.Contains(string(logged), "effort=low") || strings.Contains(string(logged), "\n-S\n") {
		t.Fatalf("first turn args/env = %s", logged)
	}

	if err := s.Send("again", nil, nil); err != nil {
		t.Fatal(err)
	}
	collectTurn(t, s)
	logged, _ = os.ReadFile(argsLog)
	if !strings.Contains(string(logged), "-S\nsession_fake1\n") {
		t.Fatalf("second turn should resume: %s", logged)
	}
}

func TestSessionReportsCLIFailure(t *testing.T) {
	cmd, _ := fakeKimi(t, 3)
	a := &Agent{workDir: t.TempDir(), cmd: cmd}
	a.catalog = newModelCatalog(func(context.Context) ([]kimiModel, error) { return nil, nil })
	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	events := collectTurn(t, s)
	last := events[len(events)-1]
	if last.Type != core.EventError || !strings.Contains(last.Error.Error(), "quota exhausted") {
		t.Fatalf("last event = %+v, want error with stderr", last)
	}
}

// Review finding 2: Kimi starts only with an explicit bypass mode.
func TestNewRequiresExplicitBypassMode(t *testing.T) {
	cmd := filepath.Join(t.TempDir(), "kimi")
	if err := os.WriteFile(cmd, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "default", "plan", "auto"} {
		if _, err := New(map[string]any{"cmd": cmd, "mode": mode}); err == nil || !strings.Contains(err.Error(), "bypassPermissions") {
			t.Errorf("mode %q: err = %v, want a bypassPermissions error", mode, err)
		}
	}
	for _, mode := range []string{"bypassPermissions", "yolo"} {
		if _, err := New(map[string]any{"cmd": cmd, "mode": mode}); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
}

// P6: backend = "acp" offers Kimi's modes, live settings, interrupt and
// native /compact; prompt mode keeps bypass only.
func TestACPBackend(t *testing.T) {
	if _, err := New(map[string]any{"cmd": "/bin/sh", "backend": "acp", "mode": "plan"}); err != nil {
		t.Fatalf("acp with plan: %v", err)
	}
	if _, err := New(map[string]any{"cmd": "/bin/sh", "mode": "plan"}); err == nil {
		t.Fatal("prompt mode accepted plan")
	}
	if _, err := New(map[string]any{"cmd": "/bin/sh", "backend": "rpc", "mode": "yolo"}); err == nil {
		t.Fatal("unknown backend accepted")
	}
	raw, err := New(map[string]any{"cmd": "/bin/sh", "backend": "acp"})
	if err != nil {
		t.Fatal(err)
	}
	a := raw.(*Agent)
	if a.GetMode() != core.PermissionModeDefault || len(a.PermissionModes()) != 4 {
		t.Fatalf("mode %q, modes %+v", a.GetMode(), a.PermissionModes())
	}
	a.SetMode("yolo")
	if a.GetMode() != core.PermissionModeBypass {
		t.Fatalf("SetMode(yolo) -> %q", a.GetMode())
	}
	caps := core.CapabilitiesOf(a)
	if !caps.LiveModel || !caps.LiveMode || !caps.Interrupt || caps.Steer || !caps.Compact {
		t.Fatalf("caps = %+v", caps)
	}
	if errs := core.ValidateCapabilities(a); len(errs) > 0 {
		t.Fatalf("capabilities: %v", errs)
	}
	// A prompt-mode session cannot switch live.
	if err := a.ApplyLiveSettings(context.Background(), newKimiSession(context.Background(), sessionConfig{}, ""), core.AgentSessionOptions{Model: "m"}); err == nil {
		t.Fatal("prompt session switched live")
	}
	print := &Agent{}
	if print.CompressCommand() != "" || core.CapabilitiesOf(print).Compact {
		t.Fatal("prompt mode offers /compact")
	}
}
