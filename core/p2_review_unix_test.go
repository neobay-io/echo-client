//go:build !windows

package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readInFlight(t *testing.T, state string) map[string]upgradeSnapshot {
	t.Helper()
	data, _ := os.ReadFile(state)
	var st agentUpgradeState
	_ = json.Unmarshal(data, &st)
	return st.InFlight
}

// P2 review 1: an installer that deletes the binary and fails is rolled back.
func TestRollbackRestoresDeletedBinary(t *testing.T) {
	f := newGuardFixture(t, "rm $DIR/bin/kimi; exit 1")
	old := fakeCLIScript("1.0", "--prompt", "--model")
	writeExecutable(t, f.bin, old)
	res := f.run(t)
	if !res.RolledBack || res.Err == nil {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
	if data, _ := os.ReadFile(f.bin); string(data) != old {
		t.Fatalf("binary not restored: %q", data)
	}
	f.assertCleanedUp(t)
}

// P2 review 2: an updater that rewrites the binary in place (same inode)
// does not corrupt the backup.
func TestRollbackSurvivesInPlaceRewrite(t *testing.T) {
	f := newGuardFixture(t, "printf '#!/bin/sh\\nexit 1\\n' > $DIR/bin/kimi")
	old := fakeCLIScript("1.0", "--prompt", "--model")
	writeExecutable(t, f.bin, old)
	res := f.run(t)
	if !res.RolledBack {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
	if data, _ := os.ReadFile(f.bin); string(data) != old {
		t.Fatalf("binary not restored: %q", data)
	}
	f.assertCleanedUp(t)
}

// P2 review 1: when the rollback itself fails, the record and backup stay
// for the next start; rollback never replaces a regular file with a link.
func TestFailedRollbackKeepsRecord(t *testing.T) {
	f := newGuardFixture(t, "rm $DIR/bin/kimi && printf '#!/bin/sh\\nexit 1\\n' > $DIR/bin/kimi && chmod +x $DIR/bin/kimi")
	writeExecutable(t, filepath.Join(f.dir, "versions", "1.0"), fakeCLIScript("1.0", "--prompt", "--model"))
	_ = os.MkdirAll(filepath.Dir(f.bin), 0o755)
	_ = os.Symlink(filepath.Join(f.dir, "versions", "1.0"), f.bin)

	res := f.run(t)
	if res.RolledBack || res.Err == nil || !strings.Contains(res.Err.Error(), "rollback failed") ||
		!strings.Contains(res.Err.Error(), "no longer a symlink") {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
	if fi, _ := os.Lstat(f.bin); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("rollback replaced the installer's regular file with a link")
	}
	snap, ok := readInFlight(t, f.state)["kimi"]
	if !ok || snap.Backup == "" {
		t.Fatal("record not kept after a failed rollback")
	}
	if _, err := os.Stat(snap.Backup); err != nil {
		t.Fatalf("backup removed after a failed rollback: %v", err)
	}
}

// P2 review 5: a record read back from the state file that does not match
// the installed CLI is not acted on, and a backup path outside the backup
// directory is never deleted.
func TestRecoveryIgnoresForeignRecord(t *testing.T) {
	f := newGuardFixture(t, "unused")
	writeExecutable(t, f.bin, fakeCLIScript("1.0", "--prompt", "--model"))
	victim := filepath.Join(f.dir, "important.txt")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	f.mgr.setInFlight(&upgradeSnapshot{
		Target: "kimi", Path: filepath.Join(f.dir, "elsewhere", "kimi"), Real: victim,
		Links: []upgradeLink{{Path: victim, Target: "/etc/passwd"}}, Backup: victim, Healthy: true, StartedAt: time.Now(),
	})
	restarted := NewAgentUpgradeManager(f.mgr.Snapshot())
	restarted.SetStatePath(f.state)
	restarted.SetCLIInspector(func(context.Context, string) AgentCLIInfo {
		return AgentCLIInfo{Agent: "kimi", Binary: "kimi", Selected: &AgentCLIInstall{Path: f.bin}}
	})
	restarted.recoverInterruptedUpgrades(context.Background())
	if data, err := os.ReadFile(victim); err != nil || string(data) != "keep" {
		t.Fatalf("foreign file touched: %q, %v", data, err)
	}
	if fi, _ := os.Lstat(victim); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("foreign file replaced with a link")
	}
	if len(readInFlight(t, f.state)) != 0 {
		t.Fatal("foreign record kept")
	}

	// Same with a matching path but a backup outside the backup directory:
	// the CLI is healthy, so recovery just ends; the "backup" must survive.
	f.mgr.setInFlight(&upgradeSnapshot{Target: "kimi", Path: f.bin, Real: f.bin, Backup: victim, Healthy: true, StartedAt: time.Now()})
	again := NewAgentUpgradeManager(f.mgr.Snapshot())
	again.SetStatePath(f.state)
	again.SetCLIInspector(func(context.Context, string) AgentCLIInfo {
		return AgentCLIInfo{Agent: "kimi", Binary: "kimi", Selected: &AgentCLIInstall{Path: f.bin}}
	})
	again.recoverInterruptedUpgrades(context.Background())
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("backup outside the backup directory was deleted: %v", err)
	}
}

// P2 review 12: the watcher follows the binary the adapter runs.
type pathAgent struct {
	namedStubAgent
	path string
}

func (a *pathAgent) CLIPath() string { return a.path }

func TestCLIWatchUsesAdapterPath(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "kimi")
	writeExecutable(t, bin, "#!/bin/sh\n")
	e := NewEngine("p", &pathAgent{namedStubAgent{name: "kimi"}, bin}, nil, "", LangEnglish)
	e.noteAgentCLIChange()
	e.interactiveStates["k"] = &interactiveState{}
	writeExecutable(t, bin, "#!/bin/sh\necho upgraded\n")
	e.noteAgentCLIChange()
	if !e.interactiveStates["k"].stale {
		t.Fatal("a change to the adapter's own binary was not noticed")
	}
}

// P2 review K5: rollback restores the innermost links first and stops once
// the old binary is reached, leaving a parent link alone that someone
// rewrote meanwhile (here to an equivalent relative form).
func TestRollbackLeavesUnrelatedParentLinks(t *testing.T) {
	f := newGuardFixture(t, "ln -sfn $DIR/home/versions/2.0 $DIR/home/bin/kimi && ln -sfn homeA $DIR/home")
	writeExecutable(t, filepath.Join(f.dir, "homeA", "versions", "1.0"), fakeCLIScript("1.0", "--prompt", "--model"))
	writeExecutable(t, filepath.Join(f.dir, "homeA", "versions", "2.0"), fakeCLIScript("2.0", "--prompt"))
	_ = os.MkdirAll(filepath.Join(f.dir, "homeA", "bin"), 0o755)
	_ = os.Symlink(filepath.Join(f.dir, "homeA"), filepath.Join(f.dir, "home"))
	_ = os.Symlink(filepath.Join(f.dir, "home", "versions", "1.0"), filepath.Join(f.dir, "home", "bin", "kimi"))
	f.bin = filepath.Join(f.dir, "home", "bin", "kimi")

	res := f.run(t)
	if !res.RolledBack {
		t.Fatalf("result = %#v (err %v)", res, res.Err)
	}
	if target, _ := os.Readlink(filepath.Join(f.dir, "home")); target != "homeA" {
		t.Fatalf("parent link rewritten to %q", target)
	}
	if target, _ := os.Readlink(filepath.Join(f.dir, "homeA", "bin", "kimi")); target != filepath.Join(f.dir, "home", "versions", "1.0") {
		t.Fatalf("binary link = %q", target)
	}
}
