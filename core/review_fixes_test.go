package core

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the P0 review round (Codex + Kimi Code).

func newPurgeEngine(t *testing.T, name string, agent Agent, cs *CronScheduler) (*Engine, *stubPlatformEngine) {
	t.Helper()
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine(name, agent, []Platform{p}, "", LangEnglish)
	if cs != nil {
		e.SetCronScheduler(cs)
		cs.RegisterEngine(name, e)
	}
	return e, p
}

func newTestCronScheduler(t *testing.T) *CronScheduler {
	t.Helper()
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewCronScheduler(store)
}

// inactive session whose transcript is "agent-x", plus an active one.
func addPurgeTarget(e *Engine, key, agentID string) *Session {
	target := e.sessions.NewSession(key, "target")
	target.AgentSessionID = agentID
	e.sessions.NewSession(key, "active")
	return target
}

func TestPurgeKeepsTranscriptUsedByCronJob(t *testing.T) {
	cs := newTestCronScheduler(t)
	agent := &optionsAgent{}
	e, p := newPurgeEngine(t, "proj", agent, cs)
	target := addPurgeTarget(e, "test:a", "agent-x")
	if err := cs.Store().Add(&CronJob{ID: "job1", Project: "proj", SessionKey: "test:a", CronExpr: "0 9 * * *", Prompt: "p", AgentSessionID: "agent-x"}); err != nil {
		t.Fatal(err)
	}

	e.cmdDelete(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{target.ID, "purge"})

	if len(agent.deleted) != 0 {
		t.Fatalf("transcript used by a cron job was deleted: %#v", agent.deleted)
	}
	if sent := strings.Join(p.sent, "\n"); !strings.Contains(sent, "cron job job1") {
		t.Fatalf("reply = %q, want the cron holder named", sent)
	}
}

func TestPurgeKeepsTranscriptUsedByAnotherProject(t *testing.T) {
	cs := newTestCronScheduler(t)
	agentA, agentB := &optionsAgent{}, &optionsAgent{}
	a, p := newPurgeEngine(t, "proj-a", agentA, cs)
	b, _ := newPurgeEngine(t, "proj-b", agentB, cs)
	target := addPurgeTarget(a, "test:a", "agent-x")
	b.sessions.NewSession("test:b", "attached").AgentSessionID = "agent-x"

	a.cmdDelete(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{target.ID, "purge"})

	if len(agentA.deleted) != 0 {
		t.Fatalf("transcript used by project proj-b was deleted")
	}
	if sent := strings.Join(p.sent, "\n"); !strings.Contains(sent, "project proj-b") {
		t.Fatalf("reply = %q", sent)
	}

	// A project running a different agent type does not share transcripts.
	other := &stubModelSwitchAgent{name: "gemini"}
	c, _ := newPurgeEngine(t, "proj-c", other, cs)
	c.sessions.NewSession("test:c", "x").AgentSessionID = "agent-y"
	target2 := addPurgeTarget(a, "test:a2", "agent-y")
	a.cmdDelete(p, &Message{SessionKey: "test:a2", ReplyCtx: "ctx"}, []string{target2.ID, "purge"})
	if len(agentA.deleted) != 1 || agentA.deleted[0] != "agent-y" {
		t.Fatalf("deleted = %#v, want agent-y (other agent type ignored)", agentA.deleted)
	}
}

type failingDeleteAgent struct{ optionsAgent }

func (a *failingDeleteAgent) DeleteSession(context.Context, string) error {
	return errors.New("disk on fire")
}

func TestPurgeFailureKeepsLocalRecordForRetry(t *testing.T) {
	e, p := newPurgeEngine(t, "proj", &failingDeleteAgent{}, nil)
	target := addPurgeTarget(e, "test:a", "agent-x")

	e.cmdDelete(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{target.ID, "purge"})

	if matched := matchLocalSession(e.localSessionList("test:a"), target.ID); matched == nil {
		t.Fatal("local record removed although the transcript deletion failed")
	}
	if sent := strings.Join(p.sent, "\n"); !strings.Contains(sent, "disk on fire") || !strings.Contains(sent, "retry") {
		t.Fatalf("reply = %q", sent)
	}
}

func TestPurgeRefusesMalformedAgentSessionIDs(t *testing.T) {
	agent := &optionsAgent{}
	e, p := newPurgeEngine(t, "proj", agent, nil)
	target := addPurgeTarget(e, "test:a", "../../etc/passwd")

	e.cmdDelete(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{target.ID, "purge"})

	if len(agent.deleted) != 0 {
		t.Fatalf("DeleteSession called with %#v", agent.deleted)
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\b`, "a..b", "*", "-x", strings.Repeat("a", 129)} {
		if ValidAgentSessionID(id) {
			t.Errorf("ValidAgentSessionID(%q) = true", id)
		}
	}
	for _, id := range []string{"51b25019-4af9-4a1d-9d3c-0123456789ab", "019fbb7f-b234-7a52-adc2-3172847e4bb2", "session_ce28cc35"} {
		if !ValidAgentSessionID(id) {
			t.Errorf("ValidAgentSessionID(%q) = false", id)
		}
	}
}

func TestSettingsCommandsWaitForSessionTransitions(t *testing.T) {
	agent := &optionsAgent{mode: "bypassPermissions"}
	e, p := newPurgeEngine(t, "proj", agent, nil)
	e.sessions.GetOrCreateActive("test:a")

	unlock := e.lockSessionTransition("test:a") // a /new or /switch in progress
	var done atomic.Bool
	go func() {
		e.cmdMode(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{"default"})
		done.Store(true)
	}()
	time.Sleep(50 * time.Millisecond)
	if done.Load() {
		unlock()
		t.Fatal("/mode did not wait for the session transition")
	}
	next := e.sessions.NewSession("test:a", "new") // what /new does under the lock
	unlock()
	deadline := time.Now().Add(2 * time.Second)
	for !done.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := next.Settings().Mode; got != "default" {
		t.Fatalf("new active session mode = %q, want default", got)
	}
}

type providerOptionsAgent struct {
	optionsAgent
	active *ProviderConfig
}

func (a *providerOptionsAgent) SetProviders([]ProviderConfig)      {}
func (a *providerOptionsAgent) SetActiveProvider(string) bool      { return true }
func (a *providerOptionsAgent) GetActiveProvider() *ProviderConfig { return a.active }
func (a *providerOptionsAgent) ListProviders() []ProviderConfig    { return nil }

func TestEffectiveModelFollowsStartPrecedence(t *testing.T) {
	agent := &providerOptionsAgent{optionsAgent: optionsAgent{model: "project"}, active: &ProviderConfig{Name: "p", Model: "provider-model"}}
	e, _ := newPurgeEngine(t, "proj", agent, nil)
	s := e.sessions.GetOrCreateActive("test:a")
	if got := e.effectiveModel(s); got != "provider-model" {
		t.Fatalf("effective model = %q, want provider-model", got)
	}
	s.UpdateSettings(func(o *AgentSessionOptions) { o.Model = "chat-model" })
	if got := e.effectiveModel(s); got != "chat-model" {
		t.Fatalf("effective model = %q, want chat-model", got)
	}
	agent.active = &ProviderConfig{Name: "p"} // provider without a model
	s.UpdateSettings(func(o *AgentSessionOptions) { o.Model = "" })
	if got := e.effectiveModel(s); got != "project" {
		t.Fatalf("effective model = %q, want project", got)
	}
}

func TestAuthRefusesDeclarationsWithoutImplementation(t *testing.T) {
	run := func(agent Agent, content string) (started bool, sent string) {
		p := &authTestPlatform{}
		e := NewEngine("project", &noRoutingAuthAgent{}, []Platform{p}, "", LangEnglish)
		m, _ := testAuthManager(0)
		m.drivers["claudecode"].start = func(context.Context, authLoginOptions) (authProcess, error) {
			started = true
			return nil, context.Canceled
		}
		e.SetAgentAuthManager(m)
		e.agent = agent
		e.handleMessage(p, &Message{SessionKey: "chat", UserID: "admin", Content: content, ReplyCtx: "private"})
		p.mu.Lock()
		defer p.mu.Unlock()
		return started, strings.Join(p.sent, "\n")
	}

	// A non-Claude adapter's login goes to its own driver (stubbed to fail
	// in tests), never to Claude's.
	codex := &declaringAgent{caps: AgentCapabilities{Auth: AuthCaps{Login: true, Status: true, Flow: AuthFlowDeviceCode}}}
	if started, sent := run(codex, "/auth login"); started || !strings.Contains(sent, "Codex authentication failed") {
		t.Fatalf("codex auth: started=%v reply=%q", started, sent)
	}
	// A flow its driver does not implement is refused.
	codexURL := &declaringAgent{caps: AgentCapabilities{Auth: AuthCaps{Login: true, Status: true, Flow: AuthFlowURLCode}}}
	if started, sent := run(codexURL, "/auth login"); started || !strings.Contains(sent, "not available") {
		t.Fatalf("codex url-code auth: started=%v reply=%q", started, sent)
	}
	// Status-only declaration does not permit login.
	statusOnly := &claudeNamedAgent{declaringAgent{caps: AgentCapabilities{Auth: AuthCaps{Status: true, Flow: AuthFlowURLCode}}}}
	if started, sent := run(statusOnly, "/auth login"); started || !strings.Contains(sent, "not available") {
		t.Fatalf("status-only auth: started=%v reply=%q", started, sent)
	}
	// Wrong flow for Claude is refused too.
	wrongFlow := &claudeNamedAgent{declaringAgent{caps: AgentCapabilities{Auth: AuthCaps{Login: true, Status: true, Flow: AuthFlowDeviceCode}}}}
	if started, _ := run(wrongFlow, "/auth login"); started {
		t.Fatal("device-code declaration started Claude's URL-code login")
	}
}
