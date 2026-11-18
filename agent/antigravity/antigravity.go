package antigravity

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/agent/acp"
	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("antigravity", New)
	// Options buildArgs passes (the hidden --gemini_dir is not listed in
	// help); checked after each CLI upgrade.
	core.RegisterAgentCLIChecks("antigravity", core.AgentCLICheck{
		Args: []string{"--help"},
		Flags: []string{"-p", "--output-format", "--conversation", "--mode", "--dangerously-skip-permissions",
			"--model", "--effort", "--add-dir"},
	})
}

// Agent drives the Antigravity CLI (agy) with one `agy -p --output-format
// stream-json` process per turn, resuming with --conversation.
//
// Modes (canonical keys):
//   - "default": agy's own review mode; every non-read-only tool call is
//     forwarded to the chat for approval through a PreToolUse hook
//   - "acceptEdits", "plan", "auto": --mode accept-edits | plan | auto
//   - "bypassPermissions": --dangerously-skip-permissions
type Agent struct {
	workDir        string
	model          string
	reasoningLevel string
	mode           string
	cmd            string   // resolved CLI path
	cliExtraArgs   []string // extra args from the cmd option
	configEnv      []string // [projects.agent.options.env]
	timeout        time.Duration
	sessionEnv     []string
	sessionOpts    core.AgentSessionOptions // per-chat overrides for the next StartSession
	catalog        *modelCatalog
	backend        string // backendPrint | backendACP
	mu             sync.Mutex
}

// Backends: one `agy -p` per turn (default), or a resident `agy acp` (P6).
const (
	backendPrint = "print"
	backendACP   = "acp"
)

var (
	_ core.ModelSwitcher          = (*Agent)(nil)
	_ core.ReasoningLevelSwitcher = (*Agent)(nil)
	_ core.ModeSwitcher           = (*Agent)(nil)
	_ core.SessionEnvInjector     = (*Agent)(nil)
	_ core.SessionOptionsInjector = (*Agent)(nil)
	_ core.MemoryFileProvider     = (*Agent)(nil)
	_ core.SkillProvider          = (*Agent)(nil)
	_ core.WorkDirSwitcher        = (*Agent)(nil)
)

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	reasoningLevel, _ := opts["reasoning_level"].(string)
	mode, _ := opts["mode"].(string)

	backend := backendPrint
	if b, _ := opts["backend"].(string); strings.TrimSpace(b) != "" {
		switch strings.ToLower(strings.TrimSpace(b)) {
		case backendPrint, "prompt", "-p":
		case backendACP:
			backend = backendACP
		default:
			return nil, fmt.Errorf(`antigravity: backend must be "print" or "acp" (got %q)`, b)
		}
	}

	cmd, extraArgs, err := resolveCommand(opts)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		backend:        backend,
		workDir:        workDir,
		model:          strings.TrimSpace(model),
		reasoningLevel: normalizeEffort(reasoningLevel),
		mode:           normalizeMode(mode),
		cmd:            cmd,
		cliExtraArgs:   extraArgs,
		configEnv:      core.ParseConfigEnv(opts),
		timeout:        timeoutOption(opts),
	}
	a.catalog = newModelCatalog(a.fetchModels)
	return a, nil
}

func resolveCommand(opts map[string]any) (string, []string, error) {
	cmd, extraArgs := core.ParseCmdOpts(opts, "")
	if cmd != "" {
		path, err := exec.LookPath(cmd)
		if err != nil {
			return "", nil, fmt.Errorf("antigravity: %q not found: %w", cmd, err)
		}
		return path, extraArgs, nil
	}
	path, err := core.ResolveAgentCLI("antigravity")
	if err != nil {
		return "", nil, fmt.Errorf("antigravity: agy CLI not found in PATH; install it from https://antigravity.google/docs/cli-overview or set cmd: %w", err)
	}
	return path, nil, nil
}

func timeoutOption(opts map[string]any) time.Duration {
	var mins int64
	switch v := opts["timeout_mins"].(type) {
	case int64:
		mins = v
	case int:
		mins = int64(v)
	case float64:
		mins = int64(v)
	}
	if mins <= 0 {
		return 0
	}
	return time.Duration(mins) * time.Minute
}

func normalizeMode(raw string) string {
	return core.NormalizePermissionMode("antigravity", raw)
}

// normalizeEffort lower-cases an effort level; "", "auto" and "default" mean
// "use the model's default".
func normalizeEffort(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "", "auto", "default":
		return ""
	}
	return v
}

func (a *Agent) Name() string { return "antigravity" }

// Capabilities implements core.CapabilityProvider: chat login runs
// `agy acp --login` (Google URL + pasted code); agy has no logout.
func (a *Agent) Capabilities() core.AgentCapabilities {
	caps := core.AgentCapabilities{
		Auth: core.AuthCaps{Login: true, Status: true, Flow: core.AuthFlowURLCode},
	}
	if a.backend == backendACP {
		// agy acp: settings change inside the session, /stop cancels the
		// turn, a message sent during a turn continues it.
		caps.LiveModel, caps.LiveMode, caps.Interrupt, caps.Steer, caps.AskUser = true, true, true, true, true
	}
	return caps
}

// ApplyLiveSettings implements core.LiveSettingsApplier: an `agy acp`
// session switches model, thinking level and mode in place. Prompt-mode
// sessions cannot; the engine restarts them instead.
func (a *Agent) ApplyLiveSettings(ctx context.Context, session core.AgentSession, opts core.AgentSessionOptions) error {
	var s *acp.Session
	switch v := session.(type) {
	case *acp.Session:
		s = v
	case acp.SteeringSession:
		s = v.Session
	default:
		return fmt.Errorf("antigravity: this session cannot change settings while running")
	}
	a.mu.Lock()
	model, effort, mode := a.model, a.reasoningLevel, a.mode
	a.mu.Unlock()
	if opts.Model != "" {
		model = strings.TrimSpace(opts.Model)
	}
	if opts.ReasoningLevel != "" {
		effort = normalizeEffort(opts.ReasoningLevel)
	}
	if opts.Mode != "" {
		mode = normalizeMode(opts.Mode)
	}
	base, suffix := splitEffortSuffix(model)
	if effort == "" {
		effort = suffix // "gemini-4-argon-high" carries its level
	}
	return s.Apply(ctx, base, a.effortForModel(model, effort), mode)
}

// CompressCommand implements core.ContextCompressor: `agy acp` runs the
// /compact command; prompt mode has none.
func (a *Agent) CompressCommand() string {
	if a.backend == backendACP {
		return "/compact"
	}
	return ""
}

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
	slog.Info("antigravity: work_dir changed", "work_dir", dir)
}

func (a *Agent) GetWorkDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workDir
}

// ── ModelSwitcher ──────────────────────────────────────────────

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = strings.TrimSpace(model)
	slog.Info("antigravity: model changed", "model", a.model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// AvailableModels lists `agy models` grouped by base model; the effort part
// of each id is chosen with /effort.
// CLIPath implements core.CLIPathProvider: the binary resolved at start
// (possibly off PATH, or a configured cmd), which the engine watches for
// upgrades.
func (a *Agent) CLIPath() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cmd
}

// InvalidateModelCache implements core.ModelCacheInvalidator: after a CLI
// upgrade the model list is fetched again.
func (a *Agent) InvalidateModelCache() { a.catalog.invalidate() }

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	models := a.catalog.get(ctx)
	options := make([]core.ModelOption, 0, len(models))
	for _, m := range models {
		desc := m.Label
		if len(m.Efforts) > 0 {
			desc += " · " + strings.Join(m.Efforts, "/")
		}
		options = append(options, core.ModelOption{Name: m.ID, Desc: desc})
	}
	return options
}

// ── ReasoningLevelSwitcher ─────────────────────────────────────

func (a *Agent) SetReasoningLevel(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasoningLevel = normalizeEffort(level)
	slog.Info("antigravity: reasoning level changed", "reasoning_level", a.reasoningLevel)
}

func (a *Agent) GetReasoningLevel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reasoningLevel
}

// AvailableReasoningLevels never blocks on the CLI; before the catalog loads
// it returns agy's --effort values and loads the catalog in the background.
func (a *Agent) AvailableReasoningLevels() []core.ReasoningLevelOption {
	levels := effortUnion(a.catalog.cached())
	if len(levels) == 0 {
		levels = []string{"low", "medium", "high"}
	}
	options := make([]core.ReasoningLevelOption, 0, len(levels))
	for _, level := range levels {
		options = append(options, core.ReasoningLevelOption{Name: level, Desc: effortDesc(level)})
	}
	return options
}

func effortDesc(level string) string {
	switch level {
	case "low":
		return "Faster, less thinking"
	case "medium":
		return "Balanced"
	case "high":
		return "Deeper thinking"
	case "xhigh":
		return "Very deep thinking"
	case "max":
		return "Maximum thinking"
	}
	return ""
}

// ── ModeSwitcher ───────────────────────────────────────────────

func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("antigravity: mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	return []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "Ask in chat before edits and commands; reads run freely", DescZh: "编辑和命令先在聊天里确认，读取直接执行"},
		{Key: "acceptEdits", Name: "Accept Edits", NameZh: "接受编辑", Desc: "agy --mode accept-edits", DescZh: "agy --mode accept-edits"},
		{Key: "plan", Name: "Plan", NameZh: "规划模式", Desc: "agy --mode plan: plan only, no changes", DescZh: "agy --mode plan：只规划，不修改"},
		{Key: "auto", Name: "Auto", NameZh: "自动模式", Desc: "agy --mode auto: agy decides approvals itself", DescZh: "agy --mode auto：由 agy 自己审批"},
		{Key: "bypassPermissions", Name: "YOLO", NameZh: "YOLO 模式", Desc: "Skip all approvals (--dangerously-skip-permissions)", DescZh: "跳过所有审批（--dangerously-skip-permissions）"},
	}
}

// ── Sessions ───────────────────────────────────────────────────

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

// SetSessionOptions sets the chat session's overrides for the next
// StartSession (core.SessionOptionsInjector).
func (a *Agent) SetSessionOptions(opts core.AgentSessionOptions) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionOpts = opts
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	model, effort, mode := a.model, a.reasoningLevel, a.mode
	if a.sessionOpts.Model != "" {
		model = strings.TrimSpace(a.sessionOpts.Model)
	}
	if a.sessionOpts.ReasoningLevel != "" {
		effort = normalizeEffort(a.sessionOpts.ReasoningLevel)
	}
	if a.sessionOpts.Mode != "" {
		mode = normalizeMode(a.sessionOpts.Mode)
	}
	extraEnv := append(append([]string(nil), a.configEnv...), a.sessionEnv...)
	if a.backend == backendACP {
		base, suffix := splitEffortSuffix(model)
		if effort == "" {
			effort = suffix // "gemini-4-argon-high" carries its level
		}
		cfg := acp.Config{
			Agent:     "antigravity",
			Command:   a.cmd,
			Args:      append(append([]string(nil), a.cliExtraArgs...), "acp"),
			Env:       core.MergeEnv(os.Environ(), extraEnv),
			WorkDir:   core.SessionWorkDirFromEnv(extraEnv, a.workDir),
			ExtraDirs: core.SessionExtraDirsFromEnv(extraEnv),
			ResumeID:  sessionID,
			Mode:      mode,
			Model:     base,
		}
		a.mu.Unlock()
		cfg.Effort = a.effortForModel(model, effort)
		s, err := acp.Start(ctx, cfg)
		if err != nil {
			return nil, err
		}
		// agy takes a prompt sent during a turn: it ends the running one and
		// continues with the new message (verified with agy 1.5.10).
		return acp.SteeringSession{Session: s}, nil
	}
	cfg := sessionConfig{
		cmd:       a.cmd,
		extraArgs: append([]string(nil), a.cliExtraArgs...),
		workDir:   core.SessionWorkDirFromEnv(extraEnv, a.workDir),
		model:     model,
		effort:    a.effortForModel(model, effort),
		mode:      mode,
		extraDirs: core.SessionExtraDirsFromEnv(extraEnv),
		extraEnv:  extraEnv,
		timeout:   a.timeout,
	}
	a.mu.Unlock()
	return newAntigravitySession(ctx, cfg, sessionID)
}

// effortForModel drops an effort the chosen model does not offer; agy rejects
// such a combination outright. Unknown models keep the effort.
func (a *Agent) effortForModel(model, effort string) string {
	if effort == "" || model == "" {
		return effort
	}
	base, _ := splitEffortSuffix(model)
	for _, m := range a.catalog.cached() {
		if m.ID != base {
			continue
		}
		for _, e := range m.Efforts {
			if e == effort {
				return effort
			}
		}
		if len(m.Efforts) == 0 {
			return effort // no effort variants listed; let agy decide
		}
		slog.Warn("antigravity: model does not offer this effort; using the model default", "model", model, "effort", effort)
		return ""
	}
	return effort
}

func (a *Agent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	return listAgySessions(ctx, a.GetWorkDir())
}

func (a *Agent) Stop() error { return nil }

// ── Files ──────────────────────────────────────────────────────

func (a *Agent) absWorkDir() string {
	dir := a.GetWorkDir()
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func (a *Agent) SkillDirs() []string {
	dirs := []string{filepath.Join(a.absWorkDir(), ".agents", "skills")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".gemini", "skills"))
	}
	return dirs
}

func (a *Agent) ProjectMemoryFile() string {
	return filepath.Join(a.absWorkDir(), "AGENTS.md")
}

func (a *Agent) GlobalMemoryFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "GEMINI.md")
}
