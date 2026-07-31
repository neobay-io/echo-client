package core

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sessionHolder describes a live local process that currently holds a Claude
// Code session (terminal CLI, SDK, happy, or the desktop GUI). It is used by
// /attach to warn about — and optionally terminate — a process before taking
// over its session, avoiding two processes writing the same transcript.
type sessionHolder struct {
	PID        int
	SessionID  string
	Cwd        string
	Entrypoint string // e.g. "sdk-cli", "sdk-ts" (from the sessions index)
	Command    string // ps command line, for GUI detection and display
	IsGUI      bool
}

// scanActiveHolders reads ~/.claude/sessions/*.json (Claude Code's live-session
// index, one file per pid) and returns holders keyed by sessionId. Dead pids
// are skipped. Note the desktop GUI does not always register here — use
// holderForSession for a per-session check that also falls back to lsof.
func scanActiveHolders() map[string]sessionHolder {
	holders := map[string]sessionHolder{}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return holders
	}
	dir := filepath.Join(homeDir, ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return holders
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var idx struct {
			PID        int    `json:"pid"`
			SessionID  string `json:"sessionId"`
			Cwd        string `json:"cwd"`
			Entrypoint string `json:"entrypoint"`
		}
		if json.Unmarshal(data, &idx) != nil || idx.SessionID == "" || idx.PID <= 0 {
			continue
		}
		if !processAlive(idx.PID) {
			continue
		}
		cmd := processCommand(idx.PID)
		holders[idx.SessionID] = sessionHolder{
			PID:        idx.PID,
			SessionID:  idx.SessionID,
			Cwd:        idx.Cwd,
			Entrypoint: idx.Entrypoint,
			Command:    cmd,
			IsGUI:      isGUICommand(cmd),
		}
	}
	return holders
}

// holderForSession returns the live process holding a session, or nil if free.
// It checks the pre-scanned index first, then falls back to lsof on the
// transcript path — this catches the desktop GUI, which may hold the file open
// without registering in the sessions index.
func holderForSession(sessionID, transcriptPath string, index map[string]sessionHolder) *sessionHolder {
	if h, ok := index[sessionID]; ok {
		return &h
	}
	if transcriptPath == "" {
		return nil
	}
	pid := lsofHolder(transcriptPath)
	if pid <= 0 || !processAlive(pid) {
		return nil
	}
	cmd := processCommand(pid)
	return &sessionHolder{
		PID:       pid,
		SessionID: sessionID,
		Command:   cmd,
		IsGUI:     isGUICommand(cmd),
	}
}

// killHolder terminates a holder process with SIGTERM. GUI processes are only
// killed when force is true, because the desktop app is multi-process and
// killing a helper is risky; callers surface that risk to the user first.
func killHolder(h sessionHolder, force bool) error {
	if h.IsGUI && !force {
		return fmt.Errorf("refusing to kill GUI process %d without force", h.PID)
	}
	p, err := os.FindProcess(h.PID)
	if err != nil {
		return err
	}
	return p.Signal(syscall.SIGTERM)
}

// processAlive reports whether pid refers to a live process (signal 0 probe).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// processCommand returns the full command line of pid, or "" if unavailable.
func processCommand(pid int) string {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isGUICommand reports whether a command line belongs to the desktop GUI.
func isGUICommand(cmd string) bool {
	return strings.Contains(cmd, "Claude.app") || strings.Contains(cmd, "Claude Helper")
}

// lsofHolder returns the first pid holding path open, or 0 if none/unavailable.
func lsofHolder(path string) int {
	out, err := exec.Command("lsof", "-t", path).Output()
	if err != nil {
		return 0
	}
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil {
			return pid
		}
	}
	return 0
}

// waitProcessGone waits up to timeout for pid to exit, polling periodically.
// Used after killing a holder so the transcript is released before takeover.
func waitProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}
