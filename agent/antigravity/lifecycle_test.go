package antigravity

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

func scriptAgy(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake CLI")
	}
	path := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func bypassAgent(t *testing.T, cmd string) *Agent {
	t.Helper()
	a := &Agent{workDir: t.TempDir(), cmd: cmd, mode: "bypassPermissions"}
	a.catalog = newModelCatalog(func(context.Context) ([]agyModel, error) { return nil, nil })
	return a
}

// Review finding 4: stopping a turn must also stop what its tools started.
func TestCloseKillsCommandsStartedByTheTurn(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late-write")
	cmd := scriptAgy(t, `(sleep 1; touch "`+marker+`") &
echo '{"event":"init","conversation_id":"c1","init":{}}'
sleep 30
`)
	s, err := bypassAgent(t, cmd).StartSession(context.Background(), "")
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

// Review finding 3: Close racing Send must never panic, also with the
// default-mode permission bridge.
func TestSendCloseRaceDoesNotPanic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := scriptAgy(t, `echo '{"event":"init","conversation_id":"c1","init":{}}'
echo '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"x"}}'
`)
	for _, mode := range []string{"bypassPermissions", "default"} {
		a := bypassAgent(t, cmd)
		a.mode = mode
		for i := 0; i < 20; i++ {
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
}

// Review finding 14: images saved for a turn whose CLI cannot start are removed.
func TestFailedStartRemovesSavedImages(t *testing.T) {
	a := bypassAgent(t, filepath.Join(t.TempDir(), "missing-agy"))
	s, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("look", []core.ImageAttachment{{MimeType: "image/png", Data: []byte("png"), FileName: "a.png"}}, nil); err == nil {
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

// A line longer than the scanner limit must end the turn with one error
// instead of blocking the CLI on a full pipe forever.
func TestReadErrorEndsTurn(t *testing.T) {
	cmd := scriptAgy(t, `echo '{"event":"init","conversation_id":"c1","init":{}}'
head -c 34000000 /dev/zero | tr '\0' 'a'
echo
exit 0
`)
	s, err := bypassAgent(t, cmd).StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Send("big", nil, nil); err != nil {
		t.Fatal(err)
	}
	events := collectTurn(t, s, nil)
	last := events[len(events)-1]
	if last.Type != core.EventError || !strings.Contains(last.Error.Error(), "read stdout") {
		t.Fatalf("last = %+v", last)
	}
}
