package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/chenhg5/cc-connect/core"
)

type buildJobManagerTestAgent struct{}

func (a *buildJobManagerTestAgent) Name() string { return "test-agent" }
func (a *buildJobManagerTestAgent) StartSession(
	_ context.Context,
	_ string,
) (core.AgentSession, error) {
	return &buildJobManagerTestSession{}, nil
}
func (a *buildJobManagerTestAgent) ListSessions(
	_ context.Context,
) ([]core.AgentSessionInfo, error) {
	return nil, nil
}
func (a *buildJobManagerTestAgent) Stop() error { return nil }

type testSkillAgent struct {
	buildJobManagerTestAgent
	dirs []string
}

func (a *testSkillAgent) SkillDirs() []string { return append([]string(nil), a.dirs...) }

type buildJobManagerTestSession struct{}

func (s *buildJobManagerTestSession) Send(_ string, _ []core.ImageAttachment, _ []core.FileAttachment) error {
	return nil
}
func (s *buildJobManagerTestSession) RespondPermission(
	_ string,
	_ core.PermissionResult,
) error {
	return nil
}
func (s *buildJobManagerTestSession) Events() <-chan core.Event {
	ch := make(chan core.Event, 1)
	ch <- core.Event{Type: core.EventResult, Content: "job finished"}
	close(ch)
	return ch
}
func (s *buildJobManagerTestSession) CurrentSessionID() string { return "session-1" }
func (s *buildJobManagerTestSession) Alive() bool              { return true }
func (s *buildJobManagerTestSession) Close() error             { return nil }

func TestBuildJobManagerRegistersProjectRunners(t *testing.T) {
	projects := []config.ProjectConfig{
		{Name: "proj-a"},
		{Name: "proj-b"},
	}
	engines := []*core.Engine{
		core.NewEngine("proj-a", &buildJobManagerTestAgent{}, nil, "", core.LangEnglish),
		core.NewEngine("proj-b", &buildJobManagerTestAgent{}, nil, "", core.LangEnglish),
	}

	jobMgr, err := buildJobManager(t.TempDir(), projects, engines)
	if err != nil {
		t.Fatalf("buildJobManager: %v", err)
	}

	job, err := jobMgr.Start(core.JobRequest{Project: "proj-b", Prompt: "run"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if job.Project != "proj-b" {
		t.Fatalf("unexpected project: %q", job.Project)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, ok := jobMgr.Get(job.ID)
		if ok && current.Status == core.JobStatusCompleted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not complete in time", job.ID)
}

func TestResolveProjectSkillDirsFallsBackToAgentDefaults(t *testing.T) {
	agent := &testSkillAgent{dirs: []string{"/default/a", "/default/b"}}
	got := resolveProjectSkillDirs(config.ProjectConfig{Name: "demo"}, agent)
	want := []string{"/default/a", "/default/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveProjectSkillDirs() = %#v, want %#v", got, want)
	}
}

func TestResolveProjectSkillDirsDedupesAgentDefaultsOnFallback(t *testing.T) {
	agent := &testSkillAgent{dirs: []string{"/default/a", " /default/a ", "/default/b"}}
	got := resolveProjectSkillDirs(config.ProjectConfig{Name: "demo"}, agent)
	want := []string{"/default/a", "/default/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveProjectSkillDirs() = %#v, want %#v", got, want)
	}
}

func TestResolveProjectSkillDirsOverridesAgentDefaultsByDefault(t *testing.T) {
	agent := &testSkillAgent{dirs: []string{"/default/a", "/default/b"}}
	got := resolveProjectSkillDirs(config.ProjectConfig{
		Name:      "demo",
		SkillDirs: []string{"/project/tester", "/project/shared"},
	}, agent)
	want := []string{"/project/tester", "/project/shared"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveProjectSkillDirs() = %#v, want %#v", got, want)
	}
}

func TestResolveProjectSkillDirsCanIncludeAgentDefaults(t *testing.T) {
	includeDefaults := true
	agent := &testSkillAgent{dirs: []string{"/default/a", "/default/b", "/project/shared"}}
	got := resolveProjectSkillDirs(config.ProjectConfig{
		Name:                    "demo",
		SkillDirs:               []string{"/project/tester", "/project/shared"},
		IncludeDefaultSkillDirs: &includeDefaults,
	}, agent)
	want := []string{"/project/tester", "/project/shared", "/default/a", "/default/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveProjectSkillDirs() = %#v, want %#v", got, want)
	}
}

func TestResolveConfigPathPrefersEchoClientButFallsBackToLegacy(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	legacyPath := filepath.Join(dir, ".cc-connect", "config.toml")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("MkdirAll legacy: %v", err)
	}
	if err := os.WriteFile(legacyPath, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile legacy: %v", err)
	}
	if got := resolveConfigPath(""); got != legacyPath {
		t.Fatalf("resolveConfigPath legacy = %q, want %q", got, legacyPath)
	}

	preferredPath := filepath.Join(dir, ".echo-client", "config.toml")
	if err := os.MkdirAll(filepath.Dir(preferredPath), 0o755); err != nil {
		t.Fatalf("MkdirAll preferred: %v", err)
	}
	if err := os.WriteFile(preferredPath, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile preferred: %v", err)
	}
	if got := resolveConfigPath(""); got != preferredPath {
		t.Fatalf("resolveConfigPath preferred = %q, want %q", got, preferredPath)
	}
}

func TestResolveSocketPathUsesCompatibleDefaultDataDir(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	preferredConfig := filepath.Join(dir, ".echo-client", "config.toml")
	if err := os.MkdirAll(filepath.Dir(preferredConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll preferred: %v", err)
	}
	if err := os.WriteFile(preferredConfig, []byte(minimalConfigForMainTests), 0o644); err != nil {
		t.Fatalf("WriteFile preferred config: %v", err)
	}
	if got := resolveSocketPath(""); got != filepath.Join(dir, ".echo-client", "run", "api.sock") {
		t.Fatalf("resolveSocketPath() = %q", got)
	}
}

func TestResolveSocketPathUsesLegacyDataDirWhenLegacyHomeConfigIsActive(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	if err := os.MkdirAll(filepath.Join(dir, ".echo-client"), 0o755); err != nil {
		t.Fatalf("MkdirAll preferred dir: %v", err)
	}

	legacyConfig := filepath.Join(dir, ".cc-connect", "config.toml")
	if err := os.MkdirAll(filepath.Dir(legacyConfig), 0o755); err != nil {
		t.Fatalf("MkdirAll legacy: %v", err)
	}
	if err := os.WriteFile(legacyConfig, []byte(minimalConfigForMainTests), 0o644); err != nil {
		t.Fatalf("WriteFile legacy config: %v", err)
	}
	if got := resolveSocketPath(""); got != filepath.Join(dir, ".cc-connect", "run", "api.sock") {
		t.Fatalf("resolveSocketPath() with legacy dir = %q", got)
	}
}

func TestResolveSocketPathUsesConfiguredDataDirFromDefaultConfig(t *testing.T) {
	origHome := os.Getenv("HOME")
	dir := t.TempDir()
	if err := os.Setenv("HOME", dir); err != nil {
		t.Fatalf("Setenv HOME: %v", err)
	}
	defer func() {
		if err := os.Setenv("HOME", origHome); err != nil {
			t.Fatalf("restore HOME: %v", err)
		}
	}()

	configPath := filepath.Join(dir, ".echo-client", "config.toml")
	customDataDir := filepath.Join(dir, "custom-data")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("MkdirAll config dir: %v", err)
	}
	content := "data_dir = \"" + customDataDir + "\"\n" + minimalConfigForMainTests
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}

	if got := resolveSocketPath(""); got != filepath.Join(customDataDir, "run", "api.sock") {
		t.Fatalf("resolveSocketPath() with configured data_dir = %q", got)
	}
}

const minimalConfigForMainTests = `
[[projects]]
name = "demo"

[projects.agent]
type = "claudecode"

[[projects.platforms]]
type = "telegram"
`
