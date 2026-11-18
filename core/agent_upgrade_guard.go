package core

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// Path is the PATH entry agents run; Real is the file it resolved to,
	// with the size and modification time it had.
	Path      string        `json:"path"`
	Real      string        `json:"real"`
	RealSize  int64         `json:"real_size,omitempty"`
	RealMtime time.Time     `json:"real_mtime,omitempty"`
	Links     []upgradeLink `json:"links,omitempty"`
	// Backup is a copy of Real (a copy-on-write clone where the filesystem
	// can make one), for CLIs that replace their binary in place (Kimi,
	// agy) or delete it. Empty when no backup could be made.
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
	return walkSymlinksAllowMissing(path, false)
}

// walkSymlinksAllowMissing is walkSymlinks that, with allowMissing, also
// resolves a path whose last component does not exist (yet).
func walkSymlinksAllowMissing(path string, allowMissing bool) (string, []upgradeLink, error) {
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
			if allowMissing && os.IsNotExist(err) && i == len(parts)-1 {
				return next, links, nil
			}
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

// takeUpgradeSnapshot records the install at path and, if backupDir is set,
// copies the binary there. A copy, not a hard link: an updater that rewrites
// the existing file in place would change a hard link too.
func takeUpgradeSnapshot(name, route, path, version string, healthy bool, backupDir string) (*upgradeSnapshot, error) {
	real, links, err := walkSymlinks(path)
	if err != nil {
		return nil, err
	}
	snap := &upgradeSnapshot{
		Target: name, Route: route, Path: path, Real: real, Links: links,
		Version: version, Healthy: healthy, StartedAt: time.Now(),
	}
	if st, err := os.Stat(real); err == nil {
		snap.RealSize, snap.RealMtime = st.Size(), st.ModTime()
	}
	if backupDir == "" || route == installRouteNPM || route == installRouteBrew {
		return snap, nil
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		slog.Warn("agent upgrade: no backup of the CLI binary", "agent", name, "error", err)
		return snap, nil
	}
	backup := filepath.Join(backupDir, fmt.Sprintf("%s-%d", name, snap.StartedAt.UnixNano()))
	if err := copyExecutable(real, backup); err != nil {
		slog.Warn("agent upgrade: no backup of the CLI binary; rollback can only restore symlinks", "agent", name, "error", err)
		_ = os.Remove(backup)
		return snap, nil
	}
	snap.Backup = backup
	return snap, nil
}

// copyExecutable copies src to dst (which must not exist) with src's mode,
// as a copy-on-write clone where the platform can make one.
func copyExecutable(src, dst string) error {
	if cloneFile(src, dst) == nil {
		return nil
	}
	_ = os.Remove(dst)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// removeBackup deletes the snapshot's backup, only if it lies in dir (the
// manager's backup directory: the record is read back from the state file).
func (s *upgradeSnapshot) removeBackup(dir string) {
	if s == nil || s.Backup == "" || !pathWithin(dir, s.Backup) {
		return
	}
	if err := os.Remove(s.Backup); err != nil && !os.IsNotExist(err) {
		slog.Warn("agent upgrade: failed to remove binary backup", "path", s.Backup, "error", err)
	}
}

// pathWithin reports whether path is inside dir.
func pathWithin(dir, path string) bool {
	if dir == "" || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rollback puts the install back the way the snapshot found it. It only
// repoints symlinks (and recreates missing ones) and restores the recorded
// binary; it never replaces a regular file or directory with a link.
func (s *upgradeSnapshot) rollback(ctx context.Context, runner AgentUpgradeCommandRunner, backupDir string) error {
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
	// Repoint symlinks the upgrade changed or removed (Claude's versions,
	// Codex's standalone/current, Qoder's versioned binary). Innermost first,
	// stopping as soon as the CLI resolves to its old binary again, so a link
	// higher up (a symlinked home or packages directory) is only touched when
	// nothing else brings the old binary back.
	for i := len(s.Links) - 1; i >= 0; i-- {
		if real, _, err := walkSymlinksAllowMissing(s.Path, true); err == nil && real == s.Real {
			break
		}
		link := s.Links[i]
		fi, err := os.Lstat(link.Path)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink == 0:
			return fmt.Errorf("restore symlink %s: it is no longer a symlink", link.Path)
		case err != nil && !os.IsNotExist(err):
			return fmt.Errorf("restore symlink %s: %w", link.Path, err)
		case err == nil:
			if cur, rerr := os.Readlink(link.Path); rerr == nil && sameLinkTarget(link.Path, cur, link.Target) {
				continue
			}
		}
		if err := replaceWithSymlink(link.Path, link.Target); err != nil {
			return fmt.Errorf("restore symlink %s: %w", link.Path, err)
		}
	}
	real, _, err := walkSymlinksAllowMissing(s.Path, true)
	if err != nil {
		return fmt.Errorf("resolve %s after restoring symlinks: %w", s.Path, err)
	}
	if real != s.Real {
		return fmt.Errorf("%s resolves to %s, not the previous %s", s.Path, real, s.Real)
	}
	// Put back a binary that was replaced in place or deleted (Kimi, agy).
	if s.Backup == "" || !pathWithin(backupDir, s.Backup) {
		if _, err := os.Stat(real); err != nil {
			return fmt.Errorf("%s is gone and there is no backup to restore", real)
		}
		return nil
	}
	if st, err := os.Stat(real); err == nil && st.Size() == s.RealSize && st.ModTime().Equal(s.RealMtime) {
		return nil // unchanged
	}
	if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
		return fmt.Errorf("restore %s: %w", real, err)
	}
	tmp := real + ".echo-client-rollback"
	_ = os.Remove(tmp)
	if err := copyExecutable(s.Backup, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("restore %s: %w", real, err)
	}
	if err := os.Rename(tmp, real); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("restore %s: %w", real, err)
	}
	return nil
}

// sameLinkTarget reports whether two targets of the link at path point to
// the same place (one may be relative, the other absolute).
func sameLinkTarget(path, a, b string) bool {
	if a == b {
		return true
	}
	abs := func(t string) string {
		if filepath.IsAbs(t) {
			return filepath.Clean(t)
		}
		return filepath.Join(filepath.Dir(path), t)
	}
	pa, pb := abs(a), abs(b)
	if pa == pb {
		return true
	}
	// Same file through different spellings (e.g. /var and /private/var).
	ra, erra := filepath.EvalSymlinks(pa)
	rb, errb := filepath.EvalSymlinks(pb)
	return erra == nil && errb == nil && ra == rb
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
// broken. Runs once when the manager starts. A record whose rollback fails is
// kept, with its backup, for the next start.
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
	backupDir := m.backupDir()
	var results []AgentUpgradeRunResult
	for _, snap := range pending {
		snap := snap
		name := snap.Target
		if _, known := cfg.Targets[name]; !known {
			slog.Warn("agent upgrade: dropping an interrupted-upgrade record for an unknown target", "target", name)
			m.clearInFlight(name)
			continue
		}
		if !m.beginTargetRun(name) {
			continue
		}
		res, done := func() (AgentUpgradeRunResult, bool) {
			defer m.endTargetRun(name)
			res := AgentUpgradeRunResult{Name: name, BeforeVersion: snap.Version}
			release, locked, err := acquireUpgradeLock(m.lockDirPath(), name)
			if err != nil || !locked {
				// Another service is upgrading it now; leave the record for
				// the next start.
				res.Skipped, res.Reason = true, "interrupted upgrade not checked: upgrade lock unavailable"
				return res, false
			}
			defer release()
			resolved := m.resolveTarget(ctx, name, cfg.Targets[name], runner)
			if resolved.info.Selected == nil || resolved.info.Selected.Path != snap.Path {
				// The record does not describe the CLI agents run now (it was
				// reinstalled elsewhere, or the record is not ours): leave
				// the filesystem alone.
				slog.Warn("agent upgrade: interrupted-upgrade record does not match the installed CLI; not touching it", "agent", name, "recorded", snap.Path)
				res.Skipped, res.Reason = true, "interrupted upgrade record does not match the installed CLI"
				return res, true
			}
			timeout := versionProbeTimeout(cfg.Timeout)
			_, version, problems := m.cliProblems(ctx, name, resolved.cfg, runner, timeout)
			if ctx.Err() != nil {
				return res, false
			}
			res.AfterVersion = version
			switch {
			case len(problems) == 0:
				res.Reason = "an interrupted upgrade left the CLI working"
				res.Changed = version != "" && snap.Version != "" && version != snap.Version
				slog.Info("agent upgrade: interrupted upgrade left the CLI working", "agent", name, "version", version)
			case !snap.Healthy:
				res.Err = fmt.Errorf("an interrupted upgrade left %s failing its smoke check, as it was before: %s", name, strings.Join(problems, "; "))
			default:
				var rollbackFailed bool
				rollbackFailed, res.Err = m.rollbackAfterFailedCheck(ctx, &snap, &res, problems, runner, resolved.cfg, timeout, "an interrupted upgrade")
				if rollbackFailed {
					return res, false
				}
			}
			return res, true
		}()
		if done {
			snap.removeBackup(backupDir)
			m.clearInFlight(name)
		}
		if ctx.Err() == nil {
			results = append(results, res)
		}
	}
	for _, res := range results {
		m.recordResult(res, true, "")
	}
	m.saveState()
	m.notifyResults(results, true)
}

// rollbackAfterFailedCheck rolls back an install the upgrade broke. It
// returns whether the rollback itself failed (the caller then keeps the record
// and backup for another try) and the error to report.
func (m *AgentUpgradeManager) rollbackAfterFailedCheck(ctx context.Context, snap *upgradeSnapshot, res *AgentUpgradeRunResult, problems []string, runner AgentUpgradeCommandRunner, target AgentUpgradeTargetConfig, timeout time.Duration, what string) (bool, error) {
	name := snap.Target
	broken := strings.Join(problems, "; ")
	slog.Warn("agent upgrade: CLI failed its smoke check after the upgrade; rolling back", "agent", name, "problems", broken, "to", snap.Version)
	rctx, cancel := context.WithTimeout(ctx, defaultAgentUpgradeTimeout)
	err := snap.rollback(rctx, runner, m.backupDir())
	cancel()
	if err != nil {
		slog.Error("agent upgrade: rollback failed", "agent", name, "error", err)
		return true, fmt.Errorf("%s left %s failing its smoke check (%s), and the rollback failed: %w", what, name, broken, err)
	}
	_, version, after := m.cliProblems(ctx, name, target, runner, timeout)
	if len(after) > 0 {
		return false, fmt.Errorf("%s left %s failing its smoke check (%s); after the rollback it still fails: %s", what, name, broken, strings.Join(after, "; "))
	}
	res.RolledBack = true
	res.AfterVersion = version
	res.Changed = false
	slog.Info("agent upgrade: rolled back", "agent", name, "version", version)
	return false, fmt.Errorf("%s failed the smoke check (%s); rolled back to %s", what, broken, firstNonEmpty(version, snap.Version))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
