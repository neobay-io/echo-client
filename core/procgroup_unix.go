//go:build !windows

package core

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// UseProcessGroup starts cmd as the leader of its own process group and makes
// context cancellation kill the whole group, so commands a CLI's tools
// started do not keep running after the turn was stopped or timed out. A CLI
// that exits on its own is not affected: its background children survive.
func UseProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error { return KillProcessGroup(cmd) }
}

// KillProcessGroup kills cmd's process group (see UseProcessGroup), falling
// back to the process itself.
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if kerr := cmd.Process.Kill(); kerr != nil && !errors.Is(kerr, os.ErrProcessDone) {
		return kerr
	}
	return nil
}

// KillGroupOnCancel makes context cancellation kill cmd's process group, for
// commands that already lead their own group (a PTY child is a session
// leader); UseProcessGroup would conflict with that.
func KillGroupOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return KillProcessGroup(cmd) }
}
