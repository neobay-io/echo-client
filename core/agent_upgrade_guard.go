package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// npmPackages are the npm packages of agent CLIs, for rolling back an npm
// install and for looking up the latest release.
var npmPackages = map[string]string{
	"claudecode": "@anthropic-ai/claude-code",
	"codex":      "@openai/codex",
	"gemini":     "@google/gemini-cli",
	"kimi":       "@moonshot-ai/kimi-code",
}

// upgradeLink is a symlink met while resolving an agent CLI, with the target
// stored in the link.
type upgradeLink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

// upgradeSnapshot records an agent CLI install just before an upgrade, so an
// upgrade that leaves the CLI broken can be rolled back. It is kept in the
// state file until the upgrade finishes, so an upgrade interrupted by a crash
// or restart is checked and repaired on the next start.
type upgradeSnapshot struct {
	Target string `json:"target"`
	Route  string `json:"route,omitempty"`
	// Path is the PATH entry agents run; Real is the file it resolved to.
	Path  string        `json:"path"`
	Real  string        `json:"real"`
	Links []upgradeLink `json:"links,omitempty"`
	// Backup is a hard link to Real, for CLIs that replace their binary in
	// place (Kimi, agy). Empty when no backup could be made.
	Backup  string `json:"backup,omitempty"`
	Version string `json:"version,omitempty"`
	// Healthy: the install passed the smoke checks before the upgrade. Only
	// then is a failing install rolled back.
	Healthy   bool      `json:"healthy"`
	StartedAt time.Time `json:"started_at"`
}

// walkSymlinks resolves path like filepath.EvalSymlinks and also returns
// every symlink on the way (including directories such as Codex's
// standalone/current) with its stored target.
func walkSymlinks(path string) (string, []upgradeLink, error) {
	if !filepath.IsAbs(path) {
		return "", nil, fmt.Errorf("%s is not an absolute path", path)
	}
	var links []upgradeLink
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	cur := string(filepath.Separator)
	for i, hops := 1, 0; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		next := filepath.Join(cur, parts[i])
		fi, err := os.Lstat(next)
		if err != nil {
			return "", links, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > 64 {
			return "", links, fmt.Errorf("too many symlinks resolving %s", path)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", links, err
		}
		links = append(links, upgradeLink{Path: next, Target: target})
		resolved := target
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(cur, resolved)
		}
		// Start over from the link's target with the remaining components.
		parts = append(strings.Split(filepath.Clean(resolved), string(filepath.Separator)), parts[i+1:]...)
		cur, i = string(filepath.Separator), 0
	}
	return cur, links, nil
}

// takeUpgradeSnapshot records the install at path. backupDir, if set, gets a
// hard link to the binary; it must be on the binary's volume, otherwise no
// backup is kept (copying hundreds of MB per upgrade is not worth it: the
// CLIs that live elsewhere keep their old versions themselves).
func takeUpgradeSnapshot(name, route, path, version string, healthy bool, backupDir string) (*upgradeSnapshot, error) {
	real, links, err := walkSymlinks(path)
	if err != nil {
		return nil, err
	}
	snap := &upgradeSnapshot{
		Target: name, Route: route, Path: path, Real: real, Links: links,
		Version: version, Healthy: healthy, StartedAt: time.Now(),
	}
	if backupDir == "" || route == installRouteNPM || route == installRouteBrew {
		return snap, nil
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		slog.Warn("agent upgrade: no backup of the CLI binary", "agent", name, "error", err)
		return snap, nil
	}
	backup := filepath.Join(backupDir, fmt.Sprintf("%s-%d", name, snap.StartedAt.UnixNano()))
	if err := os.Link(real, backup); err != nil {
		slog.Info("agent upgrade: no backup of the CLI binary; rollback can only restore symlinks", "agent", name, "error", err)
		return snap, nil
	}
	snap.Backup = backup
	return snap, nil
}

// removeBackup deletes the snapshot's backup link.
func (s *upgradeSnapshot) removeBackup() {
	if s == nil || s.Backup == "" {
		return
	}
	if err := os.Remove(s.Backup); err != nil && !os.IsNotExist(err) {
		slog.Warn("agent upgrade: failed to remove binary backup", "path", s.Backup, "error", err)
	}
}

// rollback puts the install back the way the snapshot found it.
func (s *upgradeSnapshot) rollback(ctx context.Context, runner AgentUpgradeCommandRunner) error {
	switch s.Route {
	case installRouteBrew:
		return errors.New("rollback is not supported for Homebrew installs")
	case installRouteNPM:
		pkg := npmPackages[s.Target]
		version := extractAgentVersion(s.Version)
		if pkg == "" || version == "" {
			return fmt.Errorf("no npm package or version to roll back %s to", s.Target)
		}
		out, err := runner(ctx, "npm install -g "+shellQuote(pkg+"@"+version))
		if err != nil {
			return fmt.Errorf("npm install %s@%s: %w: %s", pkg, version, err, outputTail(out, 200))
		}
		return nil
	}
	// Repoint symlinks the upgrade changed (Claude's versions, Codex's
	// standalone/current, Qoder's versioned binary).
	for _, link := range s.Links {
		if cur, err := os.Readlink(link.Path); err == nil && cur == link.Target {
			continue
		}
		if err := replaceWithSymlink(link.Path, link.Target); err != nil {
			return fmt.Errorf("restore symlink %s: %w", link.Path, err)
		}
	}
	real, _, err := walkSymlinks(s.Path)
	if err != nil {
		return fmt.Errorf("resolve %s after restoring symlinks: %w", s.Path, err)
	}
	if real != s.Real {
		return fmt.Errorf("%s resolves to %s, not the previous %s", s.Path, real, s.Real)
	}
	// Put back a binary that was replaced in place (Kimi, agy).
	if s.Backup != "" && !sameFile(s.Backup, real) {
		tmp := real + ".echo-client-rollback"
		_ = os.Remove(tmp)
		if err := os.Link(s.Backup, tmp); err != nil {
			return fmt.Errorf("restore %s: %w", real, err)
		}
		if err := os.Rename(tmp, real); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("restore %s: %w", real, err)
		}
	}
	return nil
}

func replaceWithSymlink(path, target string) error {
	tmp := path + ".echo-client-rollback"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func sameFile(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// cliProblems runs the smoke checks on the CLI agents run: it still resolves,
// reports a version, and its help still lists the options the agent adapter
// passes. It returns the binary path and what is wrong; no problems means
// healthy.
func (m *AgentUpgradeManager) cliProblems(ctx context.Context, name string, target AgentUpgradeTargetConfig, runner AgentUpgradeCommandRunner, timeout time.Duration) (string, string, []string) {
	info := m.inspectTarget(ctx, name)
	if info.Selected == nil || info.Selected.Broken {
		return "", "", []string{fmt.Sprintf("%s is not found", AgentCLIBinary(name))}
	}
	path := info.Selected.Path
	var problems []string
	versionCmd := strings.TrimSpace(target.VersionCommand)
	if versionCmd == "" || !target.VersionCommandSet {
		versionCmd = shellQuote(path) + " --version"
	}
	vctx, cancel := context.WithTimeout(ctx, timeout)
	out, err := runner(vctx, versionCmd)
	cancel()
	version := normalizeAgentUpgradeOutput(out)
	if err != nil {
		problems = append(problems, fmt.Sprintf("`--version` failed: %s", firstLine(strings.TrimSpace(err.Error()+" "+out), 160)))
		version = ""
	}
	return path, version, append(problems, m.cliFlagProblems(ctx, name, path, target, runner, timeout)...)
}

// cliFlagProblems runs the registered help checks against the binary at path.
func (m *AgentUpgradeManager) cliFlagProblems(ctx context.Context, name, path string, target AgentUpgradeTargetConfig, runner AgentUpgradeCommandRunner, timeout time.Duration) []string {
	if target.SmokeCheckOff {
		return nil
	}
	return agentCLIFlagProblems(ctx, name, path, runner, timeout)
}

// CheckAgentCLIOptions checks that the agent CLI at path still accepts the
// options its adapter passes (see RegisterAgentCLIChecks). It returns what
// is wrong; nil means every option is listed.
func CheckAgentCLIOptions(ctx context.Context, agentType, path string) []string {
	return agentCLIFlagProblems(ctx, agentType, path, defaultAgentUpgradeCommandRunner, defaultAgentUpgradeProbeTimeout)
}

func agentCLIFlagProblems(ctx context.Context, name, path string, runner AgentUpgradeCommandRunner, timeout time.Duration) []string {
	var problems []string
	bin := filepath.Base(path)
	for _, check := range agentCLIChecksFor(name) {
		quoted := make([]string, 0, len(check.Args)+1)
		quoted = append(quoted, shellQuote(path))
		for _, arg := range check.Args {
			quoted = append(quoted, shellQuote(arg))
		}
		label := strings.TrimSpace(bin + " " + strings.Join(check.Args, " "))
		cctx, cancel := context.WithTimeout(ctx, timeout)
		out, err := runner(cctx, strings.Join(quoted, " "))
		cancel()
		if err != nil {
			problems = append(problems, fmt.Sprintf("`%s` failed: %s", label, firstLine(strings.TrimSpace(err.Error()), 160)))
			continue
		}
		if missing := missingCLIFlags(out, check.Flags); len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("`%s` no longer lists %s", label, strings.Join(missing, ", ")))
		}
	}
	return problems
}

// backupDir is where binary backups go: next to the state file.
func (m *AgentUpgradeManager) backupDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(m.statePath), "agent-upgrade-backups")
}

func (m *AgentUpgradeManager) setInFlight(snap *upgradeSnapshot) {
	m.mu.Lock()
	m.inFlight[snap.Target] = *snap
	m.mu.Unlock()
	m.saveState()
}

func (m *AgentUpgradeManager) clearInFlight(name string) {
	m.mu.Lock()
	_, ok := m.inFlight[name]
	delete(m.inFlight, name)
	m.mu.Unlock()
	if ok {
		m.saveState()
	}
}

// recoverInterruptedUpgrades checks the CLIs whose upgrade did not finish
// (the service stopped or crashed during it) and rolls back the ones it left
// broken. Runs once when the manager starts.
func (m *AgentUpgradeManager) recoverInterruptedUpgrades(ctx context.Context) {
	m.mu.Lock()
	pending := make([]upgradeSnapshot, 0, len(m.inFlight))
	for _, snap := range m.inFlight {
		pending = append(pending, snap)
	}
	m.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	cfg, runner, _ := m.snapshotDeps()
	var results []AgentUpgradeRunResult
	for _, snap := range pending {
		snap := snap
		name := snap.Target
		if !m.beginTargetRun(name) {
			continue
		}
		res := func() AgentUpgradeRunResult {
			defer m.endTargetRun(name)
			res := AgentUpgradeRunResult{Name: name, BeforeVersion: snap.Version}
			release, locked, err := acquireUpgradeLock(m.lockDirPath(), name)
			if err != nil || !locked {
				// Another service is upgrading it now; leave the record for
				// the next start.
				res.Skipped, res.Reason = true, "interrupted upgrade not checked: upgrade lock unavailable"
				return res
			}
			defer release()
			resolved := m.resolveTarget(ctx, name, cfg.Targets[name], runner)
			timeout := versionProbeTimeout(cfg.Timeout)
			_, version, problems := m.cliProblems(ctx, name, resolved.cfg, runner, timeout)
			res.AfterVersion = version
			switch {
			case len(problems) == 0:
				res.Reason = "an interrupted upgrade left the CLI working"
				res.Changed = version != "" && snap.Version != "" && version != snap.Version
				slog.Info("agent upgrade: interrupted upgrade left the CLI working", "agent", name, "version", version)
			case !snap.Healthy:
				res.Err = fmt.Errorf("an interrupted upgrade left %s failing its smoke check, as it was before: %s", name, strings.Join(problems, "; "))
			default:
				res.Err = m.rollbackAfterFailedCheck(ctx, &snap, &res, problems, runner, resolved.cfg, timeout, "an interrupted upgrade")
			}
			snap.removeBackup()
			m.clearInFlight(name)
			return res
		}()
		results = append(results, res)
	}
	for _, res := range results {
		m.recordResult(res, true, "")
	}
	m.saveState()
	m.notifyResults(results, true)
}

// rollbackAfterFailedCheck rolls back an install the upgrade broke and
// returns the error to report.
func (m *AgentUpgradeManager) rollbackAfterFailedCheck(ctx context.Context, snap *upgradeSnapshot, res *AgentUpgradeRunResult, problems []string, runner AgentUpgradeCommandRunner, target AgentUpgradeTargetConfig, timeout time.Duration, what string) error {
	name := snap.Target
	broken := strings.Join(problems, "; ")
	slog.Warn("agent upgrade: CLI failed its smoke check after the upgrade; rolling back", "agent", name, "problems", broken, "to", snap.Version)
	rctx, cancel := context.WithTimeout(ctx, defaultAgentUpgradeTimeout)
	err := snap.rollback(rctx, runner)
	cancel()
	if err != nil {
		slog.Error("agent upgrade: rollback failed", "agent", name, "error", err)
		return fmt.Errorf("%s left %s failing its smoke check (%s), and the rollback failed: %w", what, name, broken, err)
	}
	_, version, after := m.cliProblems(ctx, name, target, runner, timeout)
	if len(after) > 0 {
		return fmt.Errorf("%s left %s failing its smoke check (%s); after the rollback it still fails: %s", what, name, broken, strings.Join(after, "; "))
	}
	res.RolledBack = true
	res.AfterVersion = version
	res.Changed = false
	slog.Info("agent upgrade: rolled back", "agent", name, "version", version)
	return fmt.Errorf("%s failed the smoke check (%s); rolled back to %s", what, broken, firstNonEmpty(version, snap.Version))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
