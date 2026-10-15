package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func init() {
	core.RegisterAgent("codex", New)
}

// Agent drives OpenAI Codex CLI using `codex exec --json`.
//
// Modes (maps to codex exec flags):
//   - "suggest":   default, no special flags (safe commands only)
//   - "auto-edit": --full-auto (sandbox-protected auto execution)
//   - "full-auto": --full-auto (sandbox-protected auto execution)
//   - "yolo":      --dangerously-bypass-approvals-and-sandbox
type Agent struct {
	workDir        string
	model          string
	reasoningLevel string
	mode           string // "suggest" | "auto-edit" | "full-auto" | "yolo"
	providers      []core.ProviderConfig
	activeIdx      int // -1 = no provider set
	sessionEnv     []string
	modelRunner    codexModelRunner
	mu             sync.Mutex
}

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

	return &Agent{
		workDir:        workDir,
		model:          model,
		reasoningLevel: reasoningLevel,
		mode:           mode,
		activeIdx:      -1,
	}, nil
}

func normalizeMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "auto-edit", "autoedit", "auto_edit", "edit":
		return "auto-edit"
	case "full-auto", "fullauto", "full_auto", "auto":
		return "full-auto"
	case "yolo", "bypass", "dangerously-bypass":
		return "yolo"
	default:
		return "suggest"
	}
}

func normalizeReasoningLevel(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low":
		return "low"
	case "medium", "med":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "x-high", "extra-high", "extra_high", "very-high":
		return "xhigh"
	default:
		return ""
	}
}

func (a *Agent) Name() string { return "codex" }

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
				Model       string `json:"model"`
				DisplayName string `json:"displayName"`
				IsDefault   bool   `json:"isDefault"`
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
			models = append(models, core.ModelOption{Name: name, Desc: desc})
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

func (a *Agent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	a.mu.Lock()
	mode := a.mode
	model := a.model
	reasoningLevel := a.reasoningLevel
	extraEnv := a.providerEnvLocked()
	extraEnv = append(extraEnv, a.sessionEnv...)
	workDir := core.SessionWorkDirFromEnv(extraEnv, a.workDir)
	extraDirs := core.SessionExtraDirsFromEnv(extraEnv)
	if a.activeIdx >= 0 && a.activeIdx < len(a.providers) {
		if m := a.providers[a.activeIdx].Model; m != "" {
			model = m
		}
	}
	a.mu.Unlock()

	return newCodexSession(ctx, workDir, model, reasoningLevel, mode, sessionID, extraDirs, extraEnv)
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

func (a *Agent) CompressCommand() string { return "/compact" }

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
	return []core.PermissionModeInfo{
		{Key: "suggest", Name: "Suggest", NameZh: "建议", Desc: "Ask permission for every tool call", DescZh: "每次工具调用都需确认"},
		{Key: "auto-edit", Name: "Auto Edit", NameZh: "自动编辑", Desc: "Auto-approve file edits, ask for shell commands", DescZh: "自动允许文件编辑，Shell 命令需确认"},
		{Key: "full-auto", Name: "Full Auto", NameZh: "全自动", Desc: "Auto-approve with workspace sandbox", DescZh: "自动通过（工作区沙箱）"},
		{Key: "yolo", Name: "YOLO", NameZh: "YOLO 模式", Desc: "Bypass all approvals and sandbox", DescZh: "跳过所有审批和沙箱"},
	}
}
