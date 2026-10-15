//go:build darwin || linux

package core

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestRunCommandOutputKillsDescendantsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err := RunCommandOutput(ctx, "/bin/sh", []string{"-c", "sleep 30 & wait"}, "", os.Environ())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunCommandOutput error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("RunCommandOutput exceeded bounded cleanup: %v", elapsed)
	}
}
