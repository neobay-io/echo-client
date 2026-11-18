package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AgentUpgradeLastResult is the outcome of the latest upgrade attempt of one
// target, kept across restarts.
type AgentUpgradeLastResult struct {
	At            time.Time `json:"at"`
	Automatic     bool      `json:"automatic"`
	Skipped       bool      `json:"skipped,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	BeforeVersion string    `json:"before_version,omitempty"`
	AfterVersion  string    `json:"after_version,omitempty"`
	Changed       bool      `json:"changed,omitempty"`
	LatestVersion string    `json:"latest_version,omitempty"`
	RolledBack    bool      `json:"rolled_back,omitempty"`
	Error         string    `json:"error,omitempty"`
	BinaryPath    string    `json:"binary_path,omitempty"`
}

// AgentUpgradeNotifyChat is a chat that receives upgrade notifications.
type AgentUpgradeNotifyChat struct {
	Project    string `json:"project"`
	SessionKey string `json:"session_key"`
}

type agentUpgradeState struct {
	LastAutoRun time.Time                           `json:"last_auto_run,omitempty"`
	NextAutoRun time.Time                           `json:"next_auto_run,omitempty"`
	Failures    map[string]agentUpgradeFailureState `json:"failures,omitempty"`
	LastResults map[string]AgentUpgradeLastResult   `json:"last_results,omitempty"`
	Notify      []AgentUpgradeNotifyChat            `json:"notify,omitempty"`
	InFlight    map[string]upgradeSnapshot          `json:"in_flight,omitempty"`
}

// SetStatePath enables persistence of the schedule, failure backoff, last
// results and notification chats, and loads any existing state. Call before
// Start.
func (m *AgentUpgradeManager) SetStatePath(path string) {
	m.mu.Lock()
	m.statePath = path
	m.mu.Unlock()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("agent upgrade: failed to read state", "path", path, "error", err)
		}
		return
	}
	var state agentUpgradeState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("agent upgrade: ignoring unreadable state", "path", path, "error", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastAutoRun = state.LastAutoRun
	m.nextAutoRun = state.NextAutoRun
	for name, failure := range state.Failures {
		m.failures[name] = failure
	}
	for name, result := range state.LastResults {
		m.lastResults[name] = result
	}
	m.notifyChats = append([]AgentUpgradeNotifyChat(nil), state.Notify...)
	for name, snap := range state.InFlight {
		m.inFlight[name] = snap
	}
}

// SetNotifier sets how notifications reach a chat (wired to the engine of the
// chat's project).
func (m *AgentUpgradeManager) SetNotifier(fn func(project, sessionKey, text string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifyFn = fn
}

func (m *AgentUpgradeManager) saveState() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	path := m.statePath
	state := agentUpgradeState{
		LastAutoRun: m.lastAutoRun,
		NextAutoRun: m.nextAutoRun,
		Failures:    make(map[string]agentUpgradeFailureState, len(m.failures)),
		LastResults: make(map[string]AgentUpgradeLastResult, len(m.lastResults)),
		Notify:      append([]AgentUpgradeNotifyChat(nil), m.notifyChats...),
		InFlight:    make(map[string]upgradeSnapshot, len(m.inFlight)),
	}
	for name, snap := range m.inFlight {
		state.InFlight[name] = snap
	}
	for name, failure := range m.failures {
		state.Failures[name] = failure
	}
	for name, result := range m.lastResults {
		state.LastResults[name] = result
	}
	m.mu.Unlock()
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Warn("agent upgrade: failed to create state dir", "error", err)
		return
	}
	if err := AtomicWriteFile(path, data, 0o600); err != nil {
		slog.Warn("agent upgrade: failed to save state", "path", path, "error", err)
	}
}

// LastResult returns the persisted outcome of the latest attempt for name.
func (m *AgentUpgradeManager) LastResult(name string) (AgentUpgradeLastResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result, ok := m.lastResults[name]
	return result, ok
}

func (m *AgentUpgradeManager) recordResult(res AgentUpgradeRunResult, automatic bool, binaryPath string) {
	entry := AgentUpgradeLastResult{
		At:            time.Now(),
		Automatic:     automatic,
		Skipped:       res.Skipped,
		Reason:        res.Reason,
		BeforeVersion: res.BeforeVersion,
		AfterVersion:  res.AfterVersion,
		Changed:       res.Changed,
		LatestVersion: res.LatestVersion,
		RolledBack:    res.RolledBack,
		BinaryPath:    binaryPath,
	}
	if res.Err != nil {
		entry.Error = res.Err.Error()
	}
	m.mu.Lock()
	m.lastResults[res.Name] = entry
	m.mu.Unlock()
}

// NotifyChats lists the chats that receive upgrade notifications.
func (m *AgentUpgradeManager) NotifyChats() []AgentUpgradeNotifyChat {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AgentUpgradeNotifyChat(nil), m.notifyChats...)
}

// SetNotifyChat adds or removes a chat from the notification list.
func (m *AgentUpgradeManager) SetNotifyChat(chat AgentUpgradeNotifyChat, enabled bool) {
	m.mu.Lock()
	kept := m.notifyChats[:0:0]
	for _, existing := range m.notifyChats {
		if existing != chat {
			kept = append(kept, existing)
		}
	}
	if enabled {
		kept = append(kept, chat)
	}
	m.notifyChats = kept
	m.mu.Unlock()
	m.saveState()
}

// AuthorizeAdmin checks userID against [agent_updates].allowed_user_ids.
func (m *AgentUpgradeManager) AuthorizeAdmin(userID string) error {
	cfg := m.Snapshot()
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("a resolvable user id is required")
	}
	for _, allowed := range cfg.AllowedUserIDs {
		if strings.EqualFold(strings.TrimSpace(allowed), userID) {
			return nil
		}
	}
	return fmt.Errorf("you are not in [agent_updates].allowed_user_ids")
}

// notifyResults tells registered chats about upgrades that changed a version
// or failed. Skips and no-op runs stay quiet.
func (m *AgentUpgradeManager) notifyResults(results []AgentUpgradeRunResult, automatic bool) {
	text := upgradeNotificationText(results, automatic)
	if text == "" {
		return
	}
	m.mu.Lock()
	fn := m.notifyFn
	chats := append([]AgentUpgradeNotifyChat(nil), m.notifyChats...)
	m.mu.Unlock()
	if fn == nil {
		return
	}
	for _, chat := range chats {
		if err := fn(chat.Project, chat.SessionKey, text); err != nil {
			slog.Warn("agent upgrade: notification failed", "project", chat.Project, "error", err)
		}
	}
}

func upgradeNotificationText(results []AgentUpgradeRunResult, automatic bool) string {
	var lines []string
	for _, res := range results {
		switch {
		case res.Err != nil:
			lines = append(lines, fmt.Sprintf("⚠️ %s upgrade failed: %s", res.Name, firstLine(res.Err.Error(), 200)))
		case res.Changed:
			lines = append(lines, fmt.Sprintf("✅ %s upgraded: %s → %s", res.Name, res.BeforeVersion, res.AfterVersion))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	header := "Agent CLI upgrade"
	if automatic {
		header += " (automatic)"
	}
	return header + "\n" + strings.Join(lines, "\n")
}
