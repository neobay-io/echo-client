package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestAvailableModelsUsesEffectiveCodexContext(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_HOME", "/ambient")
	a := &Agent{
		workDir:    "/project",
		providers:  []core.ProviderConfig{{Model: "provider-model", Env: map[string]string{"CODEX_HOME": "/provider"}}},
		sessionEnv: []string{"CC_WORKTREE_PATH=/worktree", "CODEX_HOME=/session"},
		modelRunner: func(_ context.Context, dir string, env []string) ([]core.ModelOption, error) {
			if dir != "/worktree" || core.EnvValue(env, "CODEX_HOME") != "/session" {
				t.Error("discovery ignored effective session configuration")
			}
			return nil, errors.New("unavailable")
		},
	}
	if models := a.AvailableModels(context.Background()); len(models) != 1 || models[0].Name != "provider-model" {
		t.Fatalf("fallback = %#v", models)
	}
}

func TestAvailableModelsRejectsNonCodingCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("version") != "test" {
			t.Errorf("bad URL: %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-image-1"},{"id":"gpt-4o-transcribe"},{"id":"gpt-4o-mini-tts"},{"id":"gpt-realtime"}]}`))
	}))
	defer srv.Close()
	a := &Agent{
		providers: []core.ProviderConfig{{APIKey: "test-key", BaseURL: srv.URL + "/v1?version=test"}},
		modelRunner: func(context.Context, string, []string) ([]core.ModelOption, error) {
			return []core.ModelOption{{Name: "coding-model"}}, nil
		},
	}
	if got := a.AvailableModels(context.Background()); len(got) != 1 || got[0].Name != "coding-model" {
		t.Fatalf("models = %#v", got)
	}
}

func TestListCodexModelsPagesAndBoundedCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell fixture")
	}
	for _, mode := range []string{"pages", "timeout", "early-exit"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			script := `#!/bin/sh
sleep 30 &
if [ "$TEST_MODEL_MODE" = timeout ]; then wait; exit; fi
if [ "$TEST_MODEL_MODE" = early-exit ]; then exit; fi
read -r line
printf '%s\n' '{"id":1,"result":{}}'
read -r line
read -r line
printf '%s\n' '{"id":2,"result":{"data":[{"model":"page-one"}],"nextCursor":"next-page"}}'
read -r line
case "$line" in
  *'"cursor":"next-page"'*) printf '%s\n' '{"id":3,"result":{"data":[{"model":"page-two"}],"nextCursor":null}}' ;;
  *) exit 4 ;;
esac
wait
`
			if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			env := core.MergeEnv(os.Environ(), []string{"TEST_MODEL_MODE=" + mode})
			timeout := 500 * time.Millisecond
			if mode == "pages" {
				timeout = 10 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			started := time.Now()
			models, err := listCodexModels(ctx, dir, env)
			if time.Since(started) > timeout+time.Second {
				t.Fatal("cleanup exceeded bound")
			}
			if mode == "pages" {
				if err != nil || len(models) != 2 || models[1].Name != "page-two" {
					t.Fatalf("models=%#v err=%v", models, err)
				}
			} else if err == nil {
				t.Fatal("expected discovery error")
			}
		})
	}
}
