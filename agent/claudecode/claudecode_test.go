package claudecode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
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

func TestAvailableModelsFollowsPagination(t *testing.T) {
	var afterIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		afterIDs = append(afterIDs, r.URL.Query().Get("after_id"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-a","display_name":"A"},{"id":"claude-b"}],"has_more":true,"last_id":"claude-b"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-c","display_name":"C"}],"has_more":false,"last_id":"claude-c"}`))
	}))
	defer srv.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)

	models := (&Agent{}).fetchModelsFromAPI(context.Background())
	var names []string
	for _, m := range models {
		names = append(names, m.Name)
	}
	if !slices.Equal(names, []string{"claude-a", "claude-b", "claude-c"}) {
		t.Fatalf("models = %#v", names)
	}
	if !slices.Equal(afterIDs, []string{"", "claude-b"}) {
		t.Fatalf("after_id sequence = %#v", afterIDs)
	}
}

func TestAvailableModelsDoesNotSendKeyAcrossRedirects(t *testing.T) {
	leaked := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" {
			leaked = true
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"evil"}]}`))
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/models", http.StatusFound)
	}))
	defer redirector.Close()
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_BASE_URL", redirector.URL)

	if models := (&Agent{}).fetchModelsFromAPI(context.Background()); len(models) != 0 {
		t.Fatalf("models = %#v, want none from a redirect", models)
	}
	if leaked {
		t.Fatal("API key followed the redirect")
	}
}

func TestStartSettingsPrefersChatOverrides(t *testing.T) {
	a := &Agent{model: "opus", reasoningLevel: "high", mode: "bypassPermissions", activeIdx: -1}
	a.SetSessionOptions(core.AgentSessionOptions{Model: "sonnet", ReasoningLevel: "auto", Mode: "plan"})
	model, level, mode := a.startSettingsLocked()
	if model != "sonnet" || level != "" || mode != "plan" {
		t.Fatalf("overrides = %q %q %q", model, level, mode)
	}
	a.SetSessionOptions(core.AgentSessionOptions{})
	if model, level, mode = a.startSettingsLocked(); model != "opus" || level != "high" || mode != "bypassPermissions" {
		t.Fatalf("defaults = %q %q %q", model, level, mode)
	}
}

func TestCapabilitiesAreImplemented(t *testing.T) {
	a := &Agent{activeIdx: -1}
	if errs := core.ValidateCapabilities(a); len(errs) != 0 {
		t.Fatalf("capabilities: %v", errs)
	}
	caps := core.CapabilitiesOf(a)
	if !caps.Auth.Login || !caps.Models || !caps.Compact || !caps.Sessions.Delete || !slices.Contains(caps.Modes, "auto") {
		t.Fatalf("claude capabilities = %#v", caps)
	}
}

func TestDeleteSessionFindsTranscriptOutsideCurrentWorkDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldProject := filepath.Join(home, ".claude", "projects", "-old-project")
	if err := os.MkdirAll(oldProject, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(oldProject, "51b25019-4af9-4a1d-9d3c-0123456789ab.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(oldProject, "other.jsonl")
	if err := os.WriteFile(keep, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &Agent{workDir: t.TempDir(), activeIdx: -1} // /dir moved away from the session's project
	if err := a.DeleteSession(context.Background(), "51b25019-4af9-4a1d-9d3c-0123456789ab"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatal("transcript still exists")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("unrelated transcript was removed")
	}
	for _, bad := range []string{"../other", "*", "", "a/b"} {
		if err := a.DeleteSession(context.Background(), bad); err == nil {
			t.Errorf("DeleteSession(%q) succeeded", bad)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("malformed id removed a transcript")
	}
}
