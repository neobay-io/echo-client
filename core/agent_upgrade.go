package core

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultAgentUpgradeTimeout = 20 * time.Minute
const defaultAgentUpgradeProbeTimeout = 30 * time.Second
const defaultAgentUpgradeInterval = 24 * time.Hour

type AgentUpgradeTargetConfig struct {
	Enabled        bool
	Strategy       string
	Channel        string
	VersionCommand string
	UpdateCommand  string
	MinimumVersion string
	PinVersion     string
	FailureBackoff time.Duration
	MaxFailures    int
	// *Set mark commands the user configured explicitly; only defaults are
	// replaced by the commands derived from the detected install route.
	VersionCommandSet bool
	UpdateCommandSet  bool
}

type AgentUpgradeConfig struct {
	Enabled        bool
	RunEnabled     bool
	AllowedUserIDs []string
	Policy         string
	Interval       time.Duration
	Timeout        time.Duration
	// Window restricts automatic runs to a local time range "HH:MM-HH:MM"
	// (may wrap midnight). Empty means any time.
	Window  string
	Targets map[string]AgentUpgradeTargetConfig
}

type AgentUpgradeStatus struct {
	Name                string
	Enabled             bool
	Strategy            string
	Channel             string
	MinimumVersion      string
	PinVersion          string
	Actionable          bool
	BlockedReason       string
	BusySessions        int
	Version             string
	VersionErr          string
	UpdateCommand       string
	ConsecutiveFailures int
	LastFailureReason   string
	BackoffUntil        time.Time
	// Where the CLI resolves for agent processes and how it was installed.
	BinaryPath   string
	ResolvedPath string
	InstallRoute string
	Warning      string
	LastResult   *AgentUpgradeLastResult
}

type AgentUpgradeRunResult struct {
	Name          string
	Strategy      string
	BusySessions  int
	Skipped       bool
	Reason        string
	BeforeVersion string
	AfterVersion  string
	Changed       bool
	Output        string
	Err           error
}

type resolvedAgentUpgradeTarget struct {
	cfg           AgentUpgradeTargetConfig
	blockedReason string
	info          AgentCLIInfo
	route         string
	warning       string
}

type agentUpgradeFailureState struct {
	ConsecutiveFailures int
	LastFailureReason   string
	LastFailureAt       time.Time
	BackoffUntil        time.Time
}

type AgentUpgradeCommandRunner func(ctx context.Context, cmd string) (string, error)

type AgentUpgradeManager struct {
	mu          sync.Mutex
	cfg         AgentUpgradeConfig
	runCommand  AgentUpgradeCommandRunner
	busyCountFn func(string) int
	inspectCLI  func(context.Context, string) AgentCLIInfo
	running     map[string]bool
	failures    map[string]agentUpgradeFailureState
	lastResults map[string]AgentUpgradeLastResult
	notifyChats []AgentUpgradeNotifyChat
	notifyFn    func(project, sessionKey, text string) error
	statePath   string
	lockDir     string
	lastAutoRun time.Time
	nextAutoRun time.Time
	// Delay before a run that was missed while the service was down, so a
	// restart does not upgrade before chats reconnect.
	startupGrace time.Duration
	startOnce    sync.Once
	wakeCh       chan struct{}
	loopDone     chan struct{}
}

const defaultAgentUpgradeStartupGrace = 2 * time.Minute

func NewAgentUpgradeManager(cfg AgentUpgradeConfig) *AgentUpgradeManager {
	m := &AgentUpgradeManager{
		runCommand:   defaultAgentUpgradeCommandRunner,
		running:      make(map[string]bool),
		failures:     make(map[string]agentUpgradeFailureState),
		lastResults:  make(map[string]AgentUpgradeLastResult),
		lockDir:      defaultAgentUpgradeLockDir(),
		startupGrace: defaultAgentUpgradeStartupGrace,
		wakeCh:       make(chan struct{}, 1),
		loopDone:     make(chan struct{}),
	}
	m.ApplyConfig(cfg)
	return m
}

// SetCLIInspector replaces how agent CLIs are located (tests).
func (m *AgentUpgradeManager) SetCLIInspector(fn func(context.Context, string) AgentCLIInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inspectCLI = fn
}

func DefaultAgentUpgradeConfig() AgentUpgradeConfig {
	return AgentUpgradeConfig{
		Enabled:    false,
		RunEnabled: false,
		Policy:     "off",
		Interval:   defaultAgentUpgradeInterval,
		Timeout:    defaultAgentUpgradeTimeout,
		Targets: map[string]AgentUpgradeTargetConfig{
			"claudecode": {
				Enabled:        true,
				Strategy:       "builtin",
				VersionCommand: "claude --version",
				UpdateCommand:  "claude update",
			},
			"codex": {
				Enabled:        true,
				Strategy:       "package_manager",
				VersionCommand: "codex --version",
				UpdateCommand:  "npm install -g @openai/codex@latest",
			},
			"gemini": {
				Enabled:        true,
				Strategy:       "observe",
				VersionCommand: "gemini --version",
				UpdateCommand:  "npm install -g @google/gemini-cli@latest",
			},
			"qoder": {
				Enabled:        true,
				Strategy:       "builtin",
				VersionCommand: "qodercli --version",
				UpdateCommand:  "qodercli update",
			},
			"kimi": {
				Enabled:        true,
				Strategy:       "builtin",
				VersionCommand: "kimi --version",
				UpdateCommand:  "kimi upgrade --yes",
			},
			"antigravity": {
				Enabled:        true,
				Strategy:       "builtin",
				VersionCommand: "agy --version",
				UpdateCommand:  "agy update",
			},
		},
	}
}

func (m *AgentUpgradeManager) SetBusyCountFunc(fn func(string) int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busyCountFn = fn
}

func (m *AgentUpgradeManager) SetCommandRunner(fn AgentUpgradeCommandRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn == nil {
		m.runCommand = defaultAgentUpgradeCommandRunner
		return
	}
	m.runCommand = fn
}

func (m *AgentUpgradeManager) ApplyConfig(cfg AgentUpgradeConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	base := DefaultAgentUpgradeConfig()
	base.Enabled = cfg.Enabled
	base.RunEnabled = cfg.RunEnabled
	base.AllowedUserIDs = dedupeTrimmedStrings(cfg.AllowedUserIDs)
	if cfg.Policy != "" {
		base.Policy = normalizeAgentUpgradePolicy(cfg.Policy)
	}
	if cfg.Interval > 0 {
		base.Interval = cfg.Interval
	}
	if cfg.Timeout > 0 {
		base.Timeout = cfg.Timeout
	}
	base.Window = strings.TrimSpace(cfg.Window)
	if len(cfg.Targets) > 0 {
		for name, override := range cfg.Targets {
			name = normalizeAgentUpgradeTarget(name)
			if name == "" {
				continue
			}
			target := base.Targets[name]
			target.Enabled = override.Enabled
			if override.Strategy != "" {
				target.Strategy = normalizeAgentUpgradeStrategy(override.Strategy)
			}
			if override.Channel != "" {
				target.Channel = normalizeAgentUpgradeChannel(override.Channel)
			}
			if strings.TrimSpace(override.VersionCommand) != "" {
				target.VersionCommand = strings.TrimSpace(override.VersionCommand)
			}
			if strings.TrimSpace(override.UpdateCommand) != "" {
				target.UpdateCommand = strings.TrimSpace(override.UpdateCommand)
			}
			target.VersionCommandSet = override.VersionCommandSet
			target.UpdateCommandSet = override.UpdateCommandSet
			if minVersion := strings.TrimSpace(override.MinimumVersion); minVersion != "" {
				target.MinimumVersion = minVersion
			}
			if pinVersion := strings.TrimSpace(override.PinVersion); pinVersion != "" {
				target.PinVersion = pinVersion
			}
			if override.FailureBackoff > 0 {
				target.FailureBackoff = override.FailureBackoff
			}
			if override.MaxFailures > 0 {
				target.MaxFailures = override.MaxFailures
			}
			base.Targets[name] = target
		}
	}

	m.cfg = base
	select {
	case m.wakeCh <- struct{}{}:
	default:
	}
}

func (m *AgentUpgradeManager) Snapshot() AgentUpgradeConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.cfg
	cfg.AllowedUserIDs = append([]string(nil), cfg.AllowedUserIDs...)
	cfg.Targets = cloneAgentUpgradeTargets(cfg.Targets)
	return cfg
}

func (m *AgentUpgradeManager) AuthorizeRun(userID string) error {
	cfg := m.Snapshot()
	if !cfg.RunEnabled {
		return fmt.Errorf("interactive agent upgrades are disabled; set [agent_updates].run_enabled = true")
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("interactive agent upgrades require a resolvable user id")
	}
	for _, allowed := range cfg.AllowedUserIDs {
		if strings.EqualFold(strings.TrimSpace(allowed), userID) {
			return nil
		}
	}
	return fmt.Errorf("you are not allowed to run agent upgrades")
}

// IsTargetRunning reports whether name is being upgraded by this process or by
// another echo-client process sharing the lock directory; new turns for that
// agent wait meanwhile.
func (m *AgentUpgradeManager) IsTargetRunning(name string) bool {
	name = normalizeAgentUpgradeTarget(name)
	if name == "" {
		return false
	}
	m.mu.Lock()
	running := m.running[name]
	dir := m.lockDir
	m.mu.Unlock()
	return running || upgradeLockHeld(dir, name)
}

func (m *AgentUpgradeManager) lockDirPath() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lockDir
}

// SetLockDir overrides where cross-process upgrade locks live (tests).
func (m *AgentUpgradeManager) SetLockDir(dir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockDir = dir
}

func (m *AgentUpgradeManager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		go func() {
			defer close(m.loopDone)
			m.loop(ctx)
		}()
	})
}

func (m *AgentUpgradeManager) ScheduleState() (time.Time, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastAutoRun, m.nextAutoRun
}

func (m *AgentUpgradeManager) AutoCheck(ctx context.Context) ([]AgentUpgradeRunResult, error) {
	cfg := m.Snapshot()
	if !cfg.Enabled {
		return nil, nil
	}
	switch cfg.Policy {
	case "check_only":
		statuses := m.Statuses(ctx)
		for _, st := range statuses {
			slog.Info("agent upgrade check",
				"agent", st.Name,
				"strategy", st.Strategy,
				"actionable", st.Actionable,
				"busy_sessions", st.BusySessions,
				"version", st.Version,
				"version_error", st.VersionErr,
				"blocked_reason", st.BlockedReason,
			)
		}
		return nil, nil
	case "idle_only":
		return m.runWithOptions(ctx, "all", true)
	default:
		return nil, nil
	}
}

func (m *AgentUpgradeManager) loop(ctx context.Context) {
	nextRun := time.Time{}
	for {
		cfg := m.Snapshot()
		if !cfg.Enabled || cfg.Policy == "off" {
			nextRun = time.Time{}
			m.updateScheduleState(time.Time{}, nextRun)
			select {
			case <-ctx.Done():
				return
			case <-m.wakeCh:
				continue
			}
		}

		interval := cfg.Interval
		if interval <= 0 {
			interval = defaultAgentUpgradeInterval
		}
		if nextRun.IsZero() && cfg.Enabled && cfg.Policy != "off" {
			window, werr := parseUpgradeWindow(cfg.Window)
			if werr != nil {
				slog.Warn("agent upgrade: ignoring invalid window", "error", werr)
			}
			m.mu.Lock()
			persistedNext, lastRun, grace := m.nextAutoRun, m.lastAutoRun, m.startupGrace
			m.mu.Unlock()
			nextRun = computeNextAutoRun(time.Now(), persistedNext, lastRun, interval, grace, window)
		}
		m.updateScheduleState(time.Time{}, nextRun)
		m.saveState()

		wait := time.Until(nextRun)
		if wait < 0 {
			wait = 0
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.wakeCh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			nextRun = time.Time{}
			continue
		case <-timer.C:
		}

		cfg = m.Snapshot()
		if !cfg.Enabled || cfg.Policy == "off" {
			nextRun = time.Time{}
			m.updateScheduleState(time.Time{}, nextRun)
			continue
		}
		results, err := m.AutoCheck(ctx)
		completedAt := time.Now()
		if err != nil {
			slog.Warn("agent upgrade auto-check failed", "error", err)
		} else {
			for _, res := range results {
				attrs := []any{
					"agent", res.Name,
					"strategy", res.Strategy,
					"busy_sessions", res.BusySessions,
					"skipped", res.Skipped,
					"reason", res.Reason,
					"before", res.BeforeVersion,
					"after", res.AfterVersion,
					"changed", res.Changed,
				}
				if res.Err != nil {
					// Previously the error was dropped, so failures could not
					// be diagnosed from the logs.
					attrs = append(attrs, "error", res.Err, "output_tail", outputTail(res.Output, 400))
					slog.Warn("agent upgrade auto-run failed", attrs...)
					continue
				}
				slog.Info("agent upgrade auto-run", attrs...)
			}
		}
		nextRun = time.Time{}
		m.updateScheduleState(completedAt, nextRun)
		m.saveState()
	}
}

// outputTail keeps the end of command output, where installers print errors.
func outputTail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

func (m *AgentUpgradeManager) Statuses(ctx context.Context) []AgentUpgradeStatus {
	cfg, runner, busyFn := m.snapshotDeps()
	names := sortedAgentUpgradeTargetNames(cfg.Targets)
	statuses := make([]AgentUpgradeStatus, 0, len(names))
	for _, name := range names {
		resolved := m.resolveTarget(ctx, name, cfg.Targets[name], runner)
		target := resolved.cfg
		failureState := m.failureState(name)
		blockedReason := resolved.blockedReason
		if backoffReason := targetBackoffReason(failureState); backoffReason != "" {
			blockedReason = joinBlockedReasons(blockedReason, backoffReason)
		}
		st := AgentUpgradeStatus{
			Name:                name,
			Enabled:             cfg.Enabled && target.Enabled,
			Strategy:            target.Strategy,
			Channel:             target.Channel,
			MinimumVersion:      target.MinimumVersion,
			PinVersion:          target.PinVersion,
			Actionable:          cfg.Enabled && target.Enabled && blockedReason == "" && target.Strategy != "observe" && target.Strategy != "disabled" && strings.TrimSpace(target.UpdateCommand) != "",
			BlockedReason:       blockedReason,
			UpdateCommand:       target.UpdateCommand,
			ConsecutiveFailures: failureState.ConsecutiveFailures,
			LastFailureReason:   failureState.LastFailureReason,
			BackoffUntil:        failureState.BackoffUntil,
			InstallRoute:        resolved.route,
			Warning:             resolved.warning,
		}
		if sel := resolved.info.Selected; sel != nil {
			st.BinaryPath = sel.Path
			st.ResolvedPath = sel.Resolved
		}
		if last, ok := m.LastResult(name); ok {
			st.LastResult = &last
		}
		if busyFn != nil {
			st.BusySessions = busyFn(name)
		}
		if strings.TrimSpace(target.VersionCommand) != "" {
			timeout := versionProbeTimeout(cfg.Timeout)
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			out, err := runner(runCtx, target.VersionCommand)
			cancel()
			if err != nil {
				st.VersionErr = err.Error()
			} else {
				st.Version = normalizeAgentUpgradeOutput(out)
				if policyReason := evaluateTargetVersionPolicy(target, st.Version); policyReason != "" {
					st.BlockedReason = joinBlockedReasons(st.BlockedReason, policyReason)
				}
			}
		}
		st.Actionable = cfg.Enabled && target.Enabled && st.BlockedReason == "" && target.Strategy != "observe" && target.Strategy != "disabled" && strings.TrimSpace(target.UpdateCommand) != ""
		statuses = append(statuses, st)
	}
	return statuses
}

func (m *AgentUpgradeManager) Run(ctx context.Context, target string) ([]AgentUpgradeRunResult, error) {
	return m.runWithOptions(ctx, target, false)
}

func (m *AgentUpgradeManager) runWithOptions(ctx context.Context, target string, automatic bool) ([]AgentUpgradeRunResult, error) {
	cfg, runner, busyFn := m.snapshotDeps()
	if !cfg.Enabled {
		return nil, fmt.Errorf("agent upgrades are disabled in config")
	}

	names, err := resolveAgentUpgradeRunTargets(target, cfg.Targets)
	if err != nil {
		return nil, err
	}
	results := make([]AgentUpgradeRunResult, 0, len(names))
	binaryPaths := make(map[string]string, len(names))
	defer func() {
		for _, res := range results {
			m.recordResult(res, automatic, binaryPaths[res.Name])
		}
		m.saveState()
		m.notifyResults(results, automatic)
	}()
	for _, name := range names {
		resolved := m.resolveTarget(ctx, name, cfg.Targets[name], runner)
		if resolved.info.Selected != nil {
			binaryPaths[name] = resolved.info.Selected.Path
		}
		if resolved.warning != "" {
			slog.Warn("agent upgrade: duplicate installs", "agent", name, "detail", resolved.warning)
		}
		cfgTarget := resolved.cfg
		res := AgentUpgradeRunResult{Name: name, Strategy: cfgTarget.Strategy}
		if automatic {
			if backoffReason := targetBackoffReason(m.failureState(name)); backoffReason != "" {
				res.Skipped = true
				res.Reason = backoffReason
				results = append(results, res)
				continue
			}
		}
		if !cfgTarget.Enabled || cfgTarget.Strategy == "disabled" {
			res.Skipped = true
			res.Reason = "disabled"
			results = append(results, res)
			continue
		}
		if cfgTarget.Strategy == "observe" {
			res.Skipped = true
			res.Reason = "observe-only"
			results = append(results, res)
			continue
		}
		if strings.TrimSpace(cfgTarget.UpdateCommand) == "" {
			res.Skipped = true
			res.Reason = "no update command configured"
			results = append(results, res)
			continue
		}
		if resolved.blockedReason != "" {
			res.Skipped = true
			res.Reason = resolved.blockedReason
			results = append(results, res)
			continue
		}
		if !m.beginTargetRun(name) {
			res.Skipped = true
			res.Reason = "upgrade already running"
			results = append(results, res)
			continue
		}
		func() {
			defer m.endTargetRun(name)

			// Another echo-client service may be upgrading the same CLI.
			release, locked, lockErr := acquireUpgradeLock(m.lockDirPath(), name)
			if lockErr != nil {
				slog.Warn("agent upgrade: cross-process lock unavailable; continuing", "agent", name, "error", lockErr)
				release, locked = func() {}, true
			}
			if !locked {
				res.Skipped = true
				res.Reason = "another echo-client process is upgrading " + name
				results = append(results, res)
				return
			}
			defer release()

			if busyFn != nil {
				res.BusySessions = busyFn(name)
				if res.BusySessions > 0 {
					res.Skipped = true
					res.Reason = fmt.Sprintf("%d active session(s)", res.BusySessions)
					results = append(results, res)
					return
				}
			}

			timeout := cfg.Timeout
			if timeout <= 0 {
				timeout = defaultAgentUpgradeTimeout
			}
			if strings.TrimSpace(cfgTarget.VersionCommand) != "" {
				beforeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout(timeout))
				out, err := runner(beforeCtx, cfgTarget.VersionCommand)
				cancel()
				if err == nil {
					res.BeforeVersion = normalizeAgentUpgradeOutput(out)
				}
			}
			if policyReason := evaluateTargetVersionPolicy(cfgTarget, res.BeforeVersion); policyReason != "" {
				res.Skipped = true
				res.Reason = policyReason
				m.clearFailureState(name)
				results = append(results, res)
				return
			}

			runCtx, cancel := context.WithTimeout(ctx, timeout)
			out, err := runner(runCtx, cfgTarget.UpdateCommand)
			cancel()
			res.Output = normalizeAgentUpgradeOutput(out)
			if err != nil {
				res.Err = err
				m.recordFailure(name, cfgTarget, err.Error())
				results = append(results, res)
				return
			}

			if strings.TrimSpace(cfgTarget.VersionCommand) != "" {
				afterCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout(timeout))
				out, err := runner(afterCtx, cfgTarget.VersionCommand)
				cancel()
				if err == nil {
					res.AfterVersion = normalizeAgentUpgradeOutput(out)
				}
			}
			res.Changed = res.BeforeVersion != "" && res.AfterVersion != "" && res.BeforeVersion != res.AfterVersion
			m.clearFailureState(name)
			results = append(results, res)
		}()
	}
	return results, nil
}

// resolveTarget works out what to probe and run for one target. It first
// locates the CLI exactly as agent processes do (this process's PATH), so the
// version probe and the update hit the binary that actually runs, then applies
// the per-agent rules below.
func (m *AgentUpgradeManager) resolveTarget(ctx context.Context, name string, target AgentUpgradeTargetConfig, runner AgentUpgradeCommandRunner) resolvedAgentUpgradeTarget {
	info := m.inspectTarget(ctx, name)
	route := detectInstallRoute(name, info.Selected)
	target, routeReason := applyInstallRoute(name, target, info, route)
	resolved := m.resolveTargetRules(ctx, name, target, runner, route)
	resolved.info = info
	resolved.route = route
	resolved.warning = otherInstallsWarning(info)
	resolved.blockedReason = joinBlockedReasons(resolved.blockedReason, routeReason)
	return resolved
}

func (m *AgentUpgradeManager) resolveTargetRules(ctx context.Context, name string, target AgentUpgradeTargetConfig, runner AgentUpgradeCommandRunner, route string) resolvedAgentUpgradeTarget {
	resolved := resolvedAgentUpgradeTarget{cfg: target}
	switch name {
	case "claudecode":
		if route == installRouteNative {
			// Located directly; no need to probe through a shell.
			if !target.UpdateCommandSet {
				resolved.cfg.UpdateCommand = "claude update"
			}
			if normalizeAgentUpgradeChannel(target.Channel) == "latest" {
				resolved.blockedReason = "channel \"latest\" is not supported for native claude installs"
			}
			if target.PinVersion != "" {
				resolved.blockedReason = joinBlockedReasons(resolved.blockedReason, fmt.Sprintf("pin_version %q is not supported for the detected claude install route", target.PinVersion))
			}
			return resolved
		}
		return resolveClaudeCodeUpgradeTarget(ctx, runner, target)
	case "codex":
		if target.Channel != "" {
			resolved.blockedReason = fmt.Sprintf("channel %q is not supported for codex", target.Channel)
			return resolved
		}
		if target.PinVersion != "" && target.Strategy == "package_manager" && !target.UpdateCommandSet {
			resolved.cfg.UpdateCommand = fmt.Sprintf("npm install -g %s", shellQuote("@openai/codex@"+strings.TrimSpace(target.PinVersion)))
		}
		return resolved
	case "gemini":
		if target.Channel != "" {
			resolved.blockedReason = fmt.Sprintf("channel %q is not supported for gemini", target.Channel)
			return resolved
		}
		if target.PinVersion != "" && target.Strategy == "package_manager" {
			resolved.cfg.UpdateCommand = fmt.Sprintf("npm install -g %s", shellQuote("@google/gemini-cli@"+strings.TrimSpace(target.PinVersion)))
		}
		return resolved
	case "qoder", "kimi", "antigravity":
		// Self-updating CLIs: no channels, and they always install the latest.
		if target.Channel != "" {
			resolved.blockedReason = fmt.Sprintf("channel %q is not supported for %s", target.Channel, name)
		}
		if target.PinVersion != "" && !target.UpdateCommandSet {
			resolved.blockedReason = joinBlockedReasons(resolved.blockedReason, "pin_version is not supported for "+name)
		}
		return resolved
	default:
		return resolved
	}
}

func (m *AgentUpgradeManager) snapshotDeps() (AgentUpgradeConfig, AgentUpgradeCommandRunner, func(string) int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg := m.cfg
	cfg.Targets = cloneAgentUpgradeTargets(cfg.Targets)
	runner := m.runCommand
	if runner == nil {
		runner = defaultAgentUpgradeCommandRunner
	}
	return cfg, runner, m.busyCountFn
}

func resolveAgentUpgradeRunTargets(target string, targets map[string]AgentUpgradeTargetConfig) ([]string, error) {
	target = strings.TrimSpace(target)
	if target == "" || strings.EqualFold(target, "all") {
		return sortedAgentUpgradeTargetNames(targets), nil
	}
	name := normalizeAgentUpgradeTarget(target)
	if name == "" {
		return nil, fmt.Errorf("unknown agent target %q", target)
	}
	if _, ok := targets[name]; !ok {
		return nil, fmt.Errorf("unknown agent target %q", target)
	}
	return []string{name}, nil
}

func sortedAgentUpgradeTargetNames(targets map[string]AgentUpgradeTargetConfig) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func cloneAgentUpgradeTargets(in map[string]AgentUpgradeTargetConfig) map[string]AgentUpgradeTargetConfig {
	out := make(map[string]AgentUpgradeTargetConfig, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func resolveClaudeCodeUpgradeTarget(ctx context.Context, runner AgentUpgradeCommandRunner, target AgentUpgradeTargetConfig) resolvedAgentUpgradeTarget {
	resolved := resolvedAgentUpgradeTarget{cfg: target}
	source, updateCommand, blockedReason := detectClaudeCodeUpgradeRoute(ctx, runner, target.Channel)
	switch source {
	case "native", "brew", "npm":
		if !target.UpdateCommandSet {
			resolved.cfg.UpdateCommand = updateCommand
		}
		if source != "native" {
			resolved.cfg.Strategy = "package_manager"
		}
	case "apt", "dnf", "apk":
		if !target.UpdateCommandSet {
			resolved.cfg.UpdateCommand = updateCommand
		}
	}
	resolved.blockedReason = blockedReason
	if target.PinVersion != "" {
		switch source {
		case "npm":
			resolved.cfg.UpdateCommand = fmt.Sprintf("npm install -g %s", shellQuote("@anthropic-ai/claude-code@"+strings.TrimSpace(target.PinVersion)))
		case "native", "brew", "apt", "dnf", "apk":
			resolved.blockedReason = joinBlockedReasons(resolved.blockedReason, fmt.Sprintf("pin_version %q is not supported for the detected claude install route", target.PinVersion))
		}
	}
	return resolved
}

func detectClaudeCodeUpgradeRoute(ctx context.Context, runner AgentUpgradeCommandRunner, channel string) (source string, updateCommand string, blockedReason string) {
	if runner == nil {
		runner = defaultAgentUpgradeCommandRunner
	}
	channel = normalizeAgentUpgradeChannel(channel)
	probe := func(cmd string) (string, bool) {
		probeCtx, cancel := context.WithTimeout(ctx, defaultAgentUpgradeProbeTimeout)
		defer cancel()
		out, err := runner(probeCtx, cmd)
		if err != nil {
			return "", false
		}
		return normalizeAgentUpgradeOutput(out), true
	}

	if path, ok := probe("command -v claude"); ok {
		if strings.Contains(path, "/.local/bin/claude") || strings.Contains(path, "/.local/share/claude/") {
			if channel == "latest" {
				return "native", "claude update", "channel \"latest\" is not supported for native claude installs"
			}
			return "native", "claude update", ""
		}
	}
	hasBrewLatest := false
	if _, ok := probe("brew list --cask claude-code@latest >/dev/null && printf ok"); ok {
		hasBrewLatest = true
	}
	hasBrewStable := false
	if _, ok := probe("brew list --cask claude-code >/dev/null && printf ok"); ok {
		hasBrewStable = true
	}
	switch channel {
	case "stable":
		if hasBrewStable {
			return "brew", "brew upgrade claude-code", ""
		}
	case "latest":
		if hasBrewLatest {
			return "brew", "brew upgrade claude-code@latest", ""
		}
	}
	if hasBrewLatest {
		if channel == "stable" {
			return "brew", "brew upgrade claude-code", "requested claude stable channel but Homebrew stable cask is not installed"
		}
		return "brew", "brew upgrade claude-code@latest", ""
	}
	if hasBrewStable {
		if channel == "latest" {
			return "brew", "brew upgrade claude-code@latest", "requested claude latest channel but Homebrew latest cask is not installed"
		}
		return "brew", "brew upgrade claude-code", ""
	}
	if _, ok := probe("npm -g ls @anthropic-ai/claude-code --depth=0 >/dev/null && printf ok"); ok {
		if channel == "latest" || channel == "stable" {
			return "npm", "npm install -g @anthropic-ai/claude-code", fmt.Sprintf("channel %q is not supported for npm-based claude installs", channel)
		}
		return "npm", "npm install -g @anthropic-ai/claude-code", ""
	}
	if _, ok := probe("dpkg -s claude-code >/dev/null && printf ok"); ok {
		return "apt", "sudo apt update && sudo apt upgrade claude-code", joinBlockedReasons("requires privileged apt upgrade; run manually", unsupportedClaudeChannelReason(channel))
	}
	if _, ok := probe("rpm -q claude-code >/dev/null && printf ok"); ok {
		return "dnf", "sudo dnf upgrade claude-code", joinBlockedReasons("requires privileged dnf upgrade; run manually", unsupportedClaudeChannelReason(channel))
	}
	if _, ok := probe("apk info -e claude-code >/dev/null && printf ok"); ok {
		return "apk", "apk update && apk upgrade claude-code", joinBlockedReasons("requires privileged apk upgrade; run manually", unsupportedClaudeChannelReason(channel))
	}
	return "", "", ""
}

func normalizeAgentUpgradeStrategy(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "builtin", "package_manager", "custom", "observe", "disabled":
		return v
	default:
		return "disabled"
	}
}

func normalizeAgentUpgradeChannel(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "stable", "latest":
		return v
	default:
		return ""
	}
}

func normalizeAgentUpgradePolicy(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "off", "check_only", "idle_only":
		return v
	default:
		return "off"
	}
}

func normalizeAgentUpgradeTarget(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "claude", "claudecode", "claude-code":
		return "claudecode"
	case "codex":
		return "codex"
	case "gemini":
		return "gemini"
	case "qoder":
		return "qoder"
	case "kimi", "kimi-code":
		return "kimi"
	case "antigravity", "agy":
		return "antigravity"
	default:
		return ""
	}
}

func normalizeAgentUpgradeOutput(v string) string {
	return strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
}

func shellQuote(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func evaluateTargetVersionPolicy(target AgentUpgradeTargetConfig, currentVersion string) string {
	if target.MinimumVersion == "" && target.PinVersion == "" {
		return ""
	}
	current := extractAgentVersion(currentVersion)
	if current == "" {
		return "version constraints require a parseable version from version_command"
	}
	if target.PinVersion != "" && sameVersion(current, target.PinVersion) {
		return fmt.Sprintf("already pinned at %s", target.PinVersion)
	}
	if target.MinimumVersion != "" {
		if ok, cmp := compareVersionStrings(current, target.MinimumVersion); ok && cmp >= 0 {
			return fmt.Sprintf("already meets minimum_version %s", target.MinimumVersion)
		}
	}
	return ""
}

func extractAgentVersion(v string) string {
	v = strings.TrimSpace(v)
	start := -1
	for i, r := range v {
		if r >= '0' && r <= '9' {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := start
	for end < len(v) {
		c := v[end]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '.' || c == '-' || c == '+' {
			end++
			continue
		}
		break
	}
	return strings.Trim(v[start:end], ".,;:()[]{}")
}

func compareVersionStrings(a, b string) (bool, int) {
	aParts, aOk := parseVersionParts(extractAgentVersion(a))
	bParts, bOk := parseVersionParts(extractAgentVersion(b))
	if !aOk || !bOk {
		return false, 0
	}
	n := len(aParts)
	if len(bParts) > n {
		n = len(bParts)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(aParts) {
			av = aParts[i]
		}
		if i < len(bParts) {
			bv = bParts[i]
		}
		switch {
		case av < bv:
			return true, -1
		case av > bv:
			return true, 1
		}
	}
	return true, 0
}

func parseVersionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, false
		}
		value := 0
		digits := 0
		for i := 0; i < len(part); i++ {
			c := part[i]
			if c < '0' || c > '9' {
				break
			}
			value = value*10 + int(c-'0')
			digits++
		}
		if digits == 0 {
			return nil, false
		}
		out = append(out, value)
	}
	return out, true
}

func sameVersion(a, b string) bool {
	ok, cmp := compareVersionStrings(a, b)
	return ok && cmp == 0
}

func unsupportedClaudeChannelReason(channel string) string {
	if channel == "" {
		return ""
	}
	return fmt.Sprintf("channel %q is not supported for the detected claude install route", channel)
}

func joinBlockedReasons(parts ...string) string {
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return strings.Join(out, "; ")
}

// defaultAgentUpgradeCommandRunner runs cmd in a plain (non-login) shell with
// this process's environment. A login shell re-reads ~/.profile and
// path_helper and can resolve a different copy of the CLI than the agent
// processes, which is how upgrades used to hit the wrong install.
func defaultAgentUpgradeCommandRunner(ctx context.Context, cmd string) (string, error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	out, err := c.CombinedOutput()
	return string(out), err
}

func (m *AgentUpgradeManager) beginTargetRun(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running[name] {
		return false
	}
	m.running[name] = true
	return true
}

func (m *AgentUpgradeManager) endTargetRun(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, name)
}

func (m *AgentUpgradeManager) updateScheduleState(last, next time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !last.IsZero() {
		m.lastAutoRun = last
	}
	m.nextAutoRun = next
}

func (m *AgentUpgradeManager) failureState(name string) agentUpgradeFailureState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failures[name]
}

func (m *AgentUpgradeManager) recordFailure(name string, target AgentUpgradeTargetConfig, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.failures[name]
	state.ConsecutiveFailures++
	state.LastFailureReason = strings.TrimSpace(reason)
	state.LastFailureAt = time.Now()
	if target.MaxFailures > 0 && target.FailureBackoff > 0 && state.ConsecutiveFailures >= target.MaxFailures {
		state.BackoffUntil = state.LastFailureAt.Add(target.FailureBackoff)
	}
	m.failures[name] = state
}

func (m *AgentUpgradeManager) clearFailureState(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.failures, name)
}

func targetBackoffReason(state agentUpgradeFailureState) string {
	if state.BackoffUntil.IsZero() || time.Now().After(state.BackoffUntil) {
		return ""
	}
	if state.LastFailureReason != "" {
		return fmt.Sprintf("backing off until %s after failures: %s", state.BackoffUntil.Format(time.RFC3339), state.LastFailureReason)
	}
	return fmt.Sprintf("backing off until %s", state.BackoffUntil.Format(time.RFC3339))
}

func versionProbeTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 || timeout > defaultAgentUpgradeProbeTimeout {
		return defaultAgentUpgradeProbeTimeout
	}
	return timeout
}

func dedupeTrimmedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
