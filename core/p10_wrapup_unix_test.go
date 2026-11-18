//go:build !windows

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeCLIScript is a CLI that reports version and lists the given options.
func fakeCLIScript(version string, flags ...string) string {
	return fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n--version) echo 'tool %s';;\n--help) echo 'Options:'; for f in %s; do echo \"  $f <value>\"; done;;\nesac\n",
		version, strings.Join(flags, " "))
}

const brokenCLIScript = "#!/bin/sh\necho 'dyld: missing symbol' >&2\nexit 1\n"

// guardFixture is a self-updating "kimi" install in a temp dir, upgraded by
// update (a shell command) with the real command runner.
type guardFixture struct {
	dir, bin, state string
	mgr             *AgentUpgradeManager
}

func newGuardFixture(t *testing.T, update string) *guardFixture {
	t.Helper()
	prev := agentCLIChecksFor("kimi")
	RegisterAgentCLIChecks("kimi", AgentCLICheck{Args: []string{"--help"}, Flags: []string{"--prompt", "--model"}})
	t.Cleanup(func() { RegisterAgentCLIChecks("kimi", prev...) })

	dir := t.TempDir()
	f := &guardFixture{dir: dir, bin: filepath.Join(dir, "bin", "kimi"), state: filepath.Join(dir, "state", "agent-upgrade-state.json")}
	cfg := singleTargetUpgradeConfig("kimi", "custom", "", strings.ReplaceAll(update, "$DIR", dir))
	target := cfg.Targets["kimi"]
	target.UpdateCommandSet = true
	target.LatestCommand = "true" // no network: latest release unknown
	cfg.Targets["kimi"] = target
	f.mgr = NewAgentUpgradeManager(cfg)
	f.mgr.SetStatePath(f.state)
	f.mgr.SetCLIInspector(func(context.Context, string) AgentCLIInfo {
		resolved, _ := filepath.EvalSymlinks(f.bin)
		return AgentCLIInfo{Agent: "kimi", Binary: "kimi", Selected: &AgentCLIInstall{Path: f.bin, Resolved: resolved}}
	})
	return f
}

func (f *guardFixture) run(t *testing.T) AgentUpgradeRunResult {
	t.Helper()
	results, err := f.mgr.Run(context.Background(), "kimi")
	if err != nil || len(results) != 1 {
		t.Fatalf("Run = %#v, %v", results, err)
	}
	return results[0]
}

func (f *guardFixture) assertCleanedUp(t *testing.T) {
	t.Helper()
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(f.state), "agent-upgrade-backups")); len(entries) != 0 {
		t.Fatalf("backups left behind: %d", len(entries))
	}
	data, _ := os.ReadFile(f.state)
	var st agentUpgradeState
	_ = json.Unmarshal(data, &st)
	if len(st.InFlight) != 0 {
		t.Fatalf("in-flight upgrade left in state: %s", data)
	}
}

// T10.5: an upgrade that switches a versions symlink to a build missing an
// option the adapter passes is rolled back to the previous version.
func TestUpgradeRollsBackSymlinkSwitch(t *testing.T) {
	f := newGuardFixture(t, "ln -sfn $DIR/versions/2.0 $DIR/bin/kimi")
	writeExecutable(t, filepath.Join(f.dir, "versions", "1.0"), fakeCLIScript("1.0", "--prompt", "--model"))
	writeExecutable(t, filepath.Join(f.dir, "versions", "2.0"), fakeCLIScript("2.0", "--prompt"))
	_ = os.MkdirAll(filepath.Dir(f.bin), 0o755)
	if err := os.Symlink(filepath.Join(f.dir, "versions", "1.0"), f.bin); err != nil {
		t.Fatal(err)
	}

	res := f.run(t)
	if !res.RolledBack || res.Err == nil || !strings.Contains(res.Err.Error(), "no longer lists --model") ||
		!strings.Contains(res.Err.Error(), "rolled back to tool 1.0") || res.AfterVersion != "tool 1.0" || res.Changed {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
	if target, _ := os.Readlink(f.bin); target != filepath.Join(f.dir, "versions", "1.0") {
		t.Fatalf("link = %s", target)
	}
	if st := f.mgr.failureState("kimi"); st.ConsecutiveFailures != 1 {
		t.Fatalf("failure not recorded: %#v", st)
	}
	if last, _ := f.mgr.LastResult("kimi"); !last.RolledBack {
		t.Fatalf("last result = %#v", last)
	}
	f.assertCleanedUp(t)
}

// T10.2/T10.5: a binary replaced in place (Kimi, agy) by one that no longer
// starts is restored from the backup, also when the installer reports an error.
func TestUpgradeRollsBackInPlaceReplacement(t *testing.T) {
	f := newGuardFixture(t, "printf '#!/bin/sh\\nexit 1\\n' > $DIR/bin/kimi.new && chmod +x $DIR/bin/kimi.new && mv $DIR/bin/kimi.new $DIR/bin/kimi && exit 3")
	old := fakeCLIScript("1.0", "--prompt", "--model")
	writeExecutable(t, f.bin, old)

	res := f.run(t)
	if !res.RolledBack || res.Err == nil || !strings.Contains(res.Err.Error(), "installer error") || !strings.Contains(res.Err.Error(), "`--version` failed") {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
	if data, _ := os.ReadFile(f.bin); string(data) != old {
		t.Fatalf("binary not restored: %q", data)
	}
	f.assertCleanedUp(t)
}

// A healthy upgrade is kept.
func TestUpgradeKeepsHealthyNewVersion(t *testing.T) {
	f := newGuardFixture(t, "ln -sfn $DIR/versions/2.0 $DIR/bin/kimi")
	writeExecutable(t, filepath.Join(f.dir, "versions", "1.0"), fakeCLIScript("1.0", "--prompt", "--model"))
	writeExecutable(t, filepath.Join(f.dir, "versions", "2.0"), fakeCLIScript("2.0", "--prompt", "--model", "--extra"))
	_ = os.MkdirAll(filepath.Dir(f.bin), 0o755)
	_ = os.Symlink(filepath.Join(f.dir, "versions", "1.0"), f.bin)

	res := f.run(t)
	if res.Err != nil || res.RolledBack || !res.Changed || res.BeforeVersion != "tool 1.0" || res.AfterVersion != "tool 2.0" {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
	if target, _ := os.Readlink(f.bin); target != filepath.Join(f.dir, "versions", "2.0") {
		t.Fatalf("link = %s", target)
	}
	f.assertCleanedUp(t)
}

// A CLI that already failed the check before upgrading is not rolled back:
// the upgrade did not break it.
func TestUpgradeDoesNotRollBackAlreadyFailingCLI(t *testing.T) {
	f := newGuardFixture(t, "ln -sfn $DIR/versions/2.0 $DIR/bin/kimi")
	writeExecutable(t, filepath.Join(f.dir, "versions", "1.0"), fakeCLIScript("1.0", "--prompt"))
	writeExecutable(t, filepath.Join(f.dir, "versions", "2.0"), fakeCLIScript("2.0", "--prompt"))
	_ = os.MkdirAll(filepath.Dir(f.bin), 0o755)
	_ = os.Symlink(filepath.Join(f.dir, "versions", "1.0"), f.bin)

	res := f.run(t)
	if res.RolledBack || res.Err != nil || !res.Changed {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
}

// T10.2: an upgrade cut short by a crash or restart is checked on the next
// start, and rolled back if it left the CLI broken.
func TestInterruptedUpgradeIsRepairedOnStart(t *testing.T) {
	f := newGuardFixture(t, "unused")
	writeExecutable(t, filepath.Join(f.dir, "versions", "1.0"), fakeCLIScript("1.0", "--prompt", "--model"))
	writeExecutable(t, filepath.Join(f.dir, "versions", "2.0"), brokenCLIScript)
	_ = os.MkdirAll(filepath.Dir(f.bin), 0o755)
	_ = os.Symlink(filepath.Join(f.dir, "versions", "1.0"), f.bin)

	// The previous run recorded the install, then died mid-upgrade.
	snap, err := takeUpgradeSnapshot("kimi", installRouteUnknown, f.bin, "tool 1.0", true, f.mgr.backupDir())
	if err != nil {
		t.Fatal(err)
	}
	f.mgr.setInFlight(snap)
	if err := replaceWithSymlink(f.bin, filepath.Join(f.dir, "versions", "2.0")); err != nil {
		t.Fatal(err)
	}

	restarted := NewAgentUpgradeManager(f.mgr.Snapshot())
	restarted.SetStatePath(f.state)
	restarted.SetCLIInspector(func(context.Context, string) AgentCLIInfo {
		return AgentCLIInfo{Agent: "kimi", Binary: "kimi", Selected: &AgentCLIInstall{Path: f.bin}}
	})
	var notes []string
	restarted.SetNotifier(func(_, _, text string) error { notes = append(notes, text); return nil })
	restarted.SetNotifyChat(AgentUpgradeNotifyChat{Project: "p", SessionKey: "k"}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	restarted.recoverInterruptedUpgrades(ctx)

	if target, _ := os.Readlink(f.bin); target != filepath.Join(f.dir, "versions", "1.0") {
		t.Fatalf("link = %s", target)
	}
	last, _ := restarted.LastResult("kimi")
	if !last.RolledBack || !strings.Contains(last.Error, "an interrupted upgrade failed the smoke check") {
		t.Fatalf("last result = %#v", last)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "kimi upgrade failed") {
		t.Fatalf("notes = %q", notes)
	}
	f.assertCleanedUp(t)
}

func TestWalkSymlinksRecordsDirectoryLinks(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	writeExecutable(t, filepath.Join(dir, "releases", "1", "bin", "codex"), "x")
	_ = os.Symlink(filepath.Join(dir, "releases", "1"), filepath.Join(dir, "current"))
	_ = os.MkdirAll(filepath.Join(dir, "local"), 0o755)
	_ = os.Symlink("../current/bin/codex", filepath.Join(dir, "local", "codex"))

	real, links, err := walkSymlinks(filepath.Join(dir, "local", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if real != filepath.Join(dir, "releases", "1", "bin", "codex") || len(links) != 2 ||
		links[0].Path != filepath.Join(dir, "local", "codex") || links[1].Path != filepath.Join(dir, "current") {
		t.Fatalf("real = %s, links = %#v", real, links)
	}
}

// An upgrade cut short by shutdown is not checked or rolled back then (the
// checks would fail only because of the shutdown); the record stays for the
// next start.
func TestUpgradeInterruptedByShutdownKeepsRecord(t *testing.T) {
	f := newGuardFixture(t, "sleep 5")
	writeExecutable(t, f.bin, fakeCLIScript("1.0", "--prompt", "--model"))
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	results, err := f.mgr.Run(ctx, "kimi")
	if err != nil || len(results) != 1 || results[0].RolledBack || results[0].Err == nil ||
		!strings.Contains(results[0].Err.Error(), "interrupted by shutdown") {
		t.Fatalf("Run = %#v, %v", results, err)
	}
	data, _ := os.ReadFile(f.state)
	var st agentUpgradeState
	_ = json.Unmarshal(data, &st)
	snap, ok := st.InFlight["kimi"]
	if !ok || snap.Backup == "" {
		t.Fatalf("record not kept: %s", data)
	}
	if _, err := os.Stat(snap.Backup); err != nil {
		t.Fatalf("backup removed: %v", err)
	}
}
