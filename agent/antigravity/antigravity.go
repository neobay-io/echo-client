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

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("antigravity", New)
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
	mu             sync.Mutex
}

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

	cmd, extraArgs, err := resolveCommand(opts)
	if err != nil {
		return nil, err
	}
	a := &Agent{
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
