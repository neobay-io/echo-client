package kimi

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
	core.RegisterAgent("kimi", New)
	// Options buildArgs passes; checked after each CLI upgrade.
	core.RegisterAgentCLIChecks("kimi", core.AgentCLICheck{
		Args:  []string{"--help"},
		Flags: []string{"--prompt", "--output-format", "-S", "--model", "--add-dir"},
	})
}

// modeBypass is the only mode Kimi Code's prompt mode offers: `kimi -p` runs
// every tool without asking and rejects --plan, --yolo and --auto. With
// backend = "acp" (`kimi acp`, P6) all of Kimi's modes are available.
const modeBypass = core.PermissionModeBypass

// Backends: one `kimi -p` per turn (default), or a resident `kimi acp`.
const (
	backendPrint = "print"
	backendACP   = "acp"
)

// effortEnv makes Kimi Code force a thinking effort for one invocation. It
// binds to the [thinking] config section's forcedEffort, which applies to the
// managed Kimi Code models (verified against Kimi Code 2.5.3).
const effortEnv = "KIMI_MODEL_THINKING_EFFORT"

// Agent drives Kimi Code CLI 2.x with one `kimi -p --output-format
// stream-json` process per turn, resuming with `-S <session id>`.
type Agent struct {
	workDir        string
	model          string
	reasoningLevel string
	cmd            string   // resolved CLI path
	cliExtraArgs   []string // extra args from the cmd option
	configEnv      []string // [projects.agent.options.env]
	timeout        time.Duration
	sessionEnv     []string
	sessionOpts    core.AgentSessionOptions // per-chat overrides for the next StartSession
	catalog        *modelCatalog
	backend        string // backendPrint | backendACP
	mode           string // permission mode (ACP backend)
	mu             sync.Mutex
}

var (
	_ core.ModelSwitcher          = (*Agent)(nil)
	_ core.ReasoningLevelSwitcher = (*Agent)(nil)
	_ core.ModeSwitcher           = (*Agent)(nil)
	_ core.SessionEnvInjector     = (*Agent)(nil)
	_ core.SessionOptionsInjector = (*Agent)(nil)
	_ core.HistoryProvider        = (*Agent)(nil)
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
	backend := backendPrint
	if b, _ := opts["backend"].(string); strings.TrimSpace(b) != "" {
		switch strings.ToLower(strings.TrimSpace(b)) {
		case backendPrint, "prompt", "-p":
		case backendACP:
			backend = backendACP
		default:
			return nil, fmt.Errorf(`kimi: backend must be "print" or "acp" (got %q)`, b)
		}
	}
	rawMode, _ := opts["mode"].(string)
	mode := modeBypass
	if backend == backendPrint {
		// Require an explicit opt-in: a missing or safer-sounding mode must
		// not silently become "run everything".
		if !core.IsBypassPermissionMode("kimi", rawMode) {
			return nil, fmt.Errorf(`kimi: set mode = "bypassPermissions" (got %q): kimi -p runs every tool without asking; set backend = "acp" for Kimi's other modes`, strings.TrimSpace(rawMode))
		}
	} else {
		if strings.TrimSpace(rawMode) == "" {
			rawMode = core.PermissionModeDefault
		}
		native, _, ok := core.ResolvePermissionMode("kimi", rawMode)
		if !ok {
			return nil, fmt.Errorf("kimi: unknown mode %q", rawMode)
		}
		mode = native
	}

	cmd, extraArgs, err := resolveCommand(opts)
	if err != nil {
		return nil, err
	}

	a := &Agent{
		workDir:        workDir,
		model:          strings.TrimSpace(model),
		reasoningLevel: normalizeEffort(reasoningLevel),
		cmd:            cmd,
		cliExtraArgs:   extraArgs,
		configEnv:      core.ParseConfigEnv(opts),
		timeout:        timeoutOption(opts),
		backend:        backend,
		mode:           mode,
	}
	a.catalog = newModelCatalog(a.fetchModels)
	return a, nil
}

// resolveCommand honors an explicit cmd option, else finds kimi on PATH or in
// ~/.kimi-code/bin (the installer's location, usually missing from a
// service's PATH).
func resolveCommand(opts map[string]any) (string, []string, error) {
	cmd, extraArgs := core.ParseCmdOpts(opts, "")
	if cmd != "" {
		path, err := exec.LookPath(cmd)
		if err != nil {
			return "", nil, fmt.Errorf("kimi: %q not found: %w", cmd, err)
		}
		return path, extraArgs, nil
	}
	path, err := core.ResolveAgentCLI("kimi")
	if err != nil {
		return "", nil, fmt.Errorf("kimi: Kimi Code CLI not found in PATH or ~/.kimi-code/bin; install it from https://moonshotai.github.io/kimi-code/ or set cmd: %w", err)
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

func (a *Agent) Name() string { return "kimi" }

// Capabilities implements core.CapabilityProvider: chat login uses Kimi
// Code's device-code flow (`kimi login`); the CLI has no logout.
func (a *Agent) Capabilities() core.AgentCapabilities {
	caps := core.AgentCapabilities{
		Auth: core.AuthCaps{Login: true, Status: true, Flow: core.AuthFlowDeviceCode},
	}
	if a.backend == backendACP {
		// kimi acp: settings change inside the session, /stop cancels the
		// turn, questions come as forms. It refuses a prompt during a turn,
		// so no steering (messages queue instead).
		caps.LiveModel, caps.LiveMode, caps.Interrupt, caps.AskUser = true, true, true, true
	}
	return caps
}

// ApplyLiveSettings implements core.LiveSettingsApplier: a `kimi acp`
// session switches model, thinking and mode in place. Prompt-mode sessions
// cannot; the engine restarts them instead.
func (a *Agent) ApplyLiveSettings(ctx context.Context, session core.AgentSession, opts core.AgentSessionOptions) error {
	s, ok := asACP(session)
	if !ok {
		return fmt.Errorf("kimi: this session cannot change settings while running")
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
		mode = opts.Mode
	}
	return s.Apply(ctx, model, a.effortForModel(model, effort), mode)
}

func asACP(session core.AgentSession) (*acp.Session, bool) {
	switch s := session.(type) {
	case *acp.Session:
		return s, true
	case acp.SteeringSession:
		return s.Session, true
	}
	return nil, false
}

// CompressCommand implements core.ContextCompressor: `kimi acp` runs the
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
	slog.Info("kimi: work_dir changed", "work_dir", dir)
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
	slog.Info("kimi: model changed", "model", a.model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

// AvailableModels lists the model aliases Kimi Code's own picker offers.
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
		options = append(options, core.ModelOption{Name: m.Alias, Desc: m.DisplayName})
	}
	return options
}

// ── ReasoningLevelSwitcher ─────────────────────────────────────

func (a *Agent) SetReasoningLevel(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasoningLevel = normalizeEffort(level)
	slog.Info("kimi: reasoning level changed", "reasoning_level", a.reasoningLevel)
}

func (a *Agent) GetReasoningLevel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reasoningLevel
}

// AvailableReasoningLevels returns the efforts the account's models support.
// It never blocks on the CLI: before the catalog is loaded it returns the
// levels Kimi Code models currently use and loads the catalog in the
// background.
func (a *Agent) AvailableReasoningLevels() []core.ReasoningLevelOption {
	levels := effortUnion(a.catalog.cached())
	if len(levels) == 0 {
		levels = []string{"low", "high", "max"}
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

// SetMode: prompt mode accepts only bypass-equivalent values (anything else
// would be a promise `kimi -p` cannot keep); the ACP backend takes Kimi's
// modes.
func (a *Agent) SetMode(mode string) {
	if a.backend != backendACP {
		if !core.IsBypassPermissionMode("kimi", mode) {
			slog.Warn("kimi: only bypassPermissions is available in prompt mode", "mode", mode)
		}
		return
	}
	native, _, ok := core.ResolvePermissionMode("kimi", mode)
	if !ok {
		slog.Warn("kimi: unknown mode", "mode", mode)
		return
	}
	a.mu.Lock()
	a.mode = native
	a.mu.Unlock()
}

func (a *Agent) GetMode() string {
	if a.backend != backendACP {
		return modeBypass
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	if a.backend == backendACP {
		return []core.PermissionModeInfo{
			{Key: core.PermissionModeDefault, Name: "Default", NameZh: "默认", Desc: "Ask before tools run", DescZh: "工具运行前确认"},
			{Key: core.PermissionModePlan, Name: "Plan", NameZh: "计划", Desc: "Read-only planning, no tool execution", DescZh: "只读规划，不执行工具"},
			{Key: core.PermissionModeAuto, Name: "Auto", NameZh: "自动", Desc: "Kimi approves safe operations, asks for the rest", DescZh: "Kimi 自动批准安全操作，其余确认"},
			{Key: modeBypass, Name: "YOLO", NameZh: "全部放行", Desc: "Approve everything", DescZh: "不经确认执行所有工具"},
		}
	}
	return []core.PermissionModeInfo{{
		Key:    modeBypass,
		Name:   "Bypass (prompt mode)",
		NameZh: "全部放行（prompt 模式）",
		Desc:   "kimi -p runs every tool without asking; other modes need ACP",
		DescZh: "kimi -p 不经确认执行所有工具；其他模式需要 ACP",
	}}
}

// ── Sessions ───────────────────────────────────────────────────

func (a *Agent) SetSessionEnv(env []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionEnv = env
}

// SetSessionOptions sets the chat session's overrides for the next
// StartSession (core.SessionOptionsInjector). Mode overrides are ignored.
func (a *Agent) SetSessionOptions(opts core.AgentSessionOptions) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionOpts = opts
}

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	if a.backend == backendACP {
		return a.startACP(ctx, sessionID)
	}
	a.mu.Lock()
	model, effort := a.model, a.reasoningLevel
	if a.sessionOpts.Model != "" {
		model = strings.TrimSpace(a.sessionOpts.Model)
	}
	if a.sessionOpts.ReasoningLevel != "" {
		effort = normalizeEffort(a.sessionOpts.ReasoningLevel)
	}
	extraEnv := append(append([]string(nil), a.configEnv...), a.sessionEnv...)
	workDir := core.SessionWorkDirFromEnv(extraEnv, a.workDir)
	extraDirs := core.SessionExtraDirsFromEnv(extraEnv)
	cfg := sessionConfig{
		cmd:       a.cmd,
		extraArgs: append([]string(nil), a.cliExtraArgs...),
		workDir:   workDir,
		model:     model,
		effort:    a.effortForModel(model, effort),
		extraDirs: extraDirs,
		extraEnv:  extraEnv,
		timeout:   a.timeout,
	}
	a.mu.Unlock()
	return newKimiSession(ctx, cfg, sessionID), nil
}

// startACP runs `kimi acp` for the chat, resuming sessionID.
func (a *Agent) startACP(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	model, effort, mode := a.model, a.reasoningLevel, a.mode
	if a.sessionOpts.Model != "" {
		model = strings.TrimSpace(a.sessionOpts.Model)
	}
	if a.sessionOpts.ReasoningLevel != "" {
		effort = normalizeEffort(a.sessionOpts.ReasoningLevel)
	}
	if a.sessionOpts.Mode != "" {
		mode = a.sessionOpts.Mode
	}
	extraEnv := append(append([]string(nil), a.configEnv...), a.sessionEnv...)
	cfg := acp.Config{
		Agent:     "kimi",
		Command:   a.cmd,
		Args:      append(append([]string(nil), a.cliExtraArgs...), "acp"),
		Env:       core.MergeEnv(os.Environ(), extraEnv),
		WorkDir:   core.SessionWorkDirFromEnv(extraEnv, a.workDir),
		ExtraDirs: core.SessionExtraDirsFromEnv(extraEnv),
		ResumeID:  sessionID,
		Mode:      mode,
		Model:     model,
	}
	a.mu.Unlock()
	cfg.Effort = a.effortForModel(model, effort)
	s, err := acp.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// effortForModel drops an effort the chosen model does not support (e.g. a
// highspeed model without thinking levels), which Kimi would otherwise reject.
// Unknown models keep the effort.
func (a *Agent) effortForModel(model, effort string) string {
	if effort == "" {
		return ""
	}
	if model == "" {
		return effort
	}
	for _, m := range a.catalog.cached() {
		if m.Alias != model {
			continue
		}
		for _, e := range m.Efforts {
			if e == effort {
				return effort
			}
		}
		slog.Warn("kimi: model does not support this effort; using the model default", "model", model, "effort", effort)
		return ""
	}
	return effort
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	a.mu.Lock()
	workDir := a.workDir
	a.mu.Unlock()
	return listKimiSessions(workDir, a.storageEnv()...)
}

func (a *Agent) GetSessionHistory(_ context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	return readSessionHistory(sessionID, limit, a.storageEnv()...)
}

func (a *Agent) storageEnv() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append(append([]string(nil), a.configEnv...), a.sessionEnv...)
}

func (a *Agent) Stop() error { return nil }

// ── Files ──────────────────────────────────────────────────────

func (a *Agent) SkillDirs() []string {
	absDir, err := filepath.Abs(a.GetWorkDir())
	if err != nil {
		absDir = a.GetWorkDir()
	}
	dirs := []string{
		filepath.Join(absDir, ".kimi-code", "skills"),
		filepath.Join(absDir, ".agents", "skills"),
	}
	if home := kimiCodeHome(a.storageEnv()...); home != "" {
		dirs = append(dirs, filepath.Join(home, "skills"))
	}
	return dirs
}

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.GetWorkDir())
	if err != nil {
		absDir = a.GetWorkDir()
	}
	return filepath.Join(absDir, "AGENTS.md")
}

func (a *Agent) GlobalMemoryFile() string {
	home := kimiCodeHome(a.storageEnv()...)
	if home == "" {
		return ""
	}
	return filepath.Join(home, "AGENTS.md")
}

// kimiCodeHome is $KIMI_CODE_HOME (the last value in extraEnv wins over the
// process environment) or ~/.kimi-code.
func kimiCodeHome(extraEnv ...string) string {
	home := os.Getenv("KIMI_CODE_HOME")
	for _, entry := range extraEnv {
		if value, ok := strings.CutPrefix(entry, "KIMI_CODE_HOME="); ok {
			home = value
		}
	}
	if home != "" {
		return home
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(userHome, ".kimi-code")
}

// SessionsAreExclusive implements core.ExclusiveSessions: over the resident
// backend a session is open in one process at a time.
func (a *Agent) SessionsAreExclusive() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backend == backendACP
}
