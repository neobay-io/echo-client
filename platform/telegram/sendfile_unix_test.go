//go:build !windows

package telegram

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Review 3: FIFOs and devices are refused before anything is opened; a FIFO
// with no writer used to block the upload forever.
func TestSendFileRejectsSpecialFiles(t *testing.T) {
	bot, fake := newFakeBot(t)
	p := &Platform{bot: bot}
	rc := replyContext{chatID: 42}
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, "/dev/null"} {
		done := make(chan error, 1)
		go func() { done <- p.SendFile(context.Background(), rc, path, "") }()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("SendFile(%s) = %v, want a not-a-regular-file error", path, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("SendFile(%s) blocked", path)
		}
	}
	for _, call := range fake.calls {
		if call == "sendDocument" {
			t.Fatal("a special file was uploaded")
		}
	}
}
