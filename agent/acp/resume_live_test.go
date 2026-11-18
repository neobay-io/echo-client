//go:build live

package acp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// The last turn before a process is closed survives into the next process
// that resumes the session (a cron run on the chat's session, a chat whose
// process is restarted after one).
func TestLiveResumeKeepsLastTurn(t *testing.T) {
	for _, c := range []struct{ agent, command string }{{"kimi", "kimi"}, {"antigravity", "agy"}} {
		t.Run(c.agent, func(t *testing.T) {
			path, err := core.ResolveAgentCLI(c.agent)
			if err != nil {
				path, err = exec.LookPath(c.command)
			}
			if err != nil {
				t.Skipf("%s not installed", c.agent)
			}
			dir := t.TempDir()
			start := func(resume string) *Session {
				s, err := Start(context.Background(), Config{Agent: c.agent, Command: path, Args: []string{"acp"}, Env: os.Environ(), WorkDir: dir, Mode: core.PermissionModeDefault, ResumeID: resume})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			a := start("")
			liveTurn(t, a, "Remember the word PELICAN. Reply with exactly: OK")
			id := a.CurrentSessionID()
			_ = a.Close()

			b := start(id)
			text, _ := liveTurn(t, b, "Reply with exactly: CRONOK")
			if !strings.Contains(text, "CRONOK") {
				t.Fatalf("second turn: %q", text)
			}
			t0 := time.Now()
			_ = b.Close() // right after the result, as a cron run does
			t.Logf("%s: Close took %s", c.agent, time.Since(t0).Round(time.Millisecond))

			cs := start(id)
			defer func() { _ = cs.Close() }()
			text, _ = liveTurn(t, cs, "What exact word did you reply with in your previous message, and which word did I ask you to remember? Reply with the two words only.")
			t.Logf("%s after two restarts: %q", c.agent, text)
			if !strings.Contains(text, "CRONOK") || !strings.Contains(strings.ToUpper(text), "PELICAN") {
				t.Errorf("%s lost a turn across restarts: %q", c.agent, text)
			}
		})
	}
}

// A second process resumes the session while the first is still running
// (idle), as a cron run on a chat's session does.
func TestLiveConcurrentResume(t *testing.T) {
	for _, c := range []struct{ agent, command string }{{"kimi", "kimi"}, {"antigravity", "agy"}} {
		t.Run(c.agent, func(t *testing.T) {
			path, err := core.ResolveAgentCLI(c.agent)
			if err != nil {
				path, err = exec.LookPath(c.command)
			}
			if err != nil {
				t.Skipf("%s not installed", c.agent)
			}
			dir := t.TempDir()
			start := func(resume string) *Session {
				s, err := Start(context.Background(), Config{Agent: c.agent, Command: path, Args: []string{"acp"}, Env: os.Environ(), WorkDir: dir, Mode: core.PermissionModeDefault, ResumeID: resume})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			a := start("")
			liveTurn(t, a, "Remember the word PELICAN. Reply with exactly: OK")
			id := a.CurrentSessionID()
			liveTurn(t, a, "Reply with exactly: ZEBRA")

			b := start(id) // a is still alive
			liveTurn(t, b, "Reply with exactly: CRONOK")
			_ = b.Close()
			_ = a.Close()

			cs := start(id)
			defer func() { _ = cs.Close() }()
			text, _ := liveTurn(t, cs, "List every word you replied with in this conversation, in order, separated by spaces. Nothing else.")
			t.Logf("%s after a concurrent resume: %q", c.agent, text)
			if !strings.Contains(text, "ZEBRA") || !strings.Contains(text, "CRONOK") {
				t.Errorf("%s lost a turn: %q", c.agent, text)
			}
		})
	}
}
