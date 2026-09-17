package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
)

type AgentAuthConfig struct {
	Enabled        bool
	AllowedUserIDs []string
	Timeout        time.Duration
}

type authOwner struct {
	Project, SessionKey, UserID string
}

type authProcess interface {
	io.ReadWriteCloser
	Wait() error
}

type authAttempt struct {
	owner     authOwner
	ctx       context.Context
	cancel    context.CancelFunc
	process   authProcess
	ready     bool
	submitted bool
}

type authUpdate struct {
	state string
	url   string
}

// AgentAuthManager owns one official CLI login per daemon, shared by projects.
// It never reads, writes, or returns the resulting persistent OAuth credentials.
type AgentAuthManager struct {
	mu      sync.Mutex
	cfg     AgentAuthConfig
	attempt *authAttempt
	start   func(context.Context) (authProcess, error)
	probe   func(context.Context) (bool, error)
	refresh func()
}

func NewAgentAuthManager(cfg AgentAuthConfig, refresh func()) *AgentAuthManager {
	m := &AgentAuthManager{start: startClaudeLogin, probe: claudeLoginStatus, refresh: refresh}
	m.ApplyConfig(cfg)
	return m
}

func (m *AgentAuthManager) ApplyConfig(cfg AgentAuthConfig) {
	cfg.AllowedUserIDs = append([]string(nil), cfg.AllowedUserIDs...)
	if cfg.Timeout <= 0 || cfg.Timeout > 30*time.Minute {
		cfg.Timeout = 10 * time.Minute
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	if m.attempt != nil && !m.authorizedLocked(m.attempt.owner.UserID) {
		m.attempt.cancel()
	}
}

func (m *AgentAuthManager) authorizedLocked(userID string) bool {
	if !m.cfg.Enabled || strings.TrimSpace(userID) == "" {
		return false
	}
	for _, allowed := range m.cfg.AllowedUserIDs {
		if allowed = strings.TrimSpace(allowed); allowed != "*" && allowed == userID {
			return true
		}
	}
	return false
}

func (m *AgentAuthManager) Authorized(userID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authorizedLocked(userID)
}

func (m *AgentAuthManager) pending(owner authOwner) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempt != nil && m.attempt.owner == owner
}

func (m *AgentAuthManager) begin(ctx context.Context, owner authOwner, notify func(authUpdate)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorizedLocked(owner.UserID) {
		return errors.New("auth_denied")
	}
	if m.attempt != nil {
		return errors.New("auth_busy")
	}
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return errors.New("auth_override")
	}
	loginCtx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	process, err := m.start(loginCtx)
	if err != nil {
		cancel()
		return errors.New("auth_failed")
	}
	attempt := &authAttempt{owner: owner, ctx: loginCtx, cancel: cancel, process: process}
	m.attempt = attempt
	go m.run(attempt, notify)
	return nil
}

var authURLPattern = regexp.MustCompile(`https://[^\x00-\x20\x7f<>"\x1b]+`)

func claudeAuthorizationURL(output string) string {
	for _, candidate := range authURLPattern.FindAllString(output, -1) {
		if len(candidate) > 2048 {
			continue
		}
		u, err := url.Parse(candidate)
		if err != nil || u.User != nil || u.Port() != "" || u.Fragment != "" {
			continue
		}
		allowed := (u.Host == "claude.com" && u.Path == "/cai/oauth/authorize") ||
			(u.Host == "claude.ai" && u.Path == "/oauth/authorize")
		q := u.Query()
		if allowed && q.Get("response_type") == "code" && q.Get("code_challenge") != "" &&
			q.Get("code_challenge_method") == "S256" && q.Get("state") != "" && q.Get("code") == "true" {
			return candidate
		}
	}
	return ""
}

func (m *AgentAuthManager) run(attempt *authAttempt, notify func(authUpdate)) {
	defer attempt.cancel()
	defer func() { _ = attempt.process.Close() }()
	runDone := make(chan struct{})
	defer close(runDone)
	// Killing the CLI alone may leave inherited PTY descriptors open. Closing
	// the reader makes cancellation bounded even in that case.
	go func() {
		select {
		case <-attempt.ctx.Done():
			_ = attempt.process.Close()
		case <-runDone:
		}
	}()
	var output strings.Builder
	buffer := make([]byte, 2048)
	linkSent := false
	for {
		n, err := attempt.process.Read(buffer)
		if n > 0 {
			if output.Len()+n > 64*1024 {
				attempt.cancel()
				break
			}
			output.Write(buffer[:n])
			if !linkSent && strings.Contains(output.String(), "Paste code") {
				if link := claudeAuthorizationURL(output.String()); link != "" {
					m.mu.Lock()
					attempt.ready = true
					m.mu.Unlock()
					linkSent = true
					notify(authUpdate{state: "link", url: link})
				}
			}
		}
		if err != nil {
			break
		}
	}
	err := attempt.process.Wait()
	output.Reset()
	state := "auth_failed"
	if errors.Is(attempt.ctx.Err(), context.DeadlineExceeded) {
		state = "auth_timeout"
	} else if attempt.ctx.Err() != nil {
		state = "auth_cancelled"
	} else if err == nil {
		probeCtx, cancel := context.WithTimeout(attempt.ctx, 15*time.Second)
		loggedIn, probeErr := m.probe(probeCtx)
		cancel()
		m.mu.Lock()
		stillAuthorized := m.authorizedLocked(attempt.owner.UserID) && attempt.ctx.Err() == nil
		m.mu.Unlock()
		if !stillAuthorized {
			state = "auth_cancelled"
		} else if probeErr == nil && loggedIn {
			if m.refresh != nil {
				m.refresh()
			}
			state = "auth_success"
		}
	}
	m.mu.Lock()
	if m.attempt == attempt {
		m.attempt = nil
	}
	m.mu.Unlock()
	notify(authUpdate{state: state})
}

var authorizationCodePattern = regexp.MustCompile(`^[A-Za-z0-9_./~+\-=#]+$`)

func (m *AgentAuthManager) submit(owner authOwner, code string) error {
	if len(code) == 0 || len(code) > 2048 || strings.HasPrefix(code, "sk-ant-") ||
		!authorizationCodePattern.MatchString(code) {
		return errors.New("auth_invalid_code")
	}
	m.mu.Lock()
	attempt := m.attempt
	if !m.authorizedLocked(owner.UserID) || attempt == nil || attempt.owner != owner || attempt.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("auth_no_pending")
	}
	if !attempt.ready || attempt.submitted {
		m.mu.Unlock()
		return errors.New("auth_not_ready")
	}
	attempt.submitted = true
	m.mu.Unlock()
	if _, err := io.WriteString(attempt.process, code+"\n"); err != nil {
		attempt.cancel()
		return errors.New("auth_failed")
	}
	return nil
}

func (m *AgentAuthManager) cancel(owner authOwner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorizedLocked(owner.UserID) || m.attempt == nil || m.attempt.owner != owner {
		return errors.New("auth_no_pending")
	}
	m.attempt.cancel()
	return nil
}

type claudeLoginProcess struct {
	*os.File
	cmd *exec.Cmd
}

func (p *claudeLoginProcess) Wait() error { return p.cmd.Wait() }

func claudeAuthEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CLAUDECODE=") {
			env = append(env, entry)
		}
	}
	return env
}

func startClaudeLogin(ctx context.Context) (authProcess, error) {
	cmd := exec.CommandContext(ctx, "claude", "auth", "login", "--claudeai")
	cmd.Env = claudeAuthEnv()
	file, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &claudeLoginProcess{File: file, cmd: cmd}, nil
}

func claudeLoginStatus(ctx context.Context) (bool, error) {
	cmd := exec.CommandContext(ctx, "claude", "auth", "status")
	cmd.Env = claudeAuthEnv()
	output, err := cmd.Output()
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if json.Unmarshal(output, &status) != nil {
		return false, errors.New("auth_failed")
	}
	// The CLI exits nonzero for a valid logged-out status response.
	if err != nil && status.LoggedIn {
		return false, errors.New("auth_failed")
	}
	return status.LoggedIn, nil
}

func (e *Engine) SetAgentAuthManager(manager *AgentAuthManager) { e.agentAuthMgr = manager }

func (e *Engine) RefreshInteractiveAuth() {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	// Do not stop an in-flight turn. The next turn retires its cached process
	// before sending, under the same startup serialization used by queues.
	for _, state := range e.interactiveStates {
		state.authStale = true
	}
}

func isAuthCommand(content string) bool {
	fields := strings.Fields(content)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return false
	}
	command, _, _ := strings.Cut(strings.TrimPrefix(fields[0], "/"), "@")
	return matchPrefix(strings.ToLower(command), builtinCommands) == "auth"
}

func RedactAuthCommand(content string) string {
	// Conservatively redact the entire value, including quoted commands and
	// commands preceded by a platform mention or embedded in multiple lines.
	for _, field := range strings.Fields(content) {
		if isAuthCommand(field) {
			return "[redacted auth command]"
		}
	}
	return content
}

func (e *Engine) handleAuthMessage(p Platform, msg *Message) bool {
	msg.QuotedContent = RedactAuthCommand(msg.QuotedContent)
	if isAuthCommand(msg.Content) {
		args := strings.Fields(msg.Content)[1:]
		e.cmdAuth(p, msg, args)
		msg.Content = ""
		return true
	}
	if e.agentAuthMgr != nil && e.agentAuthMgr.pending(authOwner{e.name, msg.SessionKey, msg.UserID}) &&
		!strings.HasPrefix(strings.TrimSpace(msg.Content), "/") {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("auth_code_hint")))
		msg.Content = ""
		return true
	}
	return false
}

func (e *Engine) cmdAuth(p Platform, msg *Message, args []string) {
	defer func() { msg.Content = "" }()
	i18n := NewI18n(e.i18n.CurrentLang())
	reply := func(key string) { e.reply(p, msg.ReplyCtx, i18n.T(MsgKey(key))) }
	if e.disabledCmds["auth"] || e.agentAuthMgr == nil || !e.agentAuthMgr.Authorized(msg.UserID) {
		reply("auth_denied")
		return
	}
	checker, ok := p.(PrivateChatChecker)
	if !ok || !checker.IsPrivateChat(msg.ReplyCtx) || msg.Forwarded || msg.HasQuotedContent() ||
		msg.Audio != nil || len(msg.Images)+len(msg.Files) > 0 {
		reply("auth_private")
		return
	}
	if e.agent == nil || e.agent.Name() != "claudecode" {
		reply("auth_unsupported")
		return
	}
	owner := authOwner{e.name, msg.SessionKey, msg.UserID}
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	if (action != "code" && len(args) > 1) || (action == "code" && len(args) != 2) {
		reply("auth_usage")
		return
	}
	switch action {
	case "login":
		// Do not capture msg: for /auth code it may contain credential material.
		replyCtx := msg.ReplyCtx
		err := e.agentAuthMgr.begin(e.ctx, owner, func(update authUpdate) {
			if update.state == "link" {
				e.reply(p, replyCtx, fmt.Sprintf(i18n.T(MsgKey("auth_link")), update.url))
			} else {
				e.reply(p, replyCtx, i18n.T(MsgKey(update.state)))
			}
		})
		if err != nil {
			reply(err.Error())
		}
	case "code":
		if err := e.agentAuthMgr.submit(owner, args[1]); err != nil {
			reply(err.Error())
		} else {
			reply("auth_submitted")
		}
	case "cancel":
		if err := e.agentAuthMgr.cancel(owner); err != nil {
			reply(err.Error())
		}
	case "status":
		if e.agentAuthMgr.pending(owner) {
			reply("auth_pending")
			return
		}
		ctx, cancel := context.WithTimeout(e.ctx, 15*time.Second)
		defer cancel()
		loggedIn, err := e.agentAuthMgr.probe(ctx)
		if err != nil {
			reply("auth_failed")
		} else if loggedIn {
			reply("auth_logged_in")
		} else {
			reply("auth_logged_out")
		}
	default:
		reply("auth_usage")
	}
}
