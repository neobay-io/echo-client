//go:build !windows

package core

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// defaultAgentUpgradeLockDir is shared by every echo-client process of this
// user, so two services cannot upgrade the same CLI at the same time.
var defaultAgentUpgradeLockDir = func() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "echo-client", "locks")
	}
	return filepath.Join(os.TempDir(), "echo-client-locks")
}

func upgradeLockPath(dir, target string) string {
	return filepath.Join(dir, "agent-upgrade-"+target+".lock")
}

// acquireUpgradeLock takes the cross-process lock for target without
// waiting. ok is false when another process holds it.
func acquireUpgradeLock(dir, target string) (release func(), ok bool, err error) {
	if dir == "" {
		return func() {}, true, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(upgradeLockPath(dir, target), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

// upgradeLockHeld reports whether some process currently holds target's
// upgrade lock.
func upgradeLockHeld(dir, target string) bool {
	if dir == "" {
		return false
	}
	f, err := os.Open(upgradeLockPath(dir, target))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
