//go:build !windows

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("child pid not written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Review 3, finding 2: a timed-out upgrade used to hang forever when an
// installer child outlived the shell and kept its output pipe open.
func TestUpgradeCommandTimeoutKillsChildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := defaultAgentUpgradeCommandRunner(ctx, "sleep 30 & echo $! > '"+pidFile+"'; wait")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("timed-out command reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not return after the timeout")
	}
	if pid := readPID(t, pidFile); !processGone(pid, 2*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("installer child %d survived the timeout", pid)
	}
}

// A command that succeeds but leaves a daemon holding its output still
// returns, and counts as success.
func TestUpgradeCommandLingeringChildIsBounded(t *testing.T) {
	prev := agentUpgradeWaitDelay
	agentUpgradeWaitDelay = 200 * time.Millisecond
	defer func() { agentUpgradeWaitDelay = prev }()
	pidFile := filepath.Join(t.TempDir(), "daemon.pid")

	done := make(chan error, 1)
	go func() {
		_, err := defaultAgentUpgradeCommandRunner(context.Background(), "sleep 30 & echo $! > '"+pidFile+"'; echo upgraded")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("err = %v, want success", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runner waited on the lingering child's output")
	}
	_ = syscall.Kill(readPID(t, pidFile), syscall.SIGKILL)
}
