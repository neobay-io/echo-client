package cursor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAvailableModelsUsesCursorCLI(t *testing.T) {
	agent := &Agent{
		cmd:     "cursor-agent-custom",
		workDir: "/workspace",
		modelRunner: func(_ context.Context, command, workDir string, _ []string) ([]byte, error) {
			if command != "cursor-agent-custom" || workDir != "/workspace" {
				t.Fatalf("runner command=%q workDir=%q", command, workDir)
			}
			return []byte("Available models\n\ngpt-6-astra - GPT-6 Astra (default)\nclaude-opus-5-5 - Claude Opus 5.5\nauto - Auto\n\nTip: use --model <id>\n"), nil
		},
	}

	models := agent.AvailableModels(context.Background())
	want := []string{"gpt-6-astra", "claude-opus-5-5", "auto"}
	if len(models) != len(want) {
		t.Fatalf("AvailableModels() = %#v", models)
	}
	for i, name := range want {
		if models[i].Name != name {
			t.Fatalf("model %d = %q, want %q", i, models[i].Name, name)
		}
	}
	if models[0].Desc != "GPT-6 Astra (default)" {
		t.Fatalf("first model description = %q", models[0].Desc)
	}
}

func TestAvailableModelsUsesEffectiveCursorContext(t *testing.T) {
	a := &Agent{providers: []core.ProviderConfig{{Model: "provider-model", Env: map[string]string{"CURSOR_API_KEY": "provider-key"}}},
		sessionEnv: []string{"CC_WORKTREE_PATH=/worktree"},
		modelRunner: func(_ context.Context, _, dir string, env []string) ([]byte, error) {
			if dir != "/worktree" || core.EnvValue(env, "CURSOR_API_KEY") != "provider-key" {
				t.Fatal("wrong effective context")
			}
			return nil, errors.New("unavailable")
		}}
	if models := a.AvailableModels(context.Background()); len(models) != 1 || models[0].Name != "provider-model" {
		t.Fatalf("fallback: %#v", models)
	}
}

func TestCursorModelDiscoveryDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell")
	}
	cmd := filepath.Join(t.TempDir(), "cursor-models")
	if err := os.WriteFile(cmd, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runCursorModelList(ctx, cmd, "", os.Environ())
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("unbounded or successful timeout: %v", err)
	}
}
