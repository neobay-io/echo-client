//go:build windows

package core

import (
	"errors"
	"os"
	"os/exec"
)

// UseProcessGroup is a no-op on Windows; cancellation kills the process.
func UseProcessGroup(cmd *exec.Cmd) {}

// KillProcessGroup kills the process.
func KillProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// KillGroupOnCancel is a no-op on Windows; cancellation kills the process.
func KillGroupOnCancel(cmd *exec.Cmd) {}
