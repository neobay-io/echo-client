package kimi

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func scriptCLI(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake CLI")
	}
	path := filepath.Join(t.TempDir(), "kimi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testAgent(t *testing.T, cmd string) *Agent {
	t.Helper()
	a := &Agent{workDir: t.TempDir(), cmd: cmd}
	a.catalog = newModelCatalog(func(context.Context) ([]kimiModel, error) { return nil, nil })
	return a
}

// Review finding 4: stopping a turn must also stop what its tools started.
func TestCloseKillsCommandsStartedByTheTurn(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late-write")
	cmd := scriptCLI(t, `(sleep 1; touch "`+marker+`") &
echo '{"role":"assistant","content":"working"}'
sleep 30
`)
	s, err := testAgent(t, cmd).StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send("go", nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	_ = s.Close()
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a command started by the stopped turn kept running and wrote its file")
	}
}

// Review finding 3: Close racing Send must never panic (send on closed channel).
func TestSendCloseRaceDoesNotPanic(t *testing.T) {
	cmd := scriptCLI(t, `echo '{"role":"assistant","content":"x"}'
echo '{"role":"meta","type":"session.resume_hint","session_id":"s1"}'
`)
	a := testAgent(t, cmd)
	for i := 0; i < 40; i++ {
		s, err := a.StartSession(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.Send("hi", nil, nil) }()
		go func() { defer wg.Done(); _ = s.Close() }()
		wg.Wait()
		_ = s.Close()
	}
}

// Review finding 14: images saved for a turn whose CLI cannot start are removed.
func TestFailedStartRemovesSavedImages(t *testing.T) {
	a := testAgent(t, filepath.Join(t.TempDir(), "missing-kimi"))
	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	err = s.Send("look", []core.ImageAttachment{{MimeType: "image/png", Data: []byte("png"), FileName: "a.png"}}, nil)
	if err == nil {
		t.Fatal("Send should fail when the CLI is missing")
	}
	var left []string
	_ = filepath.Walk(a.workDir, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			left = append(left, path)
		}
		return nil
	})
	if len(left) != 0 {
		t.Fatalf("saved images left behind: %v", left)
	}
}

// Review finding 15: a read error ends the turn with one error, not error+result.
func TestReadErrorEndsTurnWithOneTerminalEvent(t *testing.T) {
	// One line longer than the 16 MiB scanner limit, then a clean exit.
	cmd := scriptCLI(t, `head -c 17000000 /dev/zero | tr '\0' 'a'
echo
exit 0
`)
	s, err := testAgent(t, cmd).StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("big", nil, nil); err != nil {
		t.Fatal(err)
	}
	var terminal []core.Event
	deadline := time.After(20 * time.Second)
	for {
		select {
		case evt := <-s.Events():
			if evt.Type == core.EventError || evt.Type == core.EventResult {
				terminal = append(terminal, evt)
			}
			continue
		case <-time.After(500 * time.Millisecond):
			if len(terminal) == 0 {
				continue
			}
		case <-deadline:
		}
		break
	}
	if len(terminal) != 1 || terminal[0].Type != core.EventError || !strings.Contains(terminal[0].Error.Error(), "read stdout") {
		t.Fatalf("terminal events = %+v, want exactly one read error", terminal)
	}
}
