package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	driver *authDriver
	owner  authOwner
	// base is the daemon context the login was started under; ctx is the
	// login's own (with its timeout).
	base      context.Context
	ctx       context.Context
	cancel    context.CancelFunc
	cancelled bool // by the user or a config change; guarded by mu
	process   authProcess
	ready     bool
	submitted bool
}

type authUpdate struct {
	state  string
	agent  string // label of the CLI being logged in
	prompt authPrompt
}

// AgentAuthManager runs the official CLI logins of the agents (one at a time
// per daemon, shared by its projects). It never reads, writes, or returns the
// resulting persistent OAuth credentials.
type AgentAuthManager struct {
	mu      sync.Mutex
	cfg     AgentAuthConfig
	attempt *authAttempt
	// loggingOut is the agent whose logout is running; like a login it
	// blocks every other auth operation, so the two can never overlap.
	loggingOut string
	drivers    map[string]*authDriver
	// refresh retires cached processes of the agent whose login changed.
	refresh func(agent string)
}

func NewAgentAuthManager(cfg AgentAuthConfig, refresh func(agent string)) *AgentAuthManager {
	m := &AgentAuthManager{drivers: defaultAuthDrivers(), refresh: refresh}
	m.ApplyConfig(cfg)
	return m
}

// driver returns the login driver of an agent type, or nil.
func (m *AgentAuthManager) driver(agent string) *authDriver {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drivers[agent]
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
		m.attempt.cancelled = true
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

// pendingLabel names the CLI of owner's pending login ("" if none).
func (m *AgentAuthManager) pendingLabel(owner authOwner) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.attempt == nil || m.attempt.owner != owner {
		return ""
	}
	return m.attempt.driver.label
}

func (m *AgentAuthManager) begin(ctx context.Context, agent string, owner authOwner, notify func(authUpdate)) error {
	return m.beginWith(ctx, agent, owner, authLoginOptions{}, notify)
}

func (m *AgentAuthManager) beginWith(ctx context.Context, agent string, owner authOwner, opts authLoginOptions, notify func(authUpdate)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.authorizedLocked(owner.UserID) {
		return errors.New("auth_denied")
	}
	d := m.drivers[agent]
	if d == nil {
		return errors.New("auth_unsupported")
	}
	if m.attempt != nil || m.loggingOut != "" {
		return errors.New("auth_busy")
	}
	if d.override != "" && os.Getenv(d.override) != "" {
		return errors.New("auth_override")
	}
	loginCtx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
	process, err := d.start(loginCtx, opts)
	if err != nil {
		cancel()
		if err.Error() == "auth_region_required" {
			return err
		}
		return errors.New("auth_failed")
	}
	attempt := &authAttempt{driver: d, owner: owner, base: ctx, ctx: loginCtx, cancel: cancel, process: process}
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
	d := attempt.driver
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
	promptSent := false
	for {
		n, err := attempt.process.Read(buffer)
		if n > 0 {
			if output.Len()+n > 64*1024 {
				attempt.cancel()
				break
			}
			output.Write(buffer[:n])
			if !promptSent {
				if prompt, ok := d.prompt(output.String()); ok {
					m.mu.Lock()
					attempt.ready = prompt.NeedsCode
					m.mu.Unlock()
					promptSent = true
					state := "auth_device"
					if prompt.NeedsCode {
						state = "link"
					}
					notify(authUpdate{state: state, agent: d.label, prompt: prompt})
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
		// The CLI finished: check its login on a fresh deadline, not what is
		// left of the login window.
		probeCtx, cancel := context.WithTimeout(attempt.base, 30*time.Second)
		status, probeErr := d.status(probeCtx)
		cancel()
		m.mu.Lock()
		stillAuthorized := m.authorizedLocked(attempt.owner.UserID) && !attempt.cancelled
		m.mu.Unlock()
		if !stillAuthorized {
			state = "auth_cancelled"
		} else if probeErr == nil && status.LoggedIn {
			if m.refresh != nil {
				m.refresh(d.agent)
			}
			state = "auth_success"
		}
	}
	m.mu.Lock()
	if m.attempt == attempt {
		m.attempt = nil
	}
	m.mu.Unlock()
	notify(authUpdate{state: state, agent: d.label})
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
	if attempt.driver.flow == AuthFlowDeviceCode {
		m.mu.Unlock()
		return errors.New("auth_code_not_needed")
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
	m.attempt.cancelled = true
	m.attempt.cancel()
	return nil
}

// status asks an agent's CLI whether it is logged in.
func (m *AgentAuthManager) status(ctx context.Context, agent string) (authStatus, error) {
	d := m.driver(agent)
	if d == nil {
		return authStatus{}, errors.New("auth_unsupported")
	}
	return d.status(ctx)
}

// logout runs an agent CLI's logout and retires processes that still hold
// the old login.
func (m *AgentAuthManager) logout(ctx context.Context, agent string, userID string) error {
	m.mu.Lock()
	if !m.authorizedLocked(userID) {
		m.mu.Unlock()
		return errors.New("auth_denied")
	}
	d := m.drivers[agent]
	if d == nil || d.logout == nil {
		m.mu.Unlock()
		return errors.New("auth_logout_unsupported")
	}
	if m.attempt != nil || m.loggingOut != "" {
		m.mu.Unlock()
		return errors.New("auth_busy")
	}
	m.loggingOut = agent
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.loggingOut = ""
		m.mu.Unlock()
	}()
	if err := d.logout(ctx); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	if m.refresh != nil {
		m.refresh(agent)
	}
	return nil
}

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
	cmd := exec.CommandContext(ctx, authCLIPath("claudecode"), "auth", "login", "--claudeai")
	cmd.Env = claudeAuthEnv()
	KillGroupOnCancel(cmd)
	cmd.WaitDelay = 5 * time.Second
	file, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &authCLIProcess{File: file, cmd: cmd}, nil
}

func (e *Engine) SetAgentAuthManager(manager *AgentAuthManager) { e.agentAuthMgr = manager }

func (e *Engine) RefreshInteractiveAuth() {
	e.retireAllInteractiveStates()
}

// RefreshInteractiveAuthFor retires cached processes if this engine runs
// agent (its login changed).
func (e *Engine) RefreshInteractiveAuthFor(agent string) {
	if e.agent != nil && e.agent.Name() == agent {
		e.retireAllInteractiveStates()
	}
}

// retireAllInteractiveStates makes every chat's next turn start a fresh agent
// process, resuming the same agent session. It does not stop an in-flight
// turn: the next turn retires the cached process before sending, under the
// same startup serialization used by queues.
func (e *Engine) retireAllInteractiveStates() {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	for _, state := range e.interactiveStates {
		state.stale = true
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

// authRegions are the login regions `/auth login kimi <region>` accepts.
var authRegions = map[string]string{"mainland-cn": "mainland-cn", "cn": "mainland-cn", "global": "global"}

// parseAuthArgs splits /auth arguments into action, agent type (empty: the
// project's agent), the code for `code`, and confirm for `logout`. A login
// may name a region (Kimi Code); see parseAuthRegion.
func parseAuthArgs(args []string) (action, agent, code string, confirm, ok bool) {
	action, agent, code, confirm, _, ok = parseAuthArgsWithRegion(args)
	return action, agent, code, confirm, ok
}

func parseAuthArgsWithRegion(args []string) (action, agent, code string, confirm bool, region string, ok bool) {
	action = "status"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
		args = args[1:]
	}
	switch action {
	case "code":
		if len(args) != 1 {
			return action, "", "", false, "", false
		}
		return action, "", args[0], false, "", true
	case "cancel":
		return action, "", "", false, "", len(args) == 0
	case "status", "login", "logout":
	default:
		return action, "", "", false, "", false
	}
	for _, arg := range args {
		lower := strings.ToLower(arg)
		switch {
		case action == "logout" && lower == "confirm" && !confirm:
			confirm = true
		case agent == "" && authAgentAliases[lower] != "":
			agent = authAgentAliases[lower]
		case action == "login" && region == "" && authRegions[lower] != "":
			region = authRegions[lower]
		default:
			return action, "", "", false, "", false
		}
	}
	if region != "" && agent != "kimi" {
		return action, "", "", false, "", false
	}
	return action, agent, "", confirm, region, true
}

func (e *Engine) cmdAuth(p Platform, msg *Message, args []string) {
	defer func() { msg.Content = "" }()
	i18n := NewI18n(e.i18n.CurrentLang())
	reply := func(key string, a ...any) {
		text := i18n.T(MsgKey(key))
		if len(a) > 0 {
			text = fmt.Sprintf(text, a...)
		}
		e.reply(p, msg.ReplyCtx, text)
	}
	// replyErr formats error keys that name the CLI with its label.
	replyErr := func(key, label string) {
		if authLabelKeys[key] {
			reply(key, label)
		} else {
			reply(key)
		}
	}
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
	action, agent, code, confirm, region, ok := parseAuthArgsWithRegion(args)
	if !ok {
		reply("auth_usage")
		return
	}
	owner := authOwner{e.name, msg.SessionKey, msg.UserID}
	if action == "code" || action == "cancel" {
		// These act on the pending login, whichever CLI it is for.
		if action == "code" {
			label := e.agentAuthMgr.pendingLabel(owner)
			if err := e.agentAuthMgr.submit(owner, code); err != nil {
				reply(err.Error())
			} else {
				reply("auth_submitted", label)
			}
			return
		}
		if err := e.agentAuthMgr.cancel(owner); err != nil {
			reply(err.Error())
		}
		return
	}
	if agent == "" {
		// The project's own agent, if it declares chat login. The declaration
		// alone is not enough: a driver must exist for its flow.
		caps := CapabilitiesOf(e.agent)
		if e.agent == nil || !remoteAuthImplemented(e.agent.Name(), caps.Auth.Flow) ||
			(action == "login" && !caps.Auth.Login) || (action == "status" && !caps.Auth.Status) {
			reply("auth_unsupported")
			return
		}
		agent = e.agent.Name()
	}
	d := e.agentAuthMgr.driver(agent)
	if d == nil {
		reply("auth_unsupported")
		return
	}
	switch action {
	case "login":
		// Do not capture msg: for /auth code it may contain credential material.
		replyCtx := msg.ReplyCtx
		keyword := authAgentKeyword(agent)
		err := e.agentAuthMgr.beginWith(e.ctx, agent, owner, authLoginOptions{Region: region}, func(update authUpdate) {
			// The login prompt is a card on platforms with buttons: they send
			// the same /auth commands, so the same owner and chat checks apply.
			cancelButton := ButtonOption{Text: i18n.T(MsgKey("auth_button_cancel")), Data: "cmd:/auth cancel"}
			switch update.state {
			case "link":
				text := fmt.Sprintf(i18n.T(MsgKey("auth_link")), update.agent, update.prompt.URL)
				e.replyWithButtons(p, replyCtx, text, [][]ButtonOption{{cancelButton}})
			case "auth_device":
				text := fmt.Sprintf(i18n.T(MsgKey("auth_device")), update.agent, update.prompt.URL, update.prompt.UserCode)
				e.replyWithButtons(p, replyCtx, text, [][]ButtonOption{{
					{Text: i18n.T(MsgKey("auth_button_status")), Data: "cmd:/auth status " + keyword},
					cancelButton,
				}})
			default:
				e.reply(p, replyCtx, fmt.Sprintf(i18n.T(MsgKey(update.state)), update.agent))
			}
		})
		if err != nil {
			replyErr(err.Error(), d.label)
		}
	case "logout":
		if d.logout == nil {
			reply("auth_logout_unsupported", d.label)
			return
		}
		if !confirm {
			reply("auth_logout_confirm", d.label, authAgentKeyword(agent))
			return
		}
		ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
		defer cancel()
		if err := e.agentAuthMgr.logout(ctx, agent, msg.UserID); err != nil {
			key := err.Error()
			if !strings.HasPrefix(key, "auth_") {
				// The error carries only the exit status, never CLI output.
				slog.Warn("agent CLI logout failed", "agent", agent, "error", err)
				key = "auth_logout_failed"
			}
			replyErr(key, d.label)
			return
		}
		reply("auth_logout_done", d.label)
	case "status":
		if label := e.agentAuthMgr.pendingLabel(owner); label != "" {
			reply("auth_pending", label)
			return
		}
		ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
		defer cancel()
		st, err := e.agentAuthMgr.status(ctx, agent)
		switch {
		case err != nil:
			reply("auth_failed", d.label)
		case !st.LoggedIn:
			reply("auth_logged_out", d.label, authAgentKeyword(agent))
		default:
			detail := st.Detail
			if !st.Expires.IsZero() {
				detail = strings.Trim(detail+", "+fmt.Sprintf(i18n.T(MsgKey("auth_expires")), st.Expires.Local().Format("2006-01-02 15:04")), ", ")
			}
			if detail != "" {
				detail = " (" + detail + ")"
			}
			reply("auth_logged_in", d.label, detail)
		}
	default:
		reply("auth_usage")
	}
}

// authLabelKeys are the auth error messages that name the CLI.
var authLabelKeys = map[string]bool{
	"auth_failed": true, "auth_timeout": true, "auth_cancelled": true,
	"auth_logout_unsupported": true, "auth_logout_failed": true,
}

// authAgentKeyword is the short name users type for an agent after /auth.
func authAgentKeyword(agent string) string {
	switch agent {
	case "claudecode":
		return "claude"
	case "antigravity":
		return "agy"
	}
	return agent
}
