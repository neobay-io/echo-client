package opencode

import (
	"context"
	"errors"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestAvailableModelsUsesOpenCodeCLI(t *testing.T) {
	agent := &Agent{
		cmd:     "opencode-custom",
		workDir: "/workspace",
		modelRunner: func(_ context.Context, command, workDir string, _ []string) ([]byte, error) {
			if command != "opencode-custom" || workDir != "/workspace" {
				t.Fatalf("runner command=%q workDir=%q", command, workDir)
			}
			return []byte("anthropic/claude-opus-5-5\nopenai/gpt-6-astra\ngoogle/gemini-3.1-pro\n"), nil
		},
	}

	models := agent.AvailableModels(context.Background())
	want := []string{"anthropic/claude-opus-5-5", "openai/gpt-6-astra", "google/gemini-3.1-pro"}
	if len(models) != len(want) {
		t.Fatalf("AvailableModels() = %#v", models)
	}
	for i, name := range want {
		if models[i].Name != name {
			t.Fatalf("model %d = %q, want %q", i, models[i].Name, name)
		}
	}
}

func TestAvailableModelsUsesEffectiveOpenCodeContext(t *testing.T) {
	a := &Agent{providers: []core.ProviderConfig{{Model: "provider-model", Env: map[string]string{"OPENCODE_CONFIG": "/provider-config"}}},
		sessionEnv: []string{"CC_WORKTREE_PATH=/worktree"},
		modelRunner: func(_ context.Context, _, dir string, env []string) ([]byte, error) {
			if dir != "/worktree" || core.EnvValue(env, "OPENCODE_CONFIG") != "/provider-config" {
				t.Fatal("wrong effective context")
			}
			return nil, errors.New("unavailable")
		}}
	if models := a.AvailableModels(context.Background()); len(models) != 1 || models[0].Name != "provider-model" {
		t.Fatalf("fallback: %#v", models)
	}
}
