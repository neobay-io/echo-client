package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/creack/pty"
)

// authPrompt is what the user needs to finish a login in a browser.
type authPrompt struct {
	URL string
	// UserCode is the code to enter at URL (device-code flows).
	UserCode string
	// NeedsCode: the CLI waits for a one-time code pasted back with
	// `/auth code` (URL + code flows).
	NeedsCode bool
}

// authStatus is a CLI's login state as the CLI reports it.
type authStatus struct {
	LoggedIn bool
	Detail   string    // how it is logged in, e.g. "claude.ai, max"
	Expires  time.Time // zero when unknown
}

// authDriver drives one agent CLI's own login commands. Drivers never read,
// write or return the persistent credentials the CLI stores; they only start
// the CLI's login, relay the URL (and user code) it prints, and ask the CLI
// whether it is logged in.
type authDriver struct {
	agent string // agent type: claudecode, codex, kimi, antigravity
	label string // name shown in chat
	flow  string // AuthFlowURLCode | AuthFlowDeviceCode
	start func(ctx context.Context, opts authLoginOptions) (authProcess, error)
	// prompt finds the login prompt in the CLI's output so far.
	prompt func(output string) (authPrompt, bool)
	status func(ctx context.Context) (authStatus, error)
	// logout is nil when the CLI has no logout command.
	logout func(ctx context.Context) error
	// override names an environment variable that, when set for the daemon,
	// takes precedence over a browser login.
	override string
}

// authLoginOptions are per-login choices (`/auth login kimi global`).
type authLoginOptions struct {
	Region string // Kimi Code: mainland-cn | global
}

// authCommandTimeout bounds status and logout commands.
const authCommandTimeout = 30 * time.Second

// authAgentAliases maps what users type after /auth to agent types.
var authAgentAliases = map[string]string{
	"claude": "claudecode", "claudecode": "claudecode", "claude-code": "claudecode",
	"codex": "codex",
	"kimi":  "kimi", "kimi-code": "kimi",
	"agy": "antigravity", "antigravity": "antigravity",
}

func defaultAuthDrivers() map[string]*authDriver {
	return map[string]*authDriver{
		"claudecode": {
			agent: "claudecode", label: "Claude", flow: AuthFlowURLCode,
			start: func(ctx context.Context, _ authLoginOptions) (authProcess, error) { return startClaudeLogin(ctx) },
			prompt: func(output string) (authPrompt, bool) {
				if !strings.Contains(output, "Paste code") {
					return authPrompt{}, false
				}
				link := claudeAuthorizationURL(output)
				if link == "" {
					return authPrompt{}, false
				}
				return authPrompt{URL: link, NeedsCode: true}, true
			},
			status: claudeLoginStatus,
			logout: func(ctx context.Context) error {
				return runAuthCLI(ctx, "claudecode", claudeAuthEnv(), "auth", "logout")
			},
			override: "CLAUDE_CODE_OAUTH_TOKEN",
		},
		"codex": {
			agent: "codex", label: "Codex", flow: AuthFlowDeviceCode,
			start: func(ctx context.Context, _ authLoginOptions) (authProcess, error) {
				return startAuthPTY(ctx, "codex", nil, "login", "--device-auth")
			},
			prompt: deviceCodePrompt("auth.openai.com", "chatgpt.com"),
			status: codexLoginStatus,
			logout: func(ctx context.Context) error { return runAuthCLI(ctx, "codex", nil, "logout") },
		},
		"kimi": {
			agent: "kimi", label: "Kimi Code", flow: AuthFlowDeviceCode,
			start: func(ctx context.Context, opts authLoginOptions) (authProcess, error) {
				// Without --region the CLI asks interactively, which a chat
				// cannot answer; cmdAuth makes sure there is one.
				region := opts.Region
				if region == "" {
					region = kimiLoginRegion()
				}
				if region == "" {
					return nil, errors.New("auth_region_required")
				}
				return startAuthPTY(ctx, "kimi", nil, "login", "--region", region)
			},
			prompt: deviceCodePrompt("kimi.com", "kimi.ai", "moonshot.cn"),
			status: kimiLoginStatus,
		},
		"antigravity": {
			agent: "antigravity", label: "Antigravity (agy)", flow: AuthFlowURLCode,
			start: func(ctx context.Context, _ authLoginOptions) (authProcess, error) {
				return startAuthPTY(ctx, "antigravity", nil, "acp", "--login")
			},
			prompt: func(output string) (authPrompt, bool) {
				plain := stripTerminalCodes(output)
				if !strings.Contains(strings.ToLower(plain), "authorization code") {
					return authPrompt{}, false
				}
				link := allowedAuthURL(plain, "accounts.google.com")
				if link == "" {
					return authPrompt{}, false
				}
				return authPrompt{URL: link, NeedsCode: true}, true
			},
			status: agyLoginStatus,
		},
	}
}

// remoteAuthImplemented reports whether chat login is implemented for an
// agent with the declared flow.
func remoteAuthImplemented(agentName, flow string) bool {
	d := defaultAuthDrivers()[agentName]
	return d != nil && d.flow == flow
}

// authCLIProcess is a CLI login running on a pseudo-terminal: the CLIs print
// their prompts, and read pasted codes, only on a terminal.
type authCLIProcess struct {
	*os.File
	cmd *exec.Cmd
}

func (p *authCLIProcess) Wait() error { return p.cmd.Wait() }

func authCLIPath(agentType string) string {
	if path, err := ResolveAgentCLI(agentType); err == nil {
		return path
	}
	return AgentCLIBinary(agentType)
}

func startAuthPTY(ctx context.Context, agentType string, env []string, args ...string) (authProcess, error) {
	cmd := exec.CommandContext(ctx, authCLIPath(agentType), args...)
	cmd.Env = env
	// The PTY child leads its own session and group; cancelling kills the
	// whole group, so a login helper it spawned cannot keep polling.
	KillGroupOnCancel(cmd)
	cmd.WaitDelay = 5 * time.Second
	file, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &authCLIProcess{File: file, cmd: cmd}, nil
}

// authCLICommand prepares a status or logout command: bounded in time, and
// killed with everything it started.
func authCLICommand(ctx context.Context, agentType string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, authCLIPath(agentType), args...)
	cmd.Env = env
	UseProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func runAuthCLI(ctx context.Context, agentType string, env []string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, authCommandTimeout)
	defer cancel()
	// The output is not kept: it may describe the account.
	if err := authCLICommand(ctx, agentType, env, args...).Run(); err != nil {
		return err
	}
	return nil
}

var terminalCodePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// stripTerminalCodes removes ANSI escape sequences (colors, cursor moves,
// OSC 8 hyperlinks) from terminal output.
func stripTerminalCodes(s string) string {
	return terminalCodePattern.ReplaceAllString(s, "")
}

// allowedAuthURL returns the first https URL in output on one of hosts (or
// their subdomains), without credentials or a port.
func allowedAuthURL(output string, hosts ...string) string {
	for _, candidate := range authURLPattern.FindAllString(output, -1) {
		candidate = strings.TrimRight(candidate, ".,;:)]}'")
		if len(candidate) > 2048 {
			continue
		}
		u, err := url.Parse(candidate)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		for _, allowed := range hosts {
			if host == allowed || strings.HasSuffix(host, "."+allowed) {
				return candidate
			}
		}
	}
	return ""
}

var (
	userCodePattern      = regexp.MustCompile(`\b[A-Z0-9]{4,8}-[A-Z0-9]{4,8}\b`)
	validUserCodePattern = regexp.MustCompile(`^[A-Za-z0-9]{2,12}(-[A-Za-z0-9]{2,12}){0,3}$`)
)

// deviceCodePrompt finds a device-code login prompt: a verification URL on
// one of hosts plus the user code. A URL that already carries the code
// (verification_uri_complete) is enough.
func deviceCodePrompt(hosts ...string) func(string) (authPrompt, bool) {
	return func(output string) (authPrompt, bool) {
		plain := stripTerminalCodes(output)
		// Complete lines only: a PTY read can end inside the URL or code.
		end := strings.LastIndexAny(plain, "\r\n")
		if end < 0 {
			return authPrompt{}, false
		}
		plain = plain[:end+1]
		link := allowedAuthURL(plain, hosts...)
		if link == "" {
			return authPrompt{}, false
		}
		if u, err := url.Parse(link); err == nil {
			if code := u.Query().Get("user_code"); code != "" {
				// The code is shown in chat as text: accept only a plain code,
				// not anything the URL could smuggle in.
				if !validUserCodePattern.MatchString(code) {
					return authPrompt{}, false
				}
				return authPrompt{URL: link, UserCode: code}, true
			}
		}
		// Otherwise the code is printed next to the URL; wait for it.
		code := userCodePattern.FindString(strings.Replace(plain, link, "", 1))
		if code == "" {
			return authPrompt{}, false
		}
		return authPrompt{URL: link, UserCode: code}, true
	}
}

func claudeLoginStatus(ctx context.Context) (authStatus, error) {
	output, err := authCLICommand(ctx, "claudecode", claudeAuthEnv(), "auth", "status").Output()
	var status struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		SubscriptionType string `json:"subscriptionType"`
		LoginExpiresAt   string `json:"loginExpiresAt"`
	}
	if json.Unmarshal(output, &status) != nil {
		return authStatus{}, errors.New("auth_failed")
	}
	// The CLI exits nonzero for a valid logged-out status response.
	if err != nil && status.LoggedIn {
		return authStatus{}, errors.New("auth_failed")
	}
	st := authStatus{LoggedIn: status.LoggedIn}
	if status.LoggedIn {
		st.Detail = strings.Trim(status.AuthMethod+", "+status.SubscriptionType, ", ")
		if t, perr := time.Parse(time.RFC3339, status.LoginExpiresAt); perr == nil {
			st.Expires = t
		}
	}
	return st, nil
}

func codexLoginStatus(ctx context.Context) (authStatus, error) {
	out, err := authCLICommand(ctx, "codex", nil, "login", "status").CombinedOutput()
	text := strings.TrimSpace(stripTerminalCodes(string(out)))
	if strings.HasPrefix(text, "Logged in") {
		return authStatus{LoggedIn: true, Detail: strings.TrimPrefix(firstLine(text, 80), "Logged in using ")}, nil
	}
	if strings.Contains(strings.ToLower(text), "not logged in") {
		return authStatus{}, nil
	}
	if err != nil {
		return authStatus{}, errors.New("auth_failed")
	}
	return authStatus{}, errors.New("auth_failed")
}

// kimiHomeDir is $KIMI_CODE_HOME or ~/.kimi-code, like the CLI (and the
// agent adapter) resolve it. Replaced in tests.
var kimiHomeDir = func() string {
	if home := os.Getenv("KIMI_CODE_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kimi-code")
}

func kimiLoginRegion() string {
	data, err := os.ReadFile(filepath.Join(kimiHomeDir(), "region"))
	if err != nil {
		return ""
	}
	switch region := strings.TrimSpace(string(data)); region {
	case "mainland-cn", "global":
		return region
	}
	return ""
}

// kimiLoginStatus reports Kimi Code as logged in when it has stored an OAuth
// refresh token. Kimi Code has no status command; only the presence of the
// token is read, never its value.
func kimiLoginStatus(context.Context) (authStatus, error) {
	files, err := filepath.Glob(filepath.Join(kimiHomeDir(), "credentials", "*.json"))
	if err != nil {
		return authStatus{}, errors.New("auth_failed")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var cred struct {
			RefreshToken string `json:"refresh_token"`
		}
		if json.Unmarshal(data, &cred) == nil && cred.RefreshToken != "" {
			return authStatus{LoggedIn: true, Detail: "OAuth"}, nil
		}
	}
	return authStatus{}, nil
}

// agyLoginStatus asks agy for its usage quota: that only answers when it is
// signed in (agy has no status command).
func agyLoginStatus(ctx context.Context) (authStatus, error) {
	cmd := authCLICommand(ctx, "antigravity", nil, "-p", "/usage", "--output-format", "text")
	cmd.Dir = os.TempDir()
	out, err := cmd.CombinedOutput()
	text := stripTerminalCodes(string(out))
	if err == nil && strings.Contains(text, "Limit") {
		return authStatus{LoggedIn: true, Detail: "Google"}, nil
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "sign in") || strings.Contains(lower, "log in") || strings.Contains(lower, "login") ||
		strings.Contains(lower, "not authenticated") || strings.Contains(lower, "unauthenticated") {
		return authStatus{}, nil
	}
	return authStatus{}, errors.New("auth_failed")
}
