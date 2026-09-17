package core

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

const testAuthURL = "https://claude.com/cai/oauth/authorize?code=true&response_type=code&code_challenge=challenge&code_challenge_method=S256&state=state"

type fakeAuthProcess struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
	writes []string
}

func newFakeAuthProcess(ctx context.Context) *fakeAuthProcess {
	r, w := io.Pipe()
	p := &fakeAuthProcess{r: r, w: w, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			p.finish(ctx.Err())
		case <-p.done:
		}
	}()
	return p
}

func (p *fakeAuthProcess) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *fakeAuthProcess) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, string(b))
	p.mu.Unlock()
	return len(b), nil
}
func (p *fakeAuthProcess) Close() error { return p.r.Close() }
func (p *fakeAuthProcess) Wait() error {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
func (p *fakeAuthProcess) finish(err error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		_ = p.w.Close()
		close(p.done)
	})
}

func testAuthManager(timeout time.Duration) (*AgentAuthManager, <-chan *fakeAuthProcess) {
	m := NewAgentAuthManager(AgentAuthConfig{Enabled: true, AllowedUserIDs: []string{"admin"}, Timeout: timeout}, nil)
	processes := make(chan *fakeAuthProcess, 1)
	m.start = func(ctx context.Context) (authProcess, error) {
		p := newFakeAuthProcess(ctx)
		processes <- p
		go func() {
			_, _ = io.WriteString(p.w, "Opening browser\n\x1b]8;;"+testAuthURL+"\a"+testAuthURL+"\x1b]8;;\a\nPaste code here if prompted > ")
		}()
		return p, nil
	}
	m.probe = func(context.Context) (bool, error) { return true, nil }
	return m, processes
}

func waitAuthUpdate(t *testing.T, updates <-chan authUpdate, state string) authUpdate {
	t.Helper()
	select {
	case update := <-updates:
		if update.state != state {
			t.Fatalf("update = %#v, want %s", update, state)
		}
		return update
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", state)
		return authUpdate{}
	}
}

func TestClaudeAuthorizationURL(t *testing.T) {
	if got := claudeAuthorizationURL("\x1b]8;;" + testAuthURL + "\aurl\x1b]8;;\a"); got != testAuthURL {
		t.Fatalf("authorization URL = %q", got)
	}
	for _, bad := range []string{
		strings.Replace(testAuthURL, "claude.com", "evil.example", 1),
		strings.Replace(testAuthURL, "claude.com", "claude.com.evil.example", 1),
		strings.Replace(testAuthURL, "claude.com", "user@claude.com", 1),
		strings.Replace(testAuthURL, "https://", "http://", 1),
		strings.Replace(testAuthURL, "S256", "plain", 1),
		strings.Replace(testAuthURL, "code=true", "code=false", 1),
	} {
		if got := claudeAuthorizationURL(bad); got != "" {
			t.Errorf("accepted untrusted login URL %q", bad)
		}
	}
}

func TestAgentAuthOwnerIsolationAndSuccess(t *testing.T) {
	m, processes := testAuthManager(time.Second)
	owner := authOwner{"project", "private:admin", "admin"}
	updates := make(chan authUpdate, 4)
	refreshed := make(chan struct{})
	m.refresh = func() { close(refreshed) }
	if err := m.begin(context.Background(), owner, func(u authUpdate) { updates <- u }); err != nil {
		t.Fatal(err)
	}
	p := <-processes
	if update := waitAuthUpdate(t, updates, "link"); update.url != testAuthURL {
		t.Fatalf("URL = %q", update.url)
	}
	for _, other := range []authOwner{
		{"other-project", owner.SessionKey, owner.UserID},
		{owner.Project, "other-chat", owner.UserID},
		{owner.Project, owner.SessionKey, "intruder"},
	} {
		if err := m.submit(other, "secret-code#state"); err == nil {
			t.Fatalf("accepted code from %#v", other)
		}
		if err := m.cancel(other); err == nil {
			t.Fatalf("accepted cancellation from %#v", other)
		}
	}
	if err := m.begin(context.Background(), owner, func(authUpdate) {}); err == nil {
		t.Fatal("accepted concurrent login")
	}
	for _, bad := range []string{"sk-ant-oat01-token", "code\ncommand", "code\rcommand", "code with spaces", "", strings.Repeat("x", 2049)} {
		if err := m.submit(owner, bad); err == nil {
			t.Fatalf("accepted invalid code %q", bad)
		}
	}
	if err := m.submit(owner, "secret-code#state"); err != nil {
		t.Fatal(err)
	}
	if err := m.submit(owner, "secret-code#state"); err == nil {
		t.Fatal("accepted replayed code")
	}
	p.mu.Lock()
	writes := append([]string(nil), p.writes...)
	p.mu.Unlock()
	if len(writes) != 1 || writes[0] != "secret-code#state\n" {
		t.Fatalf("CLI writes = %#v", writes)
	}
	p.finish(nil)
	waitAuthUpdate(t, updates, "auth_success")
	select {
	case <-refreshed:
	default:
		t.Fatal("successful login did not refresh existing processes")
	}
	if m.pending(owner) {
		t.Fatal("completed login is still pending")
	}
}

func TestAgentAuthCancellationTimeoutAndRevocation(t *testing.T) {
	for _, action := range []string{"cancel", "timeout", "revoke"} {
		t.Run(action, func(t *testing.T) {
			timeout := time.Second
			if action == "timeout" {
				timeout = 100 * time.Millisecond
			}
			m, _ := testAuthManager(timeout)
			owner := authOwner{"project", "chat", "admin"}
			updates := make(chan authUpdate, 4)
			if err := m.begin(context.Background(), owner, func(u authUpdate) { updates <- u }); err != nil {
				t.Fatal(err)
			}
			waitAuthUpdate(t, updates, "link")
			want := "auth_cancelled"
			switch action {
			case "cancel":
				if err := m.cancel(owner); err != nil {
					t.Fatal(err)
				}
			case "revoke":
				m.ApplyConfig(AgentAuthConfig{})
			case "timeout":
				want = "auth_timeout"
			}
			waitAuthUpdate(t, updates, want)
			if m.pending(owner) {
				t.Fatal("terminated login remains pending")
			}
		})
	}
}

func TestAgentAuthFailedLoginNeverRefreshes(t *testing.T) {
	for _, mode := range []string{"exit", "logged-out", "status-error"} {
		t.Run(mode, func(t *testing.T) {
			m, processes := testAuthManager(time.Second)
			m.refresh = func() { t.Error("failed login refreshed sessions") }
			switch mode {
			case "logged-out":
				m.probe = func(context.Context) (bool, error) { return false, nil }
			case "status-error":
				m.probe = func(context.Context) (bool, error) { return false, errors.New("secret response") }
			}
			updates := make(chan authUpdate, 4)
			if err := m.begin(context.Background(), authOwner{"p", "c", "admin"}, func(u authUpdate) { updates <- u }); err != nil {
				t.Fatal(err)
			}
			p := <-processes
			waitAuthUpdate(t, updates, "link")
			var err error
			if mode == "exit" {
				err = errors.New("failed with secret")
			}
			p.finish(err)
			waitAuthUpdate(t, updates, "auth_failed")
		})
	}
}

type authTestPlatform struct {
	mu   sync.Mutex
	sent []string
}

func (p *authTestPlatform) Name() string               { return "auth-test" }
func (p *authTestPlatform) Start(MessageHandler) error { return nil }
func (p *authTestPlatform) Stop() error                { return nil }
func (p *authTestPlatform) IsPrivateChat(ctx any) bool { return ctx == "private" }
func (p *authTestPlatform) Send(_ context.Context, _ any, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, text)
	return nil
}
func (p *authTestPlatform) Reply(ctx context.Context, rc any, text string) error {
	return p.Send(ctx, rc, text)
}

type noRoutingAuthAgent struct{ stubAgent }

func (a *noRoutingAuthAgent) Name() string { return "claudecode" }
func (a *noRoutingAuthAgent) StartSession(context.Context, string) (AgentSession, error) {
	panic("credential command must not reach Agent")
}

func TestAuthCommandsNeverReachPendingEditsOrHistory(t *testing.T) {
	p := &authTestPlatform{}
	e := NewEngine("project", &noRoutingAuthAgent{}, []Platform{p}, "", LangEnglish)
	m, _ := testAuthManager(time.Second)
	e.SetAgentAuthManager(m)
	e.pendingCronEdits["chat"] = &pendingCronPromptEdit{JobID: "job"}
	for _, command := range []string{"/auth code secret-code#state", "/auth code sk-ant-oat01-token", "/au code secret-code#state"} {
		msg := &Message{SessionKey: "chat", UserID: "admin", Content: command, ReplyCtx: "private"}
		e.handleMessage(p, msg)
		if msg.Content != "" {
			t.Fatal("credential-bearing message was retained")
		}
	}
	if len(e.sessions.ListSessions("chat")) != 0 {
		t.Fatal("auth commands created a conversation session")
	}
	if e.pendingCronEdits["chat"] == nil {
		t.Fatal("auth command was consumed as cron prompt edit")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, text := range p.sent {
		if strings.Contains(text, "secret-code") || strings.Contains(text, "sk-ant-") {
			t.Fatalf("credential leaked in response: %q", text)
		}
	}
}

func TestAuthCommandsRequireAdminPrivateChat(t *testing.T) {
	for _, mode := range []string{"disabled", "intruder", "group", "forward", "quote", "attachment", "wrong-agent"} {
		t.Run(mode, func(t *testing.T) {
			p := &authTestPlatform{}
			e := NewEngine("project", &noRoutingAuthAgent{}, []Platform{p}, "", LangEnglish)
			m, _ := testAuthManager(time.Second)
			m.start = func(context.Context) (authProcess, error) {
				t.Error("forbidden login started")
				return nil, errors.New("forbidden")
			}
			e.SetAgentAuthManager(m)
			msg := &Message{SessionKey: "chat", UserID: "admin", Content: "/auth login", ReplyCtx: "private"}
			switch mode {
			case "disabled":
				m.ApplyConfig(AgentAuthConfig{})
			case "intruder":
				msg.UserID = "intruder"
			case "group":
				msg.ReplyCtx = "group"
			case "forward":
				msg.Forwarded = true
			case "quote":
				msg.QuotedContent = "quoted"
			case "attachment":
				msg.Files = []FileAttachment{{FileName: "code.txt"}}
			case "wrong-agent":
				e.agent = &stubAgent{}
			}
			e.handleMessage(p, msg)
		})
	}
}

func TestRedactAuthCommand(t *testing.T) {
	for _, text := range []string{"/auth code secret", "/auth@bot code secret", "/au code secret", "/AUTH code secret", "@bot /auth code secret", "@bot /auth@bot code secret"} {
		if strings.Contains(RedactAuthCommand(text), "secret") {
			t.Fatalf("credential not redacted: %q", text)
		}
	}
	if got := RedactAuthCommand("ordinary message"); got != "ordinary message" {
		t.Fatalf("ordinary message changed: %q", got)
	}
}

func TestAuthPendingPlainCodeIsNotRouted(t *testing.T) {
	p := &authTestPlatform{}
	e := NewEngine("project", &noRoutingAuthAgent{}, []Platform{p}, "", LangEnglish)
	m, _ := testAuthManager(time.Second)
	e.SetAgentAuthManager(m)
	updates := make(chan authUpdate, 4)
	owner := authOwner{"project", "chat", "admin"}
	if err := m.begin(context.Background(), owner, func(u authUpdate) { updates <- u }); err != nil {
		t.Fatal(err)
	}
	waitAuthUpdate(t, updates, "link")
	msg := &Message{SessionKey: "chat", UserID: "admin", Content: "raw-secret-code#state", ReplyCtx: "private"}
	e.handleMessage(p, msg)
	if msg.Content != "" || len(e.sessions.ListSessions("chat")) != 0 {
		t.Fatal("plain authorization code was retained or routed")
	}
	if err := m.cancel(owner); err != nil {
		t.Fatal(err)
	}
	waitAuthUpdate(t, updates, "auth_cancelled")
}

func TestAuthRefreshPreservesSessionAndHistory(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("chat")
	session.AgentSessionID = "existing-transcript"
	session.AddHistory("user", "keep this history")
	e.interactiveStates["chat"] = newInteractiveState(&stubAgentSession{}, nil, nil, false)
	e.RefreshInteractiveAuth()
	if !e.interactiveStates["chat"].authStale || e.sessions.ActiveSessionID("chat") != session.ID ||
		session.AgentSessionID != "existing-transcript" || session.HistoryLen() != 1 {
		t.Fatal("refresh must mark live state without clearing history or bindings")
	}
	old := e.interactiveStates["chat"]
	next := e.getOrCreateInteractiveState("chat", nil, nil, session)
	if next == old || next.authStale {
		t.Fatal("next turn reused stale credentials")
	}
}

func TestAuthQuotedCredentialsRedactedWithoutPendingLogin(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	for _, quote := range []string{
		"/auth code secret-code#state", "/auth code sk-ant-secret-token",
		"@bot /auth code secret-code#state", "old message\n/auth code secret-code#state",
	} {
		msg := &Message{Content: "explain this", QuotedContent: quote}
		if e.handleAuthMessage(nil, msg) {
			t.Fatal("ordinary reply should remain routable")
		}
		session := e.sessions.GetOrCreateActive("chat")
		prompt := e.buildAgentPrompt(session, msg)
		session.AddHistory("user", prompt)
		if strings.Contains(prompt, "secret") || strings.Contains(msg.QuotedContent, "secret") {
			t.Fatal("quoted credentials reached prompt/history")
		}
	}
}

func TestAuthRefreshAllowsRunningReplyToFinish(t *testing.T) {
	e := NewEngine("project", &stubAgent{}, nil, "", LangEnglish)
	p := &authTestPlatform{}
	session := e.sessions.GetOrCreateActive("chat")
	agentSession := &eventfulStubAgentSession{events: make(chan Event, 1)}
	state := newInteractiveState(agentSession, p, "private", false)
	e.interactiveStates["chat"] = state
	done := make(chan struct{})
	go func() {
		e.processInteractiveEvents(state, session, "chat", "message", time.Now(), nil)
		close(done)
	}()
	e.RefreshInteractiveAuth()
	select {
	case <-state.stopped:
		t.Fatal("refresh interrupted an active turn")
	default:
	}
	agentSession.events <- Event{Type: EventResult, Content: "completed reply"}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reply did not finish after refresh")
	}
	if session.HistoryLen() == 0 || session.GetHistory(1)[0].Content != "completed reply" {
		t.Fatal("running reply was silently lost")
	}
	if next := e.getOrCreateInteractiveState("chat", p, "private", session); next == state {
		t.Fatal("next turn reused stale process")
	}
}

type authRefreshTestAgent struct {
	stubAgent
	started chan *authRefreshTestSession
}

func (a *authRefreshTestAgent) StartSession(context.Context, string) (AgentSession, error) {
	s := &authRefreshTestSession{
		events: make(chan Event, 1), sent: make(chan string, 1), closed: make(chan struct{}),
	}
	a.started <- s
	return s, nil
}

type authRefreshTestSession struct {
	stubAgentSession
	events chan Event
	sent   chan string
	closed chan struct{}
	once   sync.Once
}

func (s *authRefreshTestSession) Send(prompt string, _ []ImageAttachment, _ []FileAttachment) error {
	s.sent <- prompt
	return nil
}
func (s *authRefreshTestSession) Events() <-chan Event { return s.events }
func (s *authRefreshTestSession) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestAuthRefreshReplacesProcessBeforeQueuedTurn(t *testing.T) {
	a := &authRefreshTestAgent{started: make(chan *authRefreshTestSession, 2)}
	p := &authTestPlatform{}
	e := NewEngine("project", a, []Platform{p}, "", LangEnglish)
	defer e.cancel()
	receiveStarted := func() *authRefreshTestSession {
		t.Helper()
		select {
		case s := <-a.started:
			return s
		case <-time.After(3 * time.Second):
			t.Fatal("turn did not start")
			return nil
		}
	}
	receivePrompt := func(s *authRefreshTestSession, want string) {
		t.Helper()
		select {
		case got := <-s.sent:
			if got != want {
				t.Fatalf("prompt = %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("turn did not send prompt")
		}
	}
	e.handleMessage(p, &Message{SessionKey: "chat", Platform: p.Name(), Content: "first", ReplyCtx: "private"})
	old := receiveStarted()
	receivePrompt(old, "first")
	e.handleMessage(p, &Message{SessionKey: "chat", Platform: p.Name(), Content: "queued", ReplyCtx: "private"})
	e.RefreshInteractiveAuth()
	select {
	case <-old.closed:
		t.Fatal("refresh closed active process")
	default:
	}
	old.events <- Event{Type: EventResult, Content: "first reply"}
	next := receiveStarted()
	select {
	case <-old.closed:
	default:
		t.Fatal("queue started before retiring stale process")
	}
	receivePrompt(next, "queued")
	p.mu.Lock()
	firstDelivered := false
	for _, reply := range p.sent {
		firstDelivered = firstDelivered || reply == "first reply"
	}
	p.mu.Unlock()
	if !firstDelivered {
		t.Fatal("first reply was lost before queue startup")
	}
	next.events <- Event{Type: EventResult, Content: "queued reply"}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.promptQueueMu.Lock()
		finished := len(e.promptQueues) == 0
		e.promptQueueMu.Unlock()
		if finished {
			e.cleanupInteractiveState("chat")
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queued turn did not complete")
}

func TestAuthPTYHelper(t *testing.T) {
	if os.Getenv("ECHO_AUTH_PTY_HELPER") != "1" {
		return
	}
	fmt.Printf("%s\nPaste code here if prompted > ", testAuthURL)
	code, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil || strings.TrimSpace(code) != "test-code#state" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestAgentAuthRealPTYLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY login is unavailable on Windows")
	}
	for _, action := range []string{"success", "cancel", "timeout"} {
		t.Run(action, func(t *testing.T) {
			timeout := 5 * time.Second
			if action == "timeout" {
				timeout = 500 * time.Millisecond
			}
			m, _ := testAuthManager(timeout)
			m.start = func(ctx context.Context) (authProcess, error) {
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAuthPTYHelper$")
				cmd.Env = MergeEnv(os.Environ(), []string{"ECHO_AUTH_PTY_HELPER=1", "GORACE=atexit_sleep_ms=0"})
				file, err := pty.Start(cmd)
				if err != nil {
					return nil, err
				}
				return &claudeLoginProcess{File: file, cmd: cmd}, nil
			}
			owner := authOwner{"project", "chat", "admin"}
			updates := make(chan authUpdate, 4)
			if err := m.begin(context.Background(), owner, func(u authUpdate) { updates <- u }); err != nil {
				t.Fatal(err)
			}
			waitAuthUpdate(t, updates, "link")
			want := "auth_success"
			switch action {
			case "success":
				if err := m.submit(owner, "test-code#state"); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				if err := m.cancel(owner); err != nil {
					t.Fatal(err)
				}
				want = "auth_cancelled"
			case "timeout":
				want = "auth_timeout"
			}
			waitAuthUpdate(t, updates, want)
		})
	}
}

func TestAgentAuthEnvironmentOverride(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "test-token")
	m, _ := testAuthManager(time.Second)
	if err := m.begin(context.Background(), authOwner{"p", "c", "admin"}, func(authUpdate) {}); err == nil || err.Error() != "auth_override" {
		t.Fatalf("environment override error = %v", err)
	}
}
