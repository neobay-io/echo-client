package codex

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestNormalizeModeMapsLegacyAliasesToCanonicalKeys(t *testing.T) {
	tests := map[string]string{
		"":                  "default",
		"suggest":           "default",
		"auto-edit":         "acceptEdits",
		"acceptEdits":       "acceptEdits",
		"full-auto":         "auto",
		"auto":              "auto",
		"yolo":              "bypassPermissions",
		"bypassPermissions": "bypassPermissions",
		"plan":              "default", // no plan mode in codex exec
		"garbage":           "default",
	}
	for in, want := range tests {
		if got := normalizeMode(in); got != want {
			t.Errorf("normalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
	var keys []string
	for _, m := range (&Agent{}).PermissionModes() {
		keys = append(keys, m.Key)
	}
	if !slices.Equal(keys, []string{"default", "acceptEdits", "auto", "bypassPermissions"}) {
		t.Fatalf("PermissionModes keys = %#v", keys)
	}
}

func TestCodexModeArgsNeverUseRemovedFlags(t *testing.T) {
	for _, mode := range []string{"default", "acceptEdits", "auto", "bypassPermissions", "full-auto", "auto-edit"} {
		for _, arg := range codexModeArgs(mode) {
			// --full-auto is gone; exec resume rejects --sandbox / --approve-for-me.
			if arg == "--full-auto" || arg == "--sandbox" || arg == "-s" || arg == "--approve-for-me" {
				t.Errorf("mode %q uses %q", mode, arg)
			}
		}
	}
	if args := codexModeArgs("auto"); !containsCodexArgPair(args, "-c", `approvals_reviewer="auto_review"`) ||
		!containsCodexArgPair(args, "-c", `sandbox_mode="workspace-write"`) ||
		!containsCodexArgPair(args, "-c", `approval_policy="on-request"`) {
		t.Fatalf("auto args = %#v", args)
	}
	if args := codexModeArgs("acceptEdits"); !containsCodexArgPair(args, "-c", `approval_policy="never"`) {
		t.Fatalf("acceptEdits args = %#v", args)
	}
	if args := codexModeArgs("yolo"); !slices.Equal(args, []string{"--dangerously-bypass-approvals-and-sandbox"}) {
		t.Fatalf("yolo args = %#v", args)
	}
	if args := codexModeArgs("default"); len(args) != 0 {
		t.Fatalf("default args = %#v", args)
	}
}

// Runs a fake codex binary and returns the arguments it received.
func fakeCodexArgs(t *testing.T, mode, resumeID string) []string {
	t.Helper()
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(workDir, "args.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CODEX_ARGS_FILE\"\n" +
		"echo '{\"type\":\"thread.started\",\"thread_id\":\"thread-1\"}'\necho '{\"type\":\"turn.completed\"}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cs, err := newCodexSession(context.Background(), workDir, "", "", mode, resumeID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	if err := cs.Send("hello", nil, nil); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-cs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				data, err := os.ReadFile(argsFile)
				if err != nil {
					t.Fatal(err)
				}
				return strings.Split(strings.TrimSpace(string(data)), "\n")
			}
		case <-timeout:
			t.Fatal("timed out")
		}
	}
}

func TestSendPassesModeOverridesOnNewAndResumedThreads(t *testing.T) {
	fresh := fakeCodexArgs(t, "auto", "")
	if fresh[0] != "exec" || !containsCodexArgPair(fresh, "-c", `approvals_reviewer="auto_review"`) {
		t.Fatalf("new thread args = %#v", fresh)
	}
	resumed := fakeCodexArgs(t, "acceptEdits", "thread-old")
	if resumed[0] != "exec" || resumed[1] != "resume" ||
		!containsCodexArgPair(resumed, "-c", `sandbox_mode="workspace-write"`) ||
		slices.Contains(resumed, "--full-auto") {
		t.Fatalf("resumed thread args = %#v", resumed)
	}
}

func TestStartSettingsPrefersChatOverrides(t *testing.T) {
	a := &Agent{model: "project", reasoningLevel: "medium", mode: "bypassPermissions", activeIdx: 0,
		providers: []core.ProviderConfig{{Name: "p", Model: "provider-model"}}}
	model, level, mode := a.startSettingsLocked()
	if model != "provider-model" || level != "medium" || mode != "bypassPermissions" {
		t.Fatalf("defaults = %q %q %q", model, level, mode)
	}
	a.SetSessionOptions(core.AgentSessionOptions{Model: "chat-model", ReasoningLevel: "x-high", Mode: "full-auto"})
	model, level, mode = a.startSettingsLocked()
	if model != "chat-model" || level != "xhigh" || mode != "auto" {
		t.Fatalf("overrides = %q %q %q", model, level, mode)
	}
	if errs := core.ValidateCapabilities(a); len(errs) != 0 {
		t.Fatalf("capabilities: %v", errs)
	}
}

func TestFindSessionFileMatchesWholeThreadID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "11", "17")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(dir, "rollout-2026-11-17T01-00-00-x019fbb7f-aaaa.jsonl")
	target := filepath.Join(dir, "rollout-2026-11-17T02-00-00-019fbb7f-aaaa.jsonl")
	for _, f := range []string{decoy, target} {
		if err := os.WriteFile(f, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := findSessionFile("019fbb7f-aaaa"); got != target {
		t.Fatalf("findSessionFile = %q, want %q", got, target)
	}
	for _, id := range []string{"", "aaaa", "../x", "*"} {
		if got := findSessionFile(id); got != "" && id != "aaaa" {
			t.Errorf("findSessionFile(%q) = %q", id, got)
		}
	}
	if got := findSessionFile("aaaa"); got != "" {
		t.Errorf("partial id matched %q", got)
	}
}
