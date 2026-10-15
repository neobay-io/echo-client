package gemini

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

type modelTransport func(*http.Request) (*http.Response, error)

func (f modelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAvailableModelsProtectsGeminiKeyAndPreservesOAuthChoices(t *testing.T) {
	const secret = "test-sensitive-provider-key"
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(logger) })
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	http.DefaultTransport = modelTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("x-goog-api-key") != secret || strings.Contains(r.URL.String(), secret) || r.URL.RawQuery != "" {
			t.Error("credential must be in the header only")
		}
		return nil, errors.New("synthetic transport failure " + secret)
	})
	a := &Agent{providers: []core.ProviderConfig{{APIKey: "overridden", Model: "provider-model", Env: map[string]string{"GEMINI_API_KEY": secret}}}}
	models := a.AvailableModels(context.Background())
	if strings.Contains(logs.String(), secret) {
		t.Fatal("credential leaked into debug log")
	}
	for _, want := range []string{"provider-model", "auto", "pro", "flash", "flash-lite"} {
		if !containsGeminiModel(models, want) {
			t.Errorf("missing fallback choice %s", want)
		}
	}
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	a.providers = nil
	if got := a.AvailableModels(context.Background()); len(got) < 4 {
		t.Fatalf("OAuth choices = %#v", got)
	}
}
