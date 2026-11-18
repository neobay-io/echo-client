package core

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const codexDeviceOutput = "\x1b[1mWelcome to Codex\x1b[0m\nFollow these steps to sign in with ChatGPT using device code authorization:\n\n" +
	"1. Open this link in your browser and sign in to your account\n   \x1b[94mhttps://auth.openai.com/codex/device\x1b[0m\n\n" +
	"2. Enter this one-time code \x1b[90m(expires in 15 minutes)\x1b[0m\n   \x1b[94mABCD-EFGH9\x1b[0m\n"

func TestAuthDriverPrompts(t *testing.T) {
	drivers := defaultAuthDrivers()
	cases := []struct {
		agent, output string
		want          authPrompt
		ok            bool
	}{
		{"codex", codexDeviceOutput, authPrompt{URL: "https://auth.openai.com/codex/device", UserCode: "ABCD-EFGH9"}, true},
		// The URL alone is not enough while the code has not been printed.
		{"codex", "1. Open this link\n   https://auth.openai.com/codex/device\n", authPrompt{}, false},
		{"codex", strings.Replace(codexDeviceOutput, "auth.openai.com", "auth.openai.com.evil.example", 1), authPrompt{}, false},
		{"codex", strings.Replace(codexDeviceOutput, "https://auth.openai.com", "http://auth.openai.com", 1), authPrompt{}, false},
		{"kimi", "Open https://www.kimi.com/code/authorize_device?user_code=WXYZ-1234 to authorize.\n",
			authPrompt{URL: "https://www.kimi.com/code/authorize_device?user_code=WXYZ-1234", UserCode: "WXYZ-1234"}, true},
		{"kimi", "Visit https://kimi.com.evil.example/device?user_code=WXYZ-1234\n", authPrompt{}, false},
		{"antigravity", "Starting OAuth authentication flow\nGo to https://accounts.google.com/o/oauth2/v2/auth?client_id=x&state=y\nThen paste the authorization code from the page: ",
			authPrompt{URL: "https://accounts.google.com/o/oauth2/v2/auth?client_id=x&state=y", NeedsCode: true}, true},
		{"antigravity", "Go to https://accounts.google.com/o/oauth2/v2/auth?client_id=x\n", authPrompt{}, false},
		{"antigravity", "https://evil.example/o/oauth2?x\nThen paste the authorization code from the page: ", authPrompt{}, false},
	}
	for _, tc := range cases {
		got, ok := drivers[tc.agent].prompt(tc.output)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s prompt(%q) = %#v, %v; want %#v, %v", tc.agent, tc.output, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseAuthArgs(t *testing.T) {
	type res struct {
		action, agent, code string
		confirm, ok         bool
	}
	cases := map[string]res{
		"":                        {"status", "", "", false, true},
		"status agy":              {"status", "antigravity", "", false, true},
		"login codex":             {"login", "codex", "", false, true},
		"LOGIN Kimi":              {"login", "kimi", "", false, true},
		"logout claude confirm":   {"logout", "claudecode", "", true, true},
		"logout confirm":          {"logout", "", "", true, true},
		"code 4/0AbC-d_e":         {"code", "", "4/0AbC-d_e", false, true},
		"code":                    {"code", "", "", false, false},
		"cancel":                  {"cancel", "", "", false, true},
		"cancel codex":            {"cancel", "", "", false, false},
		"login gemini":            {"login", "", "", false, false},
		"login codex kimi":        {"login", "", "", false, false},
		"status confirm":          {"status", "", "", false, false},
		"frobnicate":              {"frobnicate", "", "", false, false},
		"logout codex confirm no": {"logout", "", "", false, false},
	}
	for in, want := range cases {
		action, agent, code, confirm, ok := parseAuthArgs(strings.Fields(in))
		if got := (res{action, agent, code, confirm, ok}); got != want {
			t.Errorf("parseAuthArgs(%q) = %#v, want %#v", in, got, want)
		}
	}
}

// fakeDeviceLogin wires the codex driver to a fake process printing the
// device-code prompt.
func fakeDeviceLogin(t *testing.T, m *AgentAuthManager, loggedIn bool) <-chan *fakeAuthProcess {
	t.Helper()
	processes := make(chan *fakeAuthProcess, 1)
	d := m.drivers["codex"]
	d.start = func(ctx context.Context, _ authLoginOptions) (authProcess, error) {
		p := newFakeAuthProcess(ctx)
		processes <- p
		go func() { _, _ = io.WriteString(p.w, codexDeviceOutput) }()
		return p, nil
	}
	d.status = func(context.Context) (authStatus, error) { return authStatus{LoggedIn: loggedIn}, nil }
	return processes
}

// P2: Codex logs in with a device code; nothing is pasted back.
func TestDeviceCodeLoginFlow(t *testing.T) {
	m, _ := testAuthManager(5 * time.Second)
	processes := fakeDeviceLogin(t, m, true)
	var refreshed []string
	m.refresh = func(agent string) { refreshed = append(refreshed, agent) }
	owner := authOwner{"p", "private:admin", "admin"}
	updates := make(chan authUpdate, 4)
	if err := m.begin(context.Background(), "codex", owner, func(u authUpdate) { updates <- u }); err != nil {
		t.Fatal(err)
	}
	p := <-processes
	u := waitAuthUpdate(t, updates, "auth_device")
	if u.agent != "Codex" || u.prompt.URL != "https://auth.openai.com/codex/device" || u.prompt.UserCode != "ABCD-EFGH9" {
		t.Fatalf("update = %#v", u)
	}
	if err := m.submit(owner, "ABCD-EFGH9"); err == nil || err.Error() != "auth_code_not_needed" {
		t.Fatalf("submit = %v", err)
	}
	if label := m.pendingLabel(owner); label != "Codex" {
		t.Fatalf("pending label = %q", label)
	}
	p.finish(nil)
	waitAuthUpdate(t, updates, "auth_success")
	if len(refreshed) != 1 || refreshed[0] != "codex" {
		t.Fatalf("refreshed = %v", refreshed)
	}
}

// P2: /auth names an agent; status, login, and a confirmed logout reach that
// agent's driver from any project.
func TestAuthCommandsPerAgent(t *testing.T) {
	p := &authTestPlatform{}
	e := NewEngine("project", &namedStubAgent{name: "gemini"}, []Platform{p}, "", LangEnglish)
	m, _ := testAuthManager(5 * time.Second)
	e.SetAgentAuthManager(m)
	var mu sync.Mutex
	var loggedOut []string
	m.drivers["kimi"].status = func(context.Context) (authStatus, error) { return authStatus{LoggedIn: true, Detail: "OAuth"}, nil }
	m.drivers["codex"].status = func(context.Context) (authStatus, error) { return authStatus{}, nil }
	m.drivers["claudecode"].status = func(context.Context) (authStatus, error) {
		return authStatus{LoggedIn: true, Detail: "claude.ai, max", Expires: time.Date(2027, 1, 2, 3, 4, 0, 0, time.Local)}, nil
	}
	m.drivers["codex"].logout = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		loggedOut = append(loggedOut, "codex")
		return nil
	}
	var refreshed []string
	m.refresh = func(agent string) { refreshed = append(refreshed, agent) }

	send := func(content string) string {
		p.mu.Lock()
		p.sent = nil
		p.mu.Unlock()
		e.handleMessage(p, &Message{SessionKey: "chat", UserID: "admin", Content: content, ReplyCtx: "private"})
		p.mu.Lock()
		defer p.mu.Unlock()
		return strings.Join(p.sent, "\n")
	}
	if got := send("/auth status kimi"); !strings.Contains(got, "Kimi Code CLI reports logged in (OAuth)") {
		t.Fatalf("kimi status = %q", got)
	}
	if got := send("/auth status codex"); !strings.Contains(got, "Codex CLI is not logged in. Use /auth login codex.") {
		t.Fatalf("codex status = %q", got)
	}
	if got := send("/auth status claude"); !strings.Contains(got, "(claude.ai, max, expires 2027-01-02 03:04)") {
		t.Fatalf("claude status = %q", got)
	}
	// The project's own agent (Gemini, no chat login) needs a name.
	if got := send("/auth status"); !strings.Contains(got, "not available for this agent") {
		t.Fatalf("default status = %q", got)
	}
	if got := send("/auth logout codex"); !strings.Contains(got, "/auth logout codex confirm") || len(loggedOut) != 0 {
		t.Fatalf("unconfirmed logout = %q, logged out %v", got, loggedOut)
	}
	if got := send("/auth logout codex confirm"); !strings.Contains(got, "Codex logged out") || len(loggedOut) != 1 ||
		len(refreshed) != 1 || refreshed[0] != "codex" {
		t.Fatalf("logout = %q, logged out %v, refreshed %v", got, loggedOut, refreshed)
	}
	if got := send("/auth logout kimi confirm"); !strings.Contains(got, "Kimi Code has no logout command") {
		t.Fatalf("kimi logout = %q", got)
	}
	// Not an authorized administrator: nothing runs.
	e.handleMessage(p, &Message{SessionKey: "chat", UserID: "intruder", Content: "/auth logout codex confirm", ReplyCtx: "private"})
	if len(loggedOut) != 1 {
		t.Fatalf("unauthorized logout ran: %v", loggedOut)
	}
}

func TestEngineRefreshesOnlyItsAgent(t *testing.T) {
	e := NewEngine("p", &namedStubAgent{name: "codex"}, nil, "", LangEnglish)
	e.interactiveStates["k"] = &interactiveState{}
	e.RefreshInteractiveAuthFor("claudecode")
	if e.interactiveStates["k"].stale {
		t.Fatal("claude login retired a codex process")
	}
	e.RefreshInteractiveAuthFor("codex")
	if !e.interactiveStates["k"].stale {
		t.Fatal("codex login did not retire the codex process")
	}
}

func TestKimiLoginStatusAndRegion(t *testing.T) {
	dir := t.TempDir()
	prev := kimiHomeDir
	kimiHomeDir = func() string { return dir }
	defer func() { kimiHomeDir = prev }()

	if st, err := kimiLoginStatus(context.Background()); err != nil || st.LoggedIn {
		t.Fatalf("no credentials: %#v, %v", st, err)
	}
	if kimiLoginRegion() != "" {
		t.Fatal("region without a region file")
	}
	_ = os.MkdirAll(filepath.Join(dir, "credentials"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "credentials", "kimi-code-env-1.json"), []byte(`{"access_token":"a","refresh_token":""}`), 0o600)
	if st, _ := kimiLoginStatus(context.Background()); st.LoggedIn {
		t.Fatal("empty refresh token counted as logged in")
	}
	_ = os.WriteFile(filepath.Join(dir, "credentials", "kimi-code-env-2.json"), []byte(`{"refresh_token":"r","expires_at":1}`), 0o600)
	if st, err := kimiLoginStatus(context.Background()); err != nil || !st.LoggedIn {
		t.Fatalf("logged in: %#v, %v", st, err)
	}
	for content, want := range map[string]string{"mainland-cn\n": "mainland-cn", "global": "global", "--evil": ""} {
		_ = os.WriteFile(filepath.Join(dir, "region"), []byte(content), 0o600)
		if got := kimiLoginRegion(); got != want {
			t.Errorf("region %q = %q, want %q", content, got, want)
		}
	}
}

func TestStripTerminalCodes(t *testing.T) {
	in := "\x1b[1mbold\x1b[0m \x1b]8;;https://x.example\x07link\x1b]8;;\x07 \x1b[2K\rdone"
	if got := stripTerminalCodes(in); got != "bold link \rdone" {
		t.Fatalf("stripTerminalCodes = %q", got)
	}
}

func TestAuthLogoutRefusesDuringLogin(t *testing.T) {
	m, _ := testAuthManager(5 * time.Second)
	processes := fakeDeviceLogin(t, m, true)
	m.drivers["codex"].logout = func(context.Context) error { return errors.New("must not run") }
	owner := authOwner{"p", "c", "admin"}
	if err := m.begin(context.Background(), "codex", owner, func(authUpdate) {}); err != nil {
		t.Fatal(err)
	}
	p := <-processes
	defer p.finish(nil)
	if err := m.logout(context.Background(), "codex", "admin"); err == nil || err.Error() != "auth_busy" {
		t.Fatalf("logout during login = %v", err)
	}
}
