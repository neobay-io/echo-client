package iflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestAvailableModelsUsesEffectiveEndpointAndQuery(t *testing.T) {
	var ambientHits atomic.Int32
	ambient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ambientHits.Add(1)
	}))
	defer ambient.Close()
	t.Setenv("IFLOW_BASE_URL", ambient.URL)
	t.Setenv("IFLOW_baseUrl", ambient.URL)
	correct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.URL.Query().Get("api-version") != "test" {
			t.Errorf("wrong endpoint: %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer session-key" {
			t.Error("wrong effective key")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"effective-model"}]}`))
	}))
	defer correct.Close()
	a := &Agent{
		providers:  []core.ProviderConfig{{APIKey: "provider-key", Env: map[string]string{"IFLOW_BASE_URL": correct.URL + "/v1?api-version=test"}}},
		sessionEnv: []string{"IFLOW_API_KEY=session-key"},
	}
	models := a.AvailableModels(context.Background())
	if len(models) != 1 || models[0].Name != "effective-model" || ambientHits.Load() != 0 {
		t.Fatalf("models=%#v, ambient hits=%d", models, ambientHits.Load())
	}
}

func TestAvailableModelsRejectsDowngradeRedirect(t *testing.T) {
	var destinationHits atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationHits.Add(1) }))
	defer destination.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/models", http.StatusFound)
	}))
	defer origin.Close()
	transport := http.DefaultTransport
	http.DefaultTransport = origin.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = transport })
	a := &Agent{providers: []core.ProviderConfig{{APIKey: "secret", BaseURL: origin.URL}}}
	if models := a.fetchModelsFromAPI(context.Background()); len(models) != 0 {
		t.Fatalf("unexpected redirected catalog: %#v", models)
	}
	if destinationHits.Load() != 0 {
		t.Fatal("followed credential-bearing redirect")
	}
}

func TestNormalizeMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "default"},
		{"default", "default"},
		{"AUTO-EDIT", "auto-edit"},
		{"auto_edit", "auto-edit"},
		{"edit", "auto-edit"},
		{"plan", "plan"},
		{"yolo", "yolo"},
		{"force", "yolo"},
		{"unknown", "default"},
	}
	for _, tc := range cases {
		if got := normalizeMode(tc.in); got != tc.want {
			t.Fatalf("normalizeMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAvailableModelsUsesActiveProviderAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen-next"},{"id":"deepseek-next"}]}`))
	}))
	defer srv.Close()

	agent := &Agent{
		providers: []core.ProviderConfig{{Name: "custom", APIKey: "test-key", BaseURL: srv.URL + "/v1"}},
		activeIdx: 0,
	}
	models := agent.AvailableModels(context.Background())
	if len(models) != 2 || models[0].Name != "deepseek-next" || models[1].Name != "qwen-next" {
		t.Fatalf("AvailableModels() = %#v", models)
	}
}

func TestProviderEnvLocked(t *testing.T) {
	a := &Agent{
		providers: []core.ProviderConfig{
			{
				Name:    "custom",
				APIKey:  "k1",
				BaseURL: "https://example.com/v1",
				Env: map[string]string{
					"FOO": "bar",
				},
			},
		},
		activeIdx: 0,
	}

	got := a.providerEnvLocked()
	wantSubset := []string{
		"IFLOW_API_KEY=k1",
		"IFLOW_apiKey=k1",
		"IFLOW_BASE_URL=https://example.com/v1",
		"IFLOW_baseUrl=https://example.com/v1",
		"FOO=bar",
	}

	for _, item := range wantSubset {
		if !contains(got, item) {
			t.Fatalf("providerEnvLocked() missing %q; got=%v", item, got)
		}
	}
}

func TestIFlowProjectKey(t *testing.T) {
	path := "/Users/test/project"
	got := iflowProjectKey(path)
	if got != "-Users-test-project" {
		t.Fatalf("iflowProjectKey(%q) = %q", path, got)
	}

	if got := iflowProjectKey(""); got != "" {
		t.Fatalf("iflowProjectKey(\"\") = %q, want empty", got)
	}
}

func TestIFlowResolvedWorkDir(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	linkDir := filepath.Join(base, "link")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	want, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(realDir): %v", err)
	}

	if got := iflowResolvedWorkDir(linkDir); got != want {
		t.Fatalf("iflowResolvedWorkDir(%q) = %q, want %q", linkDir, got, want)
	}
}

func TestExtractIFlowContentText(t *testing.T) {
	if got := extractIFlowContentText("hello"); got != "hello" {
		t.Fatalf("extractIFlowContentText string = %q", got)
	}

	arr := []any{map[string]any{"type": "text", "text": "from-array"}}
	if got := extractIFlowContentText(arr); got != "from-array" {
		t.Fatalf("extractIFlowContentText array = %q", got)
	}

	if got := extractIFlowContentText(123); got != "" {
		t.Fatalf("extractIFlowContentText unexpected = %q", got)
	}
}

func TestPermissionModesKeys(t *testing.T) {
	a := &Agent{}
	modes := a.PermissionModes()
	got := make([]string, 0, len(modes))
	for _, m := range modes {
		got = append(got, m.Key)
	}
	want := []string{"default", "auto-edit", "plan", "yolo"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PermissionModes keys = %v, want %v", got, want)
	}
}

func contains(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}
