package core

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInspectAgentCLIReportsSelectedCopyAndDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	root := t.TempDir()
	standalone := filepath.Join(root, "packages", "fakecli")
	writeExecutable(t, standalone, "#!/bin/sh\necho 'fakecli 1.2.3'\n")
	other := filepath.Join(root, "npm", "fakecli")
	writeExecutable(t, other, "#!/bin/sh\necho 'fakecli 9.9.9'\n")

	first := filepath.Join(root, "first")    // symlink to standalone: selected
	sameFile := filepath.Join(root, "same")  // another link to the same file
	npmDir := filepath.Join(root, "npmbin")  // a different install
	brokenDir := filepath.Join(root, "dead") // dangling link (half-installed)
	for _, dir := range []string{first, sameFile, npmDir, brokenDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustLink := func(target, link string) {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	mustLink(standalone, filepath.Join(first, "fakecli"))
	mustLink(standalone, filepath.Join(sameFile, "fakecli"))
	mustLink(other, filepath.Join(npmDir, "fakecli"))
	mustLink(filepath.Join(root, "missing", "fakecli.js"), filepath.Join(brokenDir, "fakecli"))

	t.Setenv("PATH", strings.Join([]string{first, first, sameFile, npmDir, brokenDir}, string(os.PathListSeparator)))
	info := InspectAgentCLI(context.Background(), "fakecli")

	if info.Selected == nil || info.Selected.Path != filepath.Join(first, "fakecli") {
		t.Fatalf("selected = %#v", info.Selected)
	}
	wantResolved, _ := filepath.EvalSymlinks(standalone)
	if info.Selected.Resolved != wantResolved || info.Version != "fakecli 1.2.3" || info.VersionErr != "" {
		t.Fatalf("selected install = %#v, version %q (%q)", info.Selected, info.Version, info.VersionErr)
	}
	if len(info.Others) != 2 || info.Others[0].Path != filepath.Join(npmDir, "fakecli") || !info.Others[1].Broken {
		t.Fatalf("others = %#v, want the npm copy and the broken link", info.Others)
	}

	result := AgentCLIDoctorResult(info)
	if result.Status != DoctorWarn || !strings.Contains(result.Detail, "fakecli 1.2.3") ||
		!strings.Contains(result.Detail, "not used by the service") || !strings.Contains(result.Detail, "(broken)") {
		t.Fatalf("doctor result = %#v", result)
	}
}

func TestInspectAgentCLIFlagsFailingVersionAndMissingBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "halfcli"), "#!/bin/sh\necho 'Error: Missing optional dependency' >&2\nexit 1\n")
	t.Setenv("PATH", dir)

	info := InspectAgentCLI(context.Background(), "halfcli")
	if info.VersionErr == "" || !strings.Contains(info.VersionErr, "Missing optional dependency") {
		t.Fatalf("version error = %q", info.VersionErr)
	}
	if got := AgentCLIDoctorResult(info).Status; got != DoctorWarn {
		t.Fatalf("status = %v, want warn", got)
	}

	missing := InspectAgentCLI(context.Background(), "nothing-here")
	if missing.Selected != nil || AgentCLIDoctorResult(missing).Status != DoctorFail {
		t.Fatalf("missing binary = %#v", missing)
	}
	if AgentCLIBinary("claudecode") != "claude" || AgentCLIBinary("antigravity") != "agy" {
		t.Fatal("agent binary map changed")
	}
}
