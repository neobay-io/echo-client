package claudecode

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Opt-in: forks the supplied transcript, disables tools and persistence, and
// checks the real CLI protocol without changing the user's original session.
func TestClaudeResumeNotificationIntegration(t *testing.T) {
	binary := os.Getenv("CLAUDE_RESUME_TEST_BINARY")
	id := os.Getenv("CLAUDE_RESUME_TEST_ID")
	dir := os.Getenv("CLAUDE_RESUME_TEST_DIR")
	if binary == "" || id == "" || dir == "" {
		t.Skip("set CLAUDE_RESUME_TEST_BINARY, CLAUDE_RESUME_TEST_ID and CLAUDE_RESUME_TEST_DIR")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--resume", id, "--fork-session", "--no-session-persistence",
		"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--permission-prompt-tool", "stdio", "--tools", "")
	cmd.Dir = dir
	cmd.Env = filterEnv(os.Environ(), "CLAUDECODE")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cs := &claudeSession{cmd: cmd, stdin: stdin, events: make(chan core.Event, 64), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	cs.alive.Store(true)
	cs.resuming.Store(true)
	cs.resumeID = id
	go cs.readLoop(stdout, &stderr)
	defer func() { _ = cs.Close() }()
	if err := cs.Send("This is a read-only diagnostic in a forked session, not a continuation of prior tasks. Reply only PROBE_OK and stop. Do not use tools or continue previous work.", nil, nil); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event, ok := <-cs.Events():
			if !ok {
				t.Fatal("CLI exited before a result")
			}
			if event.Type == core.EventError {
				t.Fatalf("CLI failed: %v", event.Error)
			}
			if event.Type == core.EventResult {
				if !strings.Contains(event.Content, "PROBE_OK") {
					t.Fatalf("premature/unexpected result: %q", event.Content)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestClaudeReadLoopClosesDoneWhenCancelledWithStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.Command("sh", "-c", "printf failure >&2; exit 1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cs := &claudeSession{cmd: cmd, events: make(chan core.Event), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	cancel()
	go cs.readLoop(stdout, &stderr)
	select {
	case <-cs.done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled error path did not close done")
	}
	if _, ok := <-cs.events; ok {
		t.Fatal("events channel not closed")
	}
}
