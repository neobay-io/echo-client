package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// P2 review 7: a PTY read can end inside the code; only complete lines count.
func TestDeviceCodePromptWaitsForCompleteLines(t *testing.T) {
	prompt := defaultAuthDrivers()["codex"].prompt
	cut := strings.Index(codexDeviceOutput, "9\x1b[0m")
	if _, ok := prompt(codexDeviceOutput[:cut]); ok {
		t.Fatal("accepted a code cut at the read boundary")
	}
	if got, ok := prompt(codexDeviceOutput); !ok || got.UserCode != "ABCD-EFGH9" {
		t.Fatalf("prompt = %#v, %v", got, ok)
	}
	kimi := defaultAuthDrivers()["kimi"].prompt
	if _, ok := kimi("Open https://www.kimi.com/device?user_code=WXYZ-12"); ok {
		t.Fatal("accepted a URL cut at the read boundary")
	}
}

// P2 review 8: a user_code from the URL is shown in chat, so it must be a
// plain code.
func TestDeviceCodeRejectsSmuggledUserCode(t *testing.T) {
	kimi := defaultAuthDrivers()["kimi"].prompt
	for _, bad := range []string{
		"https://www.kimi.com/device?user_code=ABCD-EFGH%0Ahttps%3A%2F%2Fevil.example\n",
		"https://www.kimi.com/device?user_code=%3Cb%3Ehi%3C%2Fb%3E\n",
	} {
		if got, ok := kimi(bad); ok {
			t.Errorf("accepted %q: %#v", bad, got)
		}
	}
}

// P2 review 4: a logout and a login never run at the same time.
func TestLoginWaitsForRunningLogout(t *testing.T) {
	m, _ := testAuthManager(5 * time.Second)
	release := make(chan struct{})
	entered := make(chan struct{})
	m.drivers["codex"].logout = func(context.Context) error {
		close(entered)
		<-release
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- m.logout(context.Background(), "codex", "admin") }()
	<-entered
	if err := m.begin(context.Background(), "claudecode", authOwner{"p", "c", "admin"}, func(authUpdate) {}); err == nil || err.Error() != "auth_busy" {
		t.Fatalf("login during logout = %v", err)
	}
	if err := m.logout(context.Background(), "claudecode", "admin"); err == nil || err.Error() != "auth_busy" {
		t.Fatalf("second logout = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// P2 review 9: Kimi Code asks for a region interactively; chat login needs
// one, stored or named.
func TestKimiLoginRegion(t *testing.T) {
	dir := t.TempDir()
	prev := kimiHomeDir
	kimiHomeDir = func() string { return dir }
	defer func() { kimiHomeDir = prev }()
	if _, err := defaultAuthDrivers()["kimi"].start(context.Background(), authLoginOptions{}); err == nil || err.Error() != "auth_region_required" {
		t.Fatalf("start without a region = %v", err)
	}

	p := &authTestPlatform{}
	e := NewEngine("project", &namedStubAgent{name: "gemini"}, []Platform{p}, "", LangEnglish)
	m, _ := testAuthManager(5 * time.Second)
	e.SetAgentAuthManager(m)
	var regions []string
	m.drivers["kimi"].start = func(_ context.Context, opts authLoginOptions) (authProcess, error) {
		regions = append(regions, opts.Region)
		if opts.Region == "" {
			return nil, errors.New("auth_region_required")
		}
		return nil, errors.New("test: stop here")
	}
	e.handleMessage(p, &Message{SessionKey: "chat", UserID: "admin", Content: "/auth login kimi", ReplyCtx: "private"})
	e.handleMessage(p, &Message{SessionKey: "chat", UserID: "admin", Content: "/auth login kimi global", ReplyCtx: "private"})
	p.mu.Lock()
	sent := strings.Join(p.sent, "\n")
	p.mu.Unlock()
	if !strings.Contains(sent, "/auth login kimi mainland-cn") || strings.Join(regions, ",") != ",global" {
		t.Fatalf("replies = %q, regions = %v", sent, regions)
	}
	if _, _, _, _, _, ok := parseAuthArgsWithRegion(strings.Fields("login codex global")); ok {
		t.Fatal("accepted a region for codex")
	}
}

// P2 review 10.
func TestKimiHomeDirHonorsEnv(t *testing.T) {
	t.Setenv("KIMI_CODE_HOME", "/opt/kimi-home")
	if got := kimiHomeDir(); got != "/opt/kimi-home" {
		t.Fatalf("kimiHomeDir = %q", got)
	}
}

// P2 review 13: a prerelease is below its release.
func TestAtOrAboveVersionPrereleases(t *testing.T) {
	for _, tc := range []struct {
		installed, latest string
		want              bool
	}{
		{"codex-cli 0.169.1", "0.169.1", true},
		{"0.170.0", "0.169.1", true},
		{"0.169.0", "0.169.1", false},
		{"1.2.3-rc1", "1.2.3", false},
		{"1.2.3", "1.2.3-rc1", true},
		{"1.2.3-rc1", "1.2.3-rc1", true},
		{"1.2.3-rc1", "1.2.3-rc2", false},
		{"1.2.4-rc1", "1.2.3", true},
		{"1.2.3+build5", "1.2.3", true},
		{"", "1.2.3", false},
	} {
		if got := atOrAboveVersion(tc.installed, tc.latest); got != tc.want {
			t.Errorf("atOrAboveVersion(%q, %q) = %v, want %v", tc.installed, tc.latest, got, tc.want)
		}
	}
}

// P2 review 11: a message whose start meets an upgrade keeps its place in the
// queue instead of being turned away.
func TestTurnMeetingAnUpgradeIsRequeued(t *testing.T) {
	prevPoll := agentUpgradePollInterval
	agentUpgradePollInterval = 10 * time.Millisecond
	defer func() { agentUpgradePollInterval = prevPoll }()

	sent := make(chan string, 4)
	session := &jobTestSession{events: make(chan Event, 4)}
	session.onSend = func(prompt string) {
		sent <- prompt
		session.events <- Event{Type: EventResult, Content: "done", SessionID: "s1"}
	}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("test", &codexNamedAgent{jobTestAgent{session: session}}, []Platform{p}, "", LangEnglish)
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{Enabled: true})
	e.SetAgentUpgradeManager(mgr)

	s := e.sessions.GetOrCreateActive("tg:chat:user")
	if !s.TryLock() {
		t.Fatal("lock")
	}
	mgr.beginTargetRun("codex") // starts after the message was accepted
	msg := &Message{SessionKey: "tg:chat:user", Platform: "telegram", UserID: "user", Content: "keep me", ReplyCtx: "ctx"}
	e.processInteractiveMessage(p, msg, s)
	if n := len(s.GetHistory(0)); n != 0 {
		t.Fatalf("history = %d entries, want none before the turn runs", n)
	}
	select {
	case prompt := <-sent:
		t.Fatalf("sent %q during the upgrade", prompt)
	case <-time.After(100 * time.Millisecond):
	}
	mgr.endTargetRun("codex")
	select {
	case prompt := <-sent:
		if !strings.Contains(prompt, "keep me") {
			t.Fatalf("prompt = %q", prompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("requeued message was not sent after the upgrade")
	}
}

// P2 review K8: a login that finishes at the end of its window is still
// checked on a full deadline.
func TestLoginStatusProbeGetsAFreshDeadline(t *testing.T) {
	m, processes := testAuthManager(400 * time.Millisecond)
	m.drivers["claudecode"].status = func(ctx context.Context) (authStatus, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return authStatus{LoggedIn: true}, nil
		case <-ctx.Done():
			return authStatus{}, ctx.Err()
		}
	}
	updates := make(chan authUpdate, 4)
	if err := m.begin(context.Background(), "claudecode", authOwner{"p", "c", "admin"}, func(u authUpdate) { updates <- u }); err != nil {
		t.Fatal(err)
	}
	p := <-processes
	waitAuthUpdate(t, updates, "link")
	time.Sleep(250 * time.Millisecond)
	p.finish(nil) // the CLI completes with ~150 ms of the window left
	select {
	case u := <-updates:
		if u.state != "auth_success" {
			t.Fatalf("state = %s", u.state)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no result")
	}
}
