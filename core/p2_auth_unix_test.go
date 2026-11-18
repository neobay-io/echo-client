//go:build !windows

package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Status and logout run the agent's own CLI and read only what it prints.
func TestAuthStatusProbesParseCLIOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	marker := filepath.Join(dir, "logged-out")
	writeExecutable(t, filepath.Join(dir, "codex"), "#!/bin/sh\n"+
		"if [ \"$1 $2\" = \"login status\" ]; then if [ -e '"+marker+"' ]; then echo 'Not logged in'; exit 1; fi; echo 'Logged in using ChatGPT'; exit 0; fi\n"+
		"if [ \"$1\" = logout ]; then : > '"+marker+"'; echo 'Successfully logged out'; exit 0; fi\nexit 2\n")
	writeExecutable(t, filepath.Join(dir, "agy"), "#!/bin/sh\n"+
		"if [ -e '"+marker+"' ]; then echo 'Please sign in to continue: run agy to log in' >&2; exit 1; fi\n"+
		"printf 'Gemini Models\\tWeekly Limit Remaining\\t100%%\\n'\n")
	ctx := context.Background()

	if st, err := codexLoginStatus(ctx); err != nil || !st.LoggedIn || st.Detail != "ChatGPT" {
		t.Fatalf("codex status = %#v, %v", st, err)
	}
	if st, err := agyLoginStatus(ctx); err != nil || !st.LoggedIn {
		t.Fatalf("agy status = %#v, %v", st, err)
	}
	if err := defaultAuthDrivers()["codex"].logout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("codex logout did not run `codex logout`")
	}
	if st, err := codexLoginStatus(ctx); err != nil || st.LoggedIn {
		t.Fatalf("codex status after logout = %#v, %v", st, err)
	}
	if st, err := agyLoginStatus(ctx); err != nil || st.LoggedIn {
		t.Fatalf("agy status when signed out = %#v, %v", st, err)
	}
	// Output the probe cannot read is an error, not "logged out".
	writeExecutable(t, filepath.Join(dir, "codex"), "#!/bin/sh\necho 'segfault' >&2\nexit 139\n")
	if _, err := codexLoginStatus(ctx); err == nil {
		t.Fatal("unreadable codex status reported as a login state")
	}
}
