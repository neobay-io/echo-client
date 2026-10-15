package core

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const commandOutputWaitDelay = 500 * time.Millisecond

// RunCommandOutput runs a short-lived discovery command without allowing
// descendant processes that retain stdout/stderr to outlive the deadline.
func RunCommandOutput(ctx context.Context, command string, args []string, workDir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	if env != nil {
		cmd.Env = env
	}
	prepareBoundedCommand(cmd)
	cmd.WaitDelay = commandOutputWaitDelay
	defer func() {
		if cmd.Process != nil && cmd.Cancel != nil {
			_ = cmd.Cancel()
		}
	}()
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}
