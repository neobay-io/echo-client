package core

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A copy whose open the system holds (here a FIFO with no writer) is
// stopped after cloneTimeout and not retried in process.
func TestCopyExecutableStopsHeldCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "held")
	if err := syscall.Mkfifo(src, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	old := cloneTimeout
	cloneTimeout = 300 * time.Millisecond
	t.Cleanup(func() { cloneTimeout = old })

	dst := filepath.Join(dir, "copy")
	done := make(chan error, 1)
	go func() { done <- copyExecutable(src, dst) }()
	select {
	case err := <-done:
		if !errors.Is(err, errCopyTimeout) {
			t.Fatalf("copyExecutable = %v, want errCopyTimeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("copyExecutable did not return")
	}
	if _, err := os.Lstat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst left behind: %v", err)
	}
}
