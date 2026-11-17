package core

import (
	"context"
	"slices"
	"strings"
	"testing"
)

type declaringAgent struct {
	optionsAgent
	caps AgentCapabilities
}

func (a *declaringAgent) Capabilities() AgentCapabilities { return a.caps }

func TestCapabilitiesOfDerivesFromInterfaces(t *testing.T) {
	caps := CapabilitiesOf(&optionsAgent{})
	if !caps.Models || !caps.Sessions.List || !caps.Sessions.Delete || caps.Sessions.History {
		t.Fatalf("derived capabilities = %#v", caps)
	}
	if !slices.Equal(caps.Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("efforts = %#v", caps.Efforts)
	}
	if !slices.Equal(caps.Modes, []string{"default", "acceptEdits", "auto", "bypassPermissions"}) {
		t.Fatalf("modes = %#v", caps.Modes)
	}
	if caps.Auth.Login || caps.Compact || caps.EffortViaModel {
		t.Fatalf("undeclared capabilities set: %#v", caps)
	}

	bare := CapabilitiesOf(&stubAgent{})
	if bare.Models || len(bare.Efforts) > 0 || len(bare.Modes) > 0 || bare.Sessions.Delete {
		t.Fatalf("stub agent capabilities = %#v", bare)
	}
	if got := CapabilitiesOf(nil); got.Sessions.List {
		t.Fatalf("nil agent capabilities = %#v", got)
	}
}

func TestCapabilitiesOfMergesDeclarations(t *testing.T) {
	agent := &declaringAgent{caps: AgentCapabilities{
		Auth:    AuthCaps{Login: true, Status: true, Flow: AuthFlowURLCode},
		Models:  false, // derived fields cannot be switched off by declaration
		Efforts: []string{"low"},
	}}
	caps := CapabilitiesOf(agent)
	if !caps.Auth.Login || !caps.Models || !slices.Equal(caps.Efforts, []string{"low"}) {
		t.Fatalf("merged capabilities = %#v", caps)
	}
}

func TestValidateCapabilitiesRejectsUnbackedDeclarations(t *testing.T) {
	agent := &declaringAgent{caps: AgentCapabilities{
		Auth:      AuthCaps{Login: true, Flow: "carrier-pigeon"},
		Modes:     []string{"plan"},
		Compact:   true,
		Steer:     true,
		Sessions:  SessionCaps{History: true},
		LiveModel: true,
	}}
	errs := ValidateCapabilities(agent)
	text := ""
	for _, err := range errs {
		text += err.Error() + "\n"
	}
	for _, want := range []string{
		`unknown flow "carrier-pigeon"`,
		"no remote login implementation", // agent is named codex
		`mode "plan" declared but not listed`,
		"compact declared",
		"steer declared",
		"session history declared",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "live_model") {
		t.Errorf("live_model is backed by ModelSwitcher, got:\n%s", text)
	}
}

type claudeNamedAgent struct{ declaringAgent }

func (a *claudeNamedAgent) Name() string { return "claudecode" }

func TestValidateCapabilitiesAcceptsBackedDeclarations(t *testing.T) {
	agent := &claudeNamedAgent{declaringAgent{caps: AgentCapabilities{
		Auth:  AuthCaps{Login: true, Status: true, Flow: AuthFlowURLCode},
		Modes: []string{"auto", "bypassPermissions"},
	}}}
	if errs := ValidateCapabilities(agent); len(errs) != 0 {
		t.Fatalf("ValidateCapabilities = %v", errs)
	}
	if errs := ValidateCapabilities(&stubAgent{}); errs != nil {
		t.Fatalf("agent without declarations: %v", errs)
	}
}

func TestHelpHidesCommandsTheAgentCannotRun(t *testing.T) {
	for _, lang := range []Language{LangEnglish, LangChinese, LangJapanese} {
		help := NewI18n(lang).T(MsgHelp)
		got := filterHelpText(help, capabilityCommands(AgentCapabilities{Models: true}), nil)
		for _, hidden := range []string{"/auth ", "/effort ", "/mode ", "/compress\n"} {
			if strings.Contains(got, "\n\n"+hidden) {
				t.Errorf("%s: %q still listed", lang, hidden)
			}
		}
		for _, kept := range []string{"/model ", "/new ", "/status\n"} {
			if !strings.Contains(got, kept) {
				t.Errorf("%s: %q missing", lang, kept)
			}
		}
		if strings.Contains(got, "default / edit / plan / yolo") {
			t.Errorf("%s: stale permission-mode hint kept without /mode", lang)
		}
	}

	caps := AgentCapabilities{Modes: []string{"default", "auto", "bypassPermissions"}}
	en := filterHelpText(NewI18n(LangEnglish).T(MsgHelp), capabilityCommands(caps), caps.Modes)
	if !strings.HasSuffix(en, "Permission modes: default / auto / bypassPermissions") {
		t.Errorf("english mode hint = %q", en[strings.LastIndex(en, "\n\n"):])
	}
	zh := filterHelpText(NewI18n(LangChinese).T(MsgHelp), capabilityCommands(caps), caps.Modes)
	if !strings.HasSuffix(zh, "权限模式：default / auto / bypassPermissions") {
		t.Errorf("chinese mode hint = %q", zh[strings.LastIndex(zh, "\n\n"):])
	}
}

func TestStatusShowsSessionSettingsAndCapabilities(t *testing.T) {
	agent := &optionsAgent{model: "project-model", level: "medium", mode: "default"}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.cmdModel(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{"gpt-b"})
	p.sent = nil

	e.cmdStatus(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"})

	status := strings.Join(p.sent, "\n")
	for _, want := range []string{"Model: gpt-b", "Reasoning effort: medium", "Mode: default", "Agent supports: /model · /effort (low/medium/high)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}
}

func TestAuthCommandFollowsCapabilities(t *testing.T) {
	p := &authTestPlatform{}
	e := NewEngine("project", &noRoutingAuthAgent{}, []Platform{p}, "", LangEnglish)
	m, _ := testAuthManager(0)
	started := false
	m.start = func(context.Context) (authProcess, error) {
		started = true
		return nil, context.Canceled
	}
	e.SetAgentAuthManager(m)
	e.agent = &declaringAgent{} // named codex, declares no auth
	e.handleMessage(p, &Message{SessionKey: "chat", UserID: "admin", Content: "/auth login", ReplyCtx: "private"})
	if started {
		t.Fatal("login started for an agent without auth capability")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !strings.Contains(strings.Join(p.sent, "\n"), "not available for this project's agent") {
		t.Fatalf("replies = %#v", p.sent)
	}
}
