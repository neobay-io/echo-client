//go:build linux

package daemon

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvokingUserHomeUsesSudoUserLookup(t *testing.T) {
	origSudoUser := os.Getenv("SUDO_USER")
	if err := os.Setenv("SUDO_USER", "alice"); err != nil {
		t.Fatalf("Setenv SUDO_USER: %v", err)
	}
	defer func() {
		if err := os.Setenv("SUDO_USER", origSudoUser); err != nil {
			t.Fatalf("restore SUDO_USER: %v", err)
		}
	}()

	origLookup := lookupUser
	lookupUser = func(username string) (*user.User, error) {
		if username != "alice" {
			t.Fatalf("lookupUser called with %q", username)
		}
		return &user.User{HomeDir: "/Users/alice"}, nil
	}
	defer func() {
		lookupUser = origLookup
	}()

	if got := invokingUserHome(); got != "/Users/alice" {
		t.Fatalf("invokingUserHome() = %q", got)
	}
}

func TestOppositeScopeLegacyWarningDetectsSystemUnitForUserManager(t *testing.T) {
	tmpDir := t.TempDir()
	origSystemdSystemDir := systemdSystemDir
	systemdSystemDir = tmpDir
	defer func() {
		systemdSystemDir = origSystemdSystemDir
	}()

	legacyPath := filepath.Join(tmpDir, legacySystemdName)
	if err := os.WriteFile(legacyPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile legacy unit: %v", err)
	}

	warn := (&systemdManager{system: false}).oppositeScopeLegacyWarning()
	if !strings.Contains(warn, legacyPath) {
		t.Fatalf("warning %q does not mention %s", warn, legacyPath)
	}
	if !strings.Contains(warn, "sudo systemctl disable --now "+legacySystemdName) {
		t.Fatalf("warning %q missing manual cleanup guidance", warn)
	}
}
