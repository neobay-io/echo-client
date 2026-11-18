//go:build !windows

package core

import (
	"path/filepath"
	"testing"
	"time"
)

// A git command whose ssh child hangs and keeps git's output open must still
// return when it times out (it used to block until the child exited).
func TestGitCommandTimeoutKillsHangingSSH(t *testing.T) {
	prev := workspaceCommandTimeout
	workspaceCommandTimeout = 500 * time.Millisecond
	defer func() { workspaceCommandTimeout = prev }()

	repoPath := initGitRepo(t)
	ssh := filepath.Join(t.TempDir(), "ssh")
	writeExecutable(t, ssh, "#!/bin/sh\nsleep 60 &\nwait\n")
	t.Setenv("GIT_SSH_COMMAND", ssh)
	if _, err := runGit(repoPath, "remote", "add", "origin", "ssh://git@example.invalid/repo.git"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runGit(repoPath, "fetch", "origin")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("fetch through a hanging ssh succeeded")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("git fetch did not return after its timeout")
	}
}
