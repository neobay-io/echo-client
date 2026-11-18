package claudecode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Claude Code's stream-json control protocol (P3). echo-client sends
//
//	{"type":"control_request","request_id":"echo_N","request":{"subtype":...}}
//
// and the CLI answers with
//
//	{"type":"control_response","response":{"subtype":"success"|"error",
//	 "request_id":"echo_N","response":{...},"error":"..."}}
//
// Several subtypes are undocumented (list_models, get_usage,
// get_context_usage); every caller has a fallback, and a failure never breaks
// the chat turn.

// controlTimeout bounds one control request.
var controlTimeout = 15 * time.Second

type controlResult struct {
	response map[string]any
	err      error
}

// errSessionClosed is returned for control requests the process did not
// answer before it exited.
var errSessionClosed = errors.New("claude session closed")

// controlRequest sends a control request and waits for its response.
func (cs *claudeSession) controlRequest(ctx context.Context, request map[string]any) (map[string]any, error) {
	if !cs.alive.Load() {
		return nil, errSessionClosed
	}
	id := fmt.Sprintf("echo_%d", cs.controlSeq.Add(1))
	ch := make(chan controlResult, 1)
	cs.controlMu.Lock()
	if cs.controlPending == nil {
		cs.controlPending = make(map[string]chan controlResult)
	}
	cs.controlPending[id] = ch
	cs.controlMu.Unlock()
	defer func() {
		cs.controlMu.Lock()
		delete(cs.controlPending, id)
		cs.controlMu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	// The write is bounded too: a CLI that stopped reading stdin must not
	// hang the caller (e.g. /stop, which then falls back to the kill).
	written := make(chan error, 1)
	go func() {
		written <- cs.writeJSON(map[string]any{"type": "control_request", "request_id": id, "request": request})
	}()
	select {
	case err := <-written:
		if err != nil {
			return nil, err
		}
	case <-cs.done:
		return nil, errSessionClosed
	case <-ctx.Done():
		return nil, fmt.Errorf("claude control request %v: writing: %w", request["subtype"], ctx.Err())
	}
	select {
	case res := <-ch:
		return res.response, res.err
	case <-cs.done:
		return nil, errSessionClosed
	case <-ctx.Done():
		return nil, fmt.Errorf("claude control request %v: %w", request["subtype"], ctx.Err())
	}
}

// handleControlResponse routes a control_response to its waiting request.
func (cs *claudeSession) handleControlResponse(raw map[string]any) {
	resp, _ := raw["response"].(map[string]any)
	if resp == nil {
		return
	}
	id, _ := resp["request_id"].(string)
	cs.controlMu.Lock()
	ch := cs.controlPending[id]
	cs.controlMu.Unlock()
	if ch == nil {
		return
	}
	res := controlResult{}
	if subtype, _ := resp["subtype"].(string); subtype != "success" {
		msg, _ := resp["error"].(string)
		if msg == "" {
			msg = "request failed"
		}
		res.err = errors.New(msg)
	} else {
		res.response, _ = resp["response"].(map[string]any)
	}
	select {
	case ch <- res:
	default:
	}
}

// ── Live settings (T3.1) ──────────────────────────────────────

// applyLive switches model, effort and permission mode of the running
// process. Only values that differ from what it runs are sent.
func (cs *claudeSession) applyLive(ctx context.Context, model, effort, mode string) error {
	cs.settingsMu.Lock()
	defer cs.settingsMu.Unlock()
	if model != cs.model {
		target := model
		if target == "" {
			target = "default"
		}
		if _, err := cs.controlRequest(ctx, map[string]any{"subtype": "set_model", "model": target}); err != nil {
			return fmt.Errorf("set model: %w", err)
		}
		cs.model = model
	}
	if effort != cs.effort {
		if effort == "" {
			// No request clears a flag-set effort; a restart does.
			return errors.New("clearing the effort needs a new process")
		}
		if _, err := cs.controlRequest(ctx, map[string]any{"subtype": "apply_flag_settings", "settings": map[string]any{"effortLevel": effort}}); err != nil {
			return fmt.Errorf("set effort: %w", err)
		}
		cs.effort = effort
	}
	if mode != cs.mode {
		target := mode
		if target == "" {
			target = "default"
		}
		bypass := mode == "bypassPermissions"
		if !bypass {
			// Stop auto-approving before the CLI switches: a tool request
			// right after its acknowledgement must be asked, not approved.
			cs.autoApprove.Store(false)
		}
		if _, err := cs.controlRequest(ctx, map[string]any{"subtype": "set_permission_mode", "mode": target}); err != nil {
			// Unknown whether the CLI switched; stay on the safe side. The
			// engine restarts the process with the chosen mode.
			return fmt.Errorf("set permission mode: %w", err)
		}
		cs.mode = mode
		cs.autoApprove.Store(bypass)
	}
	return nil
}

// ApplyLiveSettings implements core.LiveSettingsApplier: /model, /effort and
// /mode take effect in the running process, which keeps its conversation.
func (a *Agent) ApplyLiveSettings(ctx context.Context, session core.AgentSession, opts core.AgentSessionOptions) error {
	cs, ok := session.(*claudeSession)
	if !ok || !cs.Alive() {
		return errors.New("no running Claude process")
	}
	a.mu.Lock()
	saved := a.sessionOpts
	a.sessionOpts = opts
	model, effort, mode := a.startSettingsLocked()
	a.sessionOpts = saved
	a.mu.Unlock()
	return cs.applyLive(ctx, model, effort, mode)
}

// ── Steering and interrupt (T3.3, T3.4) ───────────────────────

// Steer implements core.Steerer: the message is read by the running turn
// after its current tool call ("priority":"next").
func (cs *claudeSession) Steer(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	return cs.sendUserMessage(prompt, images, files, "next")
}

// Interrupt implements core.Interrupter: the running turn stops, the process
// and its conversation stay.
func (cs *claudeSession) Interrupt(ctx context.Context) error {
	if cs.promptWaiting() {
		// Our prompt waits behind a turn Claude started itself: interrupt
		// would stop that one and ours would run afterwards. The engine
		// stops the process instead.
		return errors.New("the prompt has not started yet")
	}
	cs.interrupting.Store(true)
	if _, err := cs.controlRequest(ctx, map[string]any{"subtype": "interrupt"}); err != nil {
		cs.interrupting.Store(false)
		return err
	}
	return nil
}

// ── Usage and context (T3.6) ──────────────────────────────────

// ContextUsage implements core.ContextReporter.
func (cs *claudeSession) ContextUsage(ctx context.Context) (*core.ContextReport, error) {
	resp, err := cs.controlRequest(ctx, map[string]any{"subtype": "get_context_usage"})
	if err != nil {
		return nil, err
	}
	report := &core.ContextReport{
		UsedTokens: intField(resp, "totalTokens"),
		MaxTokens:  intField(resp, "maxTokens"),
		Percent:    intField(resp, "percentage"),
	}
	if cats, ok := resp["categories"].([]any); ok {
		for _, c := range cats {
			m, _ := c.(map[string]any)
			if m == nil || m["kind"] == "deferred" {
				continue
			}
			name, _ := m["name"].(string)
			if tokens := intField(m, "tokens"); name != "" && tokens > 0 {
				report.Categories = append(report.Categories, core.ContextCategory{Name: name, Tokens: tokens})
			}
		}
	}
	return report, nil
}

// usage reads get_usage from a running process.
func (cs *claudeSession) usage(ctx context.Context) (*core.UsageReport, error) {
	resp, err := cs.controlRequest(ctx, map[string]any{"subtype": "get_usage"})
	if err != nil {
		return nil, err
	}
	return parseClaudeUsage(resp), nil
}

func parseClaudeUsage(resp map[string]any) *core.UsageReport {
	report := &core.UsageReport{}
	if session, ok := resp["session"].(map[string]any); ok {
		if cost, ok := session["total_cost_usd"].(float64); ok {
			report.SessionCostUSD = cost
			report.HasCost = true
		}
	}
	if plan, _ := resp["subscription_type"].(string); plan != "" {
		report.Plan = plan
	}
	limits, _ := resp["rate_limits"].(map[string]any)
	for _, w := range []struct{ key, label string }{
		{"five_hour", "5-hour limit"},
		{"seven_day", "7-day limit"},
		{"seven_day_opus", "7-day Opus limit"},
		{"seven_day_sonnet", "7-day Sonnet limit"},
	} {
		m, _ := limits[w.key].(map[string]any)
		if m == nil {
			continue
		}
		line := core.UsageLine{Name: w.label, UsedPercent: -1}
		if u, ok := m["utilization"].(float64); ok {
			line.UsedPercent = int(u)
		}
		if s, _ := m["resets_at"].(string); s != "" {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				line.ResetsAt = t
			}
		}
		if reason, _ := m["locked_reason"].(string); reason != "" {
			line.Note = reason
		}
		report.Lines = append(report.Lines, line)
	}
	return report
}

func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// Usage implements core.UsageReporter. A running process answers directly;
// otherwise a short-lived control-only process is started (no prompt, so no
// model request).
func (a *Agent) Usage(ctx context.Context, session core.AgentSession) (*core.UsageReport, error) {
	if cs, ok := session.(*claudeSession); ok && cs.Alive() {
		if report, err := cs.usage(ctx); err == nil {
			return report, nil
		} else {
			slog.Debug("claudecode: get_usage on the running process failed", "error", err)
		}
	}
	resp, err := a.controlOnly(ctx, map[string]any{"subtype": "get_usage"})
	if err != nil {
		return nil, err
	}
	return parseClaudeUsage(resp), nil
}

// controlOnly runs one control request in a fresh Claude process started in
// a temporary directory, with no prompt, then stops it.
func (a *Agent) controlOnly(ctx context.Context, request map[string]any) (map[string]any, error) {
	return runControlOnly(ctx, a, request)
}

// runControlOnly is replaced in tests, which must never start Claude.
var runControlOnly = func(ctx context.Context, a *Agent, request map[string]any) (map[string]any, error) {
	return a.startControlOnly(ctx, request)
}

func (a *Agent) startControlOnly(ctx context.Context, request map[string]any) (map[string]any, error) {
	a.mu.Lock()
	extraEnv := a.processEnvLocked()
	a.mu.Unlock()
	dir, err := tempWorkDir()
	if err != nil {
		return nil, err
	}
	defer removeTempWorkDir(dir)
	cs, err := newClaudeSession(ctx, dir, "", "", "", "", nil, nil, extraEnv)
	if err != nil {
		return nil, err
	}
	defer func() { _ = cs.Close() }()
	return cs.controlRequest(ctx, request)
}

// ── Models (T3.2) ─────────────────────────────────────────────

// claudeModel is one entry of list_models.
type claudeModel struct {
	Value       string
	Display     string
	Description string
	Efforts     []string
}

func parseClaudeModels(resp map[string]any) []claudeModel {
	list, _ := resp["models"].([]any)
	var models []claudeModel
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		value, _ := m["value"].(string)
		if strings.TrimSpace(value) == "" {
			continue
		}
		cm := claudeModel{Value: value}
		cm.Display, _ = m["displayName"].(string)
		cm.Description, _ = m["description"].(string)
		if efforts, ok := m["supportedEffortLevels"].([]any); ok {
			for _, e := range efforts {
				if s, _ := e.(string); s != "" {
					cm.Efforts = append(cm.Efforts, s)
				}
			}
		}
		models = append(models, cm)
	}
	return models
}

// modelCatalogTTL is how long a list_models answer is reused.
const modelCatalogTTL = 10 * time.Minute

// listModels returns Claude's model list, from cache or a control-only
// process. Failures return nil; callers fall back to older discovery.
func (a *Agent) listModels(ctx context.Context) []claudeModel {
	a.modelsMu.Lock()
	if time.Since(a.modelsAt) < modelCatalogTTL && len(a.models) > 0 {
		models := a.models
		a.modelsMu.Unlock()
		return models
	}
	a.modelsMu.Unlock()

	fetch := a.listModelsFn
	if fetch == nil {
		fetch = func(ctx context.Context) ([]claudeModel, error) {
			resp, err := a.controlOnly(ctx, map[string]any{"subtype": "list_models"})
			if err != nil {
				return nil, err
			}
			return parseClaudeModels(resp), nil
		}
	}
	models, err := fetch(ctx)
	if err != nil || len(models) == 0 {
		slog.Debug("claudecode: list_models failed", "error", err)
		return nil
	}
	a.modelsMu.Lock()
	a.models, a.modelsAt = models, time.Now()
	a.modelsMu.Unlock()
	return models
}

// InvalidateModelCache implements core.ModelCacheInvalidator.
func (a *Agent) InvalidateModelCache() {
	a.modelsMu.Lock()
	a.modelsAt = time.Time{}
	a.modelsMu.Unlock()
}

// EffortsForModel implements core.ModelEffortProvider: the effort levels the
// given model supports, from the cached list_models answer ("" is Claude's
// default model). It never waits for a CLI: with no fresh list it starts a
// refresh in the background and returns nil (the static levels are shown).
func (a *Agent) EffortsForModel(_ context.Context, model string) []string {
	if model == "" {
		model = "default"
	}
	a.modelsMu.Lock()
	models, fresh := a.models, time.Since(a.modelsAt) < modelCatalogTTL
	refreshing := a.modelsRefreshing
	if !fresh && !refreshing {
		a.modelsRefreshing = true
	}
	a.modelsMu.Unlock()
	if !fresh && !refreshing {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), controlTimeout+10*time.Second)
			defer cancel()
			a.listModels(ctx)
			a.modelsMu.Lock()
			a.modelsRefreshing = false
			a.modelsMu.Unlock()
		}()
	}
	for _, m := range models {
		if strings.EqualFold(m.Value, model) {
			return append([]string(nil), m.Efforts...)
		}
	}
	return nil
}

func tempWorkDir() (string, error) {
	return os.MkdirTemp("", "echo-claude-control-")
}

func removeTempWorkDir(dir string) {
	_ = os.RemoveAll(dir)
}

// SessionUsage implements core.SessionUsageReporter (/status): what this
// process has spent, from get_usage.
func (cs *claudeSession) SessionUsage(ctx context.Context) (*core.UsageReport, error) {
	return cs.usage(ctx)
}
