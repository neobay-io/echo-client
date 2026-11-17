//go:build windows

package core

// Cross-process upgrade locking is not implemented on Windows; upgrades are
// still serialized within one process.
var defaultAgentUpgradeLockDir = func() string { return "" }

func acquireUpgradeLock(dir, target string) (release func(), ok bool, err error) {
	return func() {}, true, nil
}

func upgradeLockHeld(dir, target string) bool { return false }
