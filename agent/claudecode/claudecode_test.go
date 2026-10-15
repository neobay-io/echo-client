package claudecode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestNormalizePermissionMode(t *testing.T) {
	tests := map[string]string{
		"":                   "default",
		"default":            "default",
		"edit":               "acceptEdits",
		"accept-edits":       "acceptEdits",
		"accept_edits":       "acceptEdits",
		"plan":               "plan",
		"yolo":               "bypassPermissions",
		"auto":               "auto",
		"bypass-permissions": "bypassPermissions",
		"bypass_permissions": "bypassPermissions",
		"dontAsk":            "dontAsk",
		"dont-ask":           "dontAsk",
		"dont_ask":           "dontAsk",
		"  DONTASK  ":        "dontAsk",
		"something-unknown":  "default",
	}

	for input, want := range tests {
		if got := normalizePermissionMode(input); got != want {
			t.Fatalf("normalizePermissionMode(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestAvailableModelsFallsBackToCurrentClaudeFamily(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	agent := &Agent{modelRunner: func(context.Context, string) ([]byte, error) {
		return nil, errors.New("CLI unavailable")
	}}
	models := agent.AvailableModels(context.Background())
	var names []string
	for _, model := range models {
		names = append(names, model.Name)
	}

	want := []string{
		"default",
		"opus",
		"sonnet",
		"haiku",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("AvailableModels() = %#v, want %#v", names, want)
	}
}

func TestAvailableModelsUsesClaudeCLIForOAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")

	const cliResult = `{
		"is_error": false,
		"local_command": "model",
		"result": "Current model: ` + "`" + `Opus 5.5 (default)` + "`" + `\nUsage: /model <name>. Available: sonnet, opus, haiku, fable, best, sonnet[1m], opus[1m], fable[1m], opusplan, default, or a full model ID."
	}`
	var gotWorkDir string
	agent := &Agent{
		workDir: "/workspace",
		modelRunner: func(_ context.Context, workDir string) ([]byte, error) {
			gotWorkDir = workDir
			return []byte(cliResult), nil
		},
	}

	models := agent.AvailableModels(context.Background())
	var names []string
	for _, model := range models {
		names = append(names, model.Name)
	}
	want := []string{
		"sonnet", "opus", "haiku", "fable", "best",
		"sonnet[1m]", "opus[1m]", "fable[1m]", "opusplan", "default",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("AvailableModels() = %#v, want %#v", names, want)
	}
	if models[1].Desc != "Opus 5.5 (default)" || models[len(models)-1].Desc != "Opus 5.5 (default)" {
		t.Fatalf("dynamic model descriptions = %#v", models)
	}
	if gotWorkDir != "/workspace" {
		t.Fatalf("model runner workDir = %q, want /workspace", gotWorkDir)
	}
}

func TestParseClaudeModelAliasesRejectsNonModelOutput(t *testing.T) {
	if models := parseClaudeModelAliases("Current model: Opus"); len(models) != 0 {
		t.Fatalf("parseClaudeModelAliases() = %#v, want no models", models)
	}
	if models := parseClaudeModelAliases("Available: opus, OPUS, sonnet.\n"); len(models) != 2 || models[0].Name != "opus" || !strings.EqualFold(models[1].Name, "sonnet") {
		t.Fatalf("parseClaudeModelAliases() = %#v", models)
	}
}

func TestAvailableModelsUsesAPIWhenAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Fatalf("x-api-key = %q, want test-key", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{"id": "claude-opus-4-7", "display_name": "Claude Opus 4.7"},
				{"id": "claude-sonnet-4-6", "display_name": "Claude Sonnet 4.6"}
			]
		}`))
	}))
	defer srv.Close()

	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	agent := &Agent{modelRunner: func(context.Context, string) ([]byte, error) {
		t.Fatal("Claude CLI should not run when provider API returns models")
		return nil, nil
	}}
	models := agent.AvailableModels(context.Background())
	if len(models) != 2 {
		t.Fatalf("len(models) = %d, want 2", len(models))
	}
	if models[0].Name != "claude-opus-4-7" || models[1].Name != "claude-sonnet-4-6" {
		t.Fatalf("models = %#v", models)
	}
}
