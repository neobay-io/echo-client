package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("codex", New)
	// Options Send passes to `codex exec` and `codex exec resume`; checked
	// after each CLI upgrade.
	core.RegisterAgentCLIChecks("codex",
		core.AgentCLICheck{
			Args: []string{"exec", "--help"},
			Flags: []string{"--json", "--skip-git-repo-check", "--model", "-c", "--add-dir", "--image", "--cd",
				"--dangerously-bypass-approvals-and-sandbox"},
		},
		core.AgentCLICheck{
			Args:  []string{"exec", "resume", "--help"},
			Flags: []string{"--json", "--skip-git-repo-check", "--model", "-c", "--image", "--dangerously-bypass-approvals-and-sandbox"},
		},
		// The app-server backend (P4).
		core.AgentCLICheck{Args: []string{"app-server", "--help"}, Flags: []string{"--stdio"}},
	)
}

// Agent drives OpenAI Codex CLI using `codex exec --json`.
//
// Modes use the canonical names from core (legacy aliases in parentheses).
// `codex exec resume` rejects --sandbox / --approve-for-me, so everything is
// passed as -c overrides that both `exec` and `exec resume` accept:
//   - "default" (suggest): no overrides; ~/.codex/config.toml decides
//   - "acceptEdits" (auto-edit): sandbox_mode=workspace-write, approval_policy=never
//   - "auto" (full-auto): what --approve-for-me sets: workspace-write sandbox,
//     approval_policy=on-request, approvals_reviewer=auto_review
//   - "bypassPermissions" (yolo): --dangerously-bypass-approvals-and-sandbox
type Agent struct {
	workDir        string
	model          string
	reasoningLevel string
	mode           string // canonical: "default" | "acceptEdits" | "auto" | "bypassPermissions"
	providers      []core.ProviderConfig
	activeIdx      int // -1 = no provider set
	sessionEnv     []string
	sessionOpts    core.AgentSessionOptions // per-chat overrides for the next StartSession
	modelRunner    codexModelRunner
	// backend: "app-server" (default; P4) or "exec" (one `codex exec` per
	// turn, the pre-P4 behavior).
	backend string
	// efforts caches each model's reasoning efforts from model/list.
	efforts map[string][]string
	mu      sync.Mutex
}

const (
	backendAppServer = "app-server"
	backendExec      = "exec"
)

type codexModelRunner func(ctx context.Context, workDir string, env []string) ([]core.ModelOption, error)

func New(opts map[string]any) (core.Agent, error) {
	workDir, _ := opts["work_dir"].(string)
	if workDir == "" {
		workDir = "."
	}
	model, _ := opts["model"].(string)
	reasoningLevel, _ := opts["reasoning_level"].(string)
	reasoningLevel = normalizeReasoningLevel(reasoningLevel)
	mode, _ := opts["mode"].(string)
	mode = normalizeMode(mode)

	if _, err := exec.LookPath("codex"); err != nil {
		return nil, fmt.Errorf("codex: 'codex' CLI not found in PATH, install with: npm install -g @openai/codex")
	}

	backend := strings.ToLower(strings.TrimSpace(fmt.Sprint(opts["backend"])))
	switch backend {
	case backendExec:
	case "", "<nil>", backendAppServer, "appserver":
		backend = backendAppServer
	default:
		return nil, fmt.Errorf("codex: unknown backend %q (app-server or exec)", backend)
	}

	return &Agent{
		workDir:        workDir,
		model:          model,
		reasoningLevel: reasoningLevel,
		mode:           mode,
		activeIdx:      -1,
		backend:        backend,
	}, nil
}

func (a *Agent) appServer() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.backend == backendAppServer
}

// normalizeMode maps config values and legacy aliases (suggest, auto-edit,
// full-auto, yolo) to canonical mode keys; unknown values become "default".
func normalizeMode(raw string) string {
	return core.NormalizePermissionMode("codex", raw)
}

func normalizeReasoningLevel(raw string) string {
	switch lower := strings.ToLower(strings.TrimSpace(raw)); lower {
	case "", "auto", "default":
		return ""
	case "low":
		return "low"
	case "medium", "med":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "extra-high", "extra_high", "very-high":
		return "xhigh"
	default:
		// Other levels a model lists in model/list (minimal, max, ultra, …)
		// pass through; anything else is dropped.
		if validEffortName.MatchString(lower) {
			return lower
		}
		return ""
	}
}

var validEffortName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

func (a *Agent) Name() string { return "codex" }

// Capabilities implements core.CapabilityProvider: chat login uses Codex's
// device-code flow (`codex login --device-auth`); the app-server backend adds
// live settings, steering, interrupt, usage and questions.
func (a *Agent) Capabilities() core.AgentCapabilities {
	caps := core.AgentCapabilities{
		Auth: core.AuthCaps{Login: true, Status: true, Logout: true, Flow: core.AuthFlowDeviceCode},
	}
	if a.appServer() {
		caps.LiveModel, caps.LiveMode = true, true
		caps.Steer, caps.Interrupt, caps.Usage, caps.AskUser = true, true, true, true
	}
	return caps
}

func (a *Agent) SetWorkDir(dir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workDir = dir
	slog.Info("codex: work_dir changed", "work_dir", dir)
}

func (a *Agent) GetWorkDir() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workDir
}

func (a *Agent) SetModel(model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.model = model
	slog.Info("codex: model changed", "model", model)
}

func (a *Agent) GetModel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.model
}

func (a *Agent) SetReasoningLevel(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reasoningLevel = normalizeReasoningLevel(level)
	slog.Info("codex: reasoning level changed", "reasoning_level", a.reasoningLevel)
}

func (a *Agent) GetReasoningLevel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reasoningLevel
}

func (a *Agent) AvailableReasoningLevels() []core.ReasoningLevelOption {
	return []core.ReasoningLevelOption{
		{Name: "low", Desc: "Fastest, shallow reasoning"},
		{Name: "medium", Desc: "Balanced default"},
		{Name: "high", Desc: "Deeper reasoning"},
		{Name: "xhigh", Desc: "Deepest reasoning"},
	}
}

func (a *Agent) AvailableModels(ctx context.Context) []core.ModelOption {
	if models := a.fetchModelsFromAPI(ctx); len(models) > 0 {
		return models
	}

	a.mu.Lock()
	runner := a.modelRunner
	extraEnv := append(a.providerEnvLocked(), a.sessionEnv...)
	workDir := core.SessionWorkDirFromEnv(extraEnv, a.workDir)
	current := a.model
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) && a.providers[a.activeIdx].Model != "" {
		current = a.providers[a.activeIdx].Model
	}
	env := core.MergeEnv(os.Environ(), extraEnv)
	a.mu.Unlock()
	if runner == nil {
		runner = listCodexModels
	}
	if models, err := runner(ctx, workDir, env); err == nil && len(models) > 0 {
		efforts := make(map[string][]string, len(models))
		for _, m := range models {
			if len(m.Efforts) > 0 {
				efforts[m.Name] = m.Efforts
			}
		}
		a.mu.Lock()
		a.efforts = efforts
		a.mu.Unlock()
		return models
	} else if err != nil {
		slog.Debug("codex: failed to fetch models from app-server", "error", err)
	}
	if current != "" {
		return []core.ModelOption{{Name: current, Desc: "Current model"}}
	}
	return nil
}

func listCodexModels(ctx context.Context, workDir string, env []string) ([]core.ModelOption, error) {
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--stdio")
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	cmd.Env = env
	prepareCommandForGroupKill(cmd)
	cmd.WaitDelay = 500 * time.Millisecond

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex model list stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("codex model list stdout: %w", err)
	}
	// Close readers on cancellation even if an escaped descendant retains them.
	cmd.Cancel = func() error {
		_ = stdout.Close()
		_ = stdin.Close()
		return killCommandProcessGroup(cmd, cmd.Process.Pid)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex model list start: %w", err)
	}

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = stdin.Close()
		_ = stdout.Close()
		_ = killCommandProcessGroup(cmd, cmd.Process.Pid)
		_ = cmd.Wait()
	}
	defer stop()

	encoder := json.NewEncoder(stdin)
	if err := encoder.Encode(map[string]any{
		"id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]string{"name": "echo-client", "version": "1"}},
	}); err != nil {
		return nil, fmt.Errorf("codex model list initialize: %w", err)
	}

	type rpcResponse struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	waitForResponse := func(id int) (json.RawMessage, error) {
		for scanner.Scan() {
			var response rpcResponse
			if err := json.Unmarshal(scanner.Bytes(), &response); err != nil || response.ID != id {
				continue
			}
			if len(response.Error) > 0 && string(response.Error) != "null" {
				return nil, fmt.Errorf("app-server error: %s", response.Error)
			}
			return response.Result, nil
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
		stop()
		return nil, fmt.Errorf("app-server exited before response %d", id)
	}

	if _, err := waitForResponse(1); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	var models []core.ModelOption
	cursor := ""
	seenCursors := make(map[string]bool)
	seenModels := make(map[string]bool)
	for requestID := 2; ; requestID++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := encoder.Encode(map[string]any{"id": requestID, "method": "model/list", "params": params}); err != nil {
			return nil, err
		}
		resultJSON, err := waitForResponse(requestID)
		if err != nil {
			return nil, err
		}
		var result struct {
			NextCursor string `json:"nextCursor"`
			Data       []struct {
				Model                     string `json:"model"`
				DisplayName               string `json:"displayName"`
				IsDefault                 bool   `json:"isDefault"`
				SupportedReasoningEfforts []struct {
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
		}
		if err := json.Unmarshal(resultJSON, &result); err != nil {
			return nil, fmt.Errorf("decode codex model list: %w", err)
		}

		for _, model := range result.Data {
			name := strings.TrimSpace(model.Model)
			if name == "" || seenModels[name] {
				continue
			}
			seenModels[name] = true
			desc := strings.TrimSpace(model.DisplayName)
			if model.IsDefault {
				desc = strings.TrimSpace(desc + " (default)")
			}
			var efforts []string
			for _, e := range model.SupportedReasoningEfforts {
				if e.ReasoningEffort != "" {
					efforts = append(efforts, e.ReasoningEffort)
				}
			}
			models = append(models, core.ModelOption{Name: name, Desc: desc, Efforts: efforts})
		}
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
		if seenCursors[cursor] {
			return nil, fmt.Errorf("repeated model catalog cursor")
		}
		seenCursors[cursor] = true
	}

	stop()
	return models, nil
}

func isOpenAIChatModel(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, incompatible := range []string{"image", "audio", "transcrib", "tts", "realtime", "embedding", "search", "moderation"} {
		if strings.Contains(id, incompatible) {
			return false
		}
	}
	return strings.HasPrefix(id, "gpt-") ||
		strings.HasPrefix(id, "codex-") ||
		strings.HasPrefix(id, "o1") ||
		strings.HasPrefix(id, "o3") ||
		strings.HasPrefix(id, "o4")
}

func (a *Agent) fetchModelsFromAPI(ctx context.Context) []core.ModelOption {
	a.mu.Lock()
	extraEnv := append(a.providerEnvLocked(), a.sessionEnv...)
	a.mu.Unlock()
	env := core.MergeEnv(os.Environ(), extraEnv)
	apiKey := core.EnvValue(env, "OPENAI_API_KEY")
	baseURL := core.EnvValue(env, "OPENAI_BASE_URL")
	if apiKey == "" {
		return nil
	}
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if endpoint.Path == "" {
		endpoint.Path = "/v1"
	}
	if !strings.HasSuffix(endpoint.Path, "/models") {
		endpoint.Path += "/models"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint.String(), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := core.NoRedirectHTTPClient().Do(req)
	if err != nil {
		slog.Debug("codex: failed to fetch models", "error", err)
		return nil
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	var models []core.ModelOption
	for _, m := range result.Data {
		if isOpenAIChatModel(m.ID) {
			models = append(models, core.ModelOption{Name: m.ID})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })
	return models
}

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
	model, reasoningLevel, mode := a.startSettingsLocked()
	extraEnv := a.providerEnvLocked()
	extraEnv = append(extraEnv, a.sessionEnv...)
	workDir := core.SessionWorkDirFromEnv(extraEnv, a.workDir)
	extraDirs := core.SessionExtraDirsFromEnv(extraEnv)
	backend := a.backend
	a.mu.Unlock()

	if backend == backendAppServer {
		s, err := newAppServerSession(ctx, workDir, model, reasoningLevel, mode, sessionID, extraDirs, extraEnv)
		if err == nil {
			return s, nil
		}
		if !errors.Is(err, errAppServerUnavailable) {
			return nil, err
		}
		if mode == "plan" {
			// exec cannot plan; running the turn as default could edit.
			return nil, fmt.Errorf("codex: plan mode needs the app-server, which this Codex CLI cannot run: %w", err)
		}
		slog.Warn("codex: app-server unavailable; using codex exec for this session", "error", err)
	}
	return newCodexSession(ctx, workDir, model, reasoningLevel, mode, sessionID, extraDirs, extraEnv)
}

// ApplyLiveSettings implements core.LiveSettingsApplier: with app-server,
// model, effort and mode are turn/start parameters, so they apply from the
// next turn in the same process.
func (a *Agent) ApplyLiveSettings(_ context.Context, session core.AgentSession, opts core.AgentSessionOptions) error {
	s, ok := session.(*appServerSession)
	if !ok || !s.Alive() {
		return errors.New("no running codex app-server session")
	}
	a.mu.Lock()
	saved := a.sessionOpts
	a.sessionOpts = opts
	model, effort, mode := a.startSettingsLocked()
	a.sessionOpts = saved
	a.mu.Unlock()
	s.setSettings(model, effort, mode)
	return nil
}

// Usage implements core.UsageReporter: plan limits from
// account/rateLimits/read, asked of the running app-server or a short-lived
// one.
func (a *Agent) Usage(ctx context.Context, session core.AgentSession) (*core.UsageReport, error) {
	if s, ok := session.(*appServerSession); ok && s.Alive() {
		return s.SessionUsage(ctx)
	}
	a.mu.Lock()
	env := core.MergeEnv(os.Environ(), append(a.providerEnvLocked(), a.sessionEnv...))
	workDir := a.workDir
	a.mu.Unlock()
	proc, err := startAppServer(ctx, workDir, env)
	if err != nil {
		return nil, err
	}
	defer proc.stop()
	if err := proc.serve(ctx); err != nil {
		return nil, err
	}
	return readRateLimits(ctx, proc.rpc)
}

// EffortsForModel implements core.ModelEffortProvider from the efforts the
// last model listing reported ("" is the configured default model).
func (a *Agent) EffortsForModel(_ context.Context, model string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if model == "" {
		model = a.model
	}
	return append([]string(nil), a.efforts[model]...)
}

// startSettingsLocked resolves the model, reasoning level and permission mode
// for the next session: project defaults, then the active provider's model,
// then the chat session's explicit /model, /effort or /mode choice.
// Caller holds a.mu.
func (a *Agent) startSettingsLocked() (model, reasoningLevel, mode string) {
	model, reasoningLevel, mode = a.model, a.reasoningLevel, a.mode
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	opts := a.sessionOpts
	if opts.Model != "" {
		model = opts.Model
	}
	if opts.ReasoningLevel != "" {
		reasoningLevel = normalizeReasoningLevel(opts.ReasoningLevel)
	}
	if opts.Mode != "" {
		mode = normalizeMode(opts.Mode)
	}
	return model, reasoningLevel, mode
}

func (a *Agent) ListSessions(_ context.Context) ([]core.AgentSessionInfo, error) {
	return listCodexSessions(a.workDir)
}

func (a *Agent) GetSessionHistory(_ context.Context, sessionID string, limit int) ([]core.HistoryEntry, error) {
	return getSessionHistory(sessionID, limit)
}

func (a *Agent) DeleteSession(_ context.Context, sessionID string) error {
	path := findSessionFile(sessionID)
	if path == "" {
		return fmt.Errorf("session file not found: %s", sessionID)
	}
	return os.Remove(path)
}

func (a *Agent) Stop() error { return nil }

// SetMode changes the approval mode for future sessions.
func (a *Agent) SetMode(mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mode = normalizeMode(mode)
	slog.Info("codex: approval mode changed", "mode", a.mode)
}

func (a *Agent) GetMode() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode
}

// ── SkillProvider implementation ──────────────────────────────

func (a *Agent) SkillDirs() []string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	dirs := []string{
		filepath.Join(absDir, ".agents", "skills"),
		filepath.Join(absDir, ".codex", "skills"),
		filepath.Join(absDir, ".claude", "skills"),
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			codexHome = filepath.Join(home, ".codex")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".agents", "skills"))
	}
	if codexHome != "" {
		dirs = append(dirs, filepath.Join(codexHome, "skills"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
	}
	return dirs
}

// ── ContextCompressor implementation ──────────────────────────

// CompressCommand: compaction is native on the app-server backend
// (thread/compact/start, see appServerSession.Compact); `codex exec` has no
// way to compact, so the exec backend reports none.
func (a *Agent) CompressCommand() string {
	if a.appServer() {
		return "/compact"
	}
	return ""
}

// ── MemoryFileProvider implementation ─────────────────────────

func (a *Agent) ProjectMemoryFile() string {
	absDir, err := filepath.Abs(a.workDir)
	if err != nil {
		absDir = a.workDir
	}
	return filepath.Join(absDir, "AGENTS.md")
}

func (a *Agent) GlobalMemoryFile() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(homeDir, ".codex")
	}
	return filepath.Join(codexHome, "AGENTS.md")
}

// ── ProviderSwitcher implementation ──────────────────────────

func (a *Agent) SetProviders(providers []core.ProviderConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers = providers
}

func (a *Agent) SetActiveProvider(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if name == "" {
		a.activeIdx = -1
		slog.Info("codex: provider cleared")
		return true
	}
	for i, p := range a.providers {
		if p.Name == name {
			a.activeIdx = i
			slog.Info("codex: provider switched", "provider", name)
			return true
		}
	}
	return false
}

func (a *Agent) GetActiveProvider() *core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	return &p
}

func (a *Agent) ListProviders() []core.ProviderConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := make([]core.ProviderConfig, len(a.providers))
	copy(result, a.providers)
	return result
}

func (a *Agent) providerEnvLocked() []string {
	if a.activeIdx < 0 || a.activeIdx >= len(a.providers) {
		return nil
	}
	p := a.providers[a.activeIdx]
	var env []string
	if p.APIKey != "" {
		env = append(env, "OPENAI_API_KEY="+p.APIKey)
	}
	if p.BaseURL != "" {
		env = append(env, "OPENAI_BASE_URL="+p.BaseURL)
	}
	for k, v := range p.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func (a *Agent) PermissionModes() []core.PermissionModeInfo {
	modes := []core.PermissionModeInfo{
		{Key: "default", Name: "Default", NameZh: "默认", Desc: "No overrides: follows your Codex config (~/.codex/config.toml)", DescZh: "不加覆盖，按 ~/.codex/config.toml 的设置执行"},
		{Key: "acceptEdits", Name: "Accept Edits", NameZh: "接受编辑", Desc: "Edit and run commands inside the workspace sandbox; no escalation", DescZh: "可在工作区沙箱内编辑和执行命令，不允许越权"},
		{Key: "auto", Name: "Auto", NameZh: "自动模式", Desc: "Workspace sandbox; escalation requests reviewed automatically (--approve-for-me)", DescZh: "工作区沙箱，越权请求由 Codex 自动审核（--approve-for-me）"},
		{Key: "bypassPermissions", Name: "YOLO", NameZh: "YOLO 模式", Desc: "Bypass all approvals and sandbox", DescZh: "跳过所有审批和沙箱"},
	}
	if a.appServer() {
		modes = append(modes, core.PermissionModeInfo{Key: "plan", Name: "Plan", NameZh: "计划模式", Desc: "Plan first; Codex asks before it acts (app-server collaboration mode)", DescZh: "先做计划，执行前询问（app-server 协作模式）"})
	}
	return modes
}
