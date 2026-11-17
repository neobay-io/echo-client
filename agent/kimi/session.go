package kimi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

type sessionConfig struct {
	cmd       string
	extraArgs []string
	workDir   string
	model     string
	effort    string
	extraDirs []string
	extraEnv  []string
	timeout   time.Duration
}

// kimiSession runs one `kimi -p` process per Send and resumes the same Kimi
// session with -S on later turns.
type kimiSession struct {
	cfg       sessionConfig
	events    chan core.Event
	sessionID atomic.Value // string
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	alive     atomic.Bool
	closeOnce sync.Once
	// startMu orders a Send's admission (alive check + wg.Add) against Close,
	// so Close never closes events while a turn it did not wait for runs.
	startMu sync.Mutex

	pendingMsgs []string // assistant text not yet known to be final
}

func newKimiSession(ctx context.Context, cfg sessionConfig, resumeID string) *kimiSession {
	sessionCtx, cancel := context.WithCancel(ctx)
	ks := &kimiSession{
		cfg:    cfg,
		events: make(chan core.Event, 64),
		ctx:    sessionCtx,
		cancel: cancel,
	}
	ks.alive.Store(true)
	if resumeID = strings.TrimSpace(resumeID); resumeID != "" {
		ks.sessionID.Store(resumeID)
	}
	return ks
}

// buildArgs builds one prompt-mode invocation. Permission flags are never
// passed: Kimi Code rejects --plan/--yolo/--auto together with -p.
func (ks *kimiSession) buildArgs(prompt string) []string {
	args := append([]string{}, ks.cfg.extraArgs...)
	args = append(args, "--output-format", "stream-json")
	if sid := ks.CurrentSessionID(); sid != "" {
		args = append(args, "-S", sid)
	}
	if ks.cfg.model != "" {
		args = append(args, "--model", ks.cfg.model)
	}
	for _, dir := range ks.cfg.extraDirs {
		if strings.TrimSpace(dir) != "" {
			args = append(args, "--add-dir", dir)
		}
	}
	// --prompt=<text> keeps prompts that start with "-" from being parsed as
	// options.
	return append(args, "--prompt="+prompt)
}

func (ks *kimiSession) buildEnv() []string {
	extra := append([]string(nil), ks.cfg.extraEnv...)
	if ks.cfg.effort != "" {
		extra = append(extra, effortEnv+"="+ks.cfg.effort)
	}
	return core.MergeEnv(os.Environ(), extra)
}

func (ks *kimiSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) (err error) {
	ks.startMu.Lock()
	if !ks.alive.Load() {
		ks.startMu.Unlock()
		return fmt.Errorf("session is closed")
	}
	ks.wg.Add(1)
	ks.startMu.Unlock()

	imagePaths := core.SaveImagesToDisk(ks.cfg.workDir, images)
	started := false
	defer func() {
		if !started {
			removeFiles(imagePaths)
			ks.wg.Done()
		}
	}()
	filePaths := core.SaveFilesToDisk(ks.cfg.workDir, files)
	if strings.TrimSpace(prompt) == "" && len(imagePaths) > 0 {
		prompt = "Please analyze the attached image(s)."
	}
	if len(imagePaths) > 0 {
		prompt += "\n\n(Images saved locally, please read them: " + strings.Join(imagePaths, ", ") + ")"
	}
	prompt = core.AppendInteractionOptions(core.AppendFileRefs(prompt, filePaths))

	ctx, cancel := context.WithCancel(ks.ctx)
	if ks.cfg.timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(ks.ctx, ks.cfg.timeout)
	}
	args := ks.buildArgs(prompt)
	slog.Debug("kimiSession: launching", "resume", ks.CurrentSessionID() != "", "args", core.RedactArgs(args))
	cmd := exec.CommandContext(ctx, ks.cfg.cmd, args...)
	core.UseProcessGroup(cmd) // stopping the turn also stops the commands its tools started
	cmd.WaitDelay = time.Second
	cmd.Dir = ks.cfg.workDir
	cmd.Env = ks.buildEnv()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("kimiSession: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("kimiSession: start: %w", err)
	}
	started = true

	go func() {
		defer ks.wg.Done()
		defer cancel()
		defer removeFiles(imagePaths)
		ks.readLoop(ctx, cancel, cmd, stdout, &stderrBuf)
	}()
	return nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

// readLoop streams one turn and ends it with exactly one terminal event: an
// error or a result. A closed session gets none.
func (ks *kimiSession) readLoop(ctx context.Context, cancel context.CancelFunc, cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	go func() {
		<-ctx.Done()
		_ = stdout.Close()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			slog.Debug("kimiSession: non-JSON line", "len", len(line))
			continue
		}
		ks.handleEvent(raw)
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// Nothing drains stdout any more; a CLI still writing would block on
		// the full pipe and the turn would never end.
		cancel()
	}
	waitErr := cmd.Wait()

	if ks.ctx.Err() != nil {
		return // closed
	}
	var failure string
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		failure = fmt.Sprintf("kimi turn timed out after %s", ks.cfg.timeout)
	case scanErr != nil:
		failure = "read stdout: " + scanErr.Error()
	case waitErr != nil:
		failure = strings.TrimSpace(stderrBuf.String())
		if failure == "" {
			failure = waitErr.Error()
		}
	}
	if failure != "" {
		slog.Error("kimiSession: turn failed", "error", waitErr, "detail", truncate(failure, 500))
		ks.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("%s", failure)})
		return
	}
	ks.flushPendingAsText()
	ks.emit(core.Event{Type: core.EventResult, SessionID: ks.CurrentSessionID(), Done: true})
}

func (ks *kimiSession) emit(evt core.Event) {
	select {
	case ks.events <- evt:
	case <-ks.ctx.Done():
	}
}

// handleEvent dispatches one stream-json line. Kimi Code emits
//
//	{"role":"assistant","content":"…","tool_calls":[…]}
//	{"role":"tool","tool_call_id":"…","content":"…"}
//	{"role":"meta","type":"session.resume_hint","session_id":"…"}
func (ks *kimiSession) handleEvent(raw map[string]any) {
	switch role, _ := raw["role"].(string); role {
	case "assistant":
		ks.handleAssistant(raw)
	case "tool":
		ks.handleTool(raw)
	case "meta":
		ks.handleMeta(raw)
	}
}

func (ks *kimiSession) handleMeta(raw map[string]any) {
	if t, _ := raw["type"].(string); t != "session.resume_hint" {
		return
	}
	if id, _ := raw["session_id"].(string); strings.TrimSpace(id) != "" {
		ks.sessionID.Store(strings.TrimSpace(id))
	}
}

func (ks *kimiSession) handleAssistant(raw map[string]any) {
	switch content := raw["content"].(type) {
	case string:
		if content != "" {
			ks.pendingMsgs = append(ks.pendingMsgs, content)
		}
	case []any:
		for _, item := range content {
			block, _ := item.(map[string]any)
			switch blockType, _ := block["type"].(string); blockType {
			case "think", "thinking":
				if think, _ := block["think"].(string); think != "" {
					ks.emit(core.Event{Type: core.EventThinking, Content: think})
				}
			case "text":
				if text, _ := block["text"].(string); text != "" {
					ks.pendingMsgs = append(ks.pendingMsgs, text)
				}
			}
		}
	}

	toolCalls, _ := raw["tool_calls"].([]any)
	if len(toolCalls) == 0 {
		return
	}
	// Text before a tool call is narration, not the final answer.
	ks.flushPendingAsThinking()
	for _, tc := range toolCalls {
		tcMap, _ := tc.(map[string]any)
		fn, _ := tcMap["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		id, _ := tcMap["id"].(string)
		ks.emit(core.Event{
			Type:      core.EventToolUse,
			ToolName:  name,
			ToolInput: truncate(strings.TrimSpace(args), 500),
			RequestID: id,
		})
	}
}

func (ks *kimiSession) handleTool(raw map[string]any) {
	var output string
	switch content := raw["content"].(type) {
	case string:
		output = content
	case []any:
		var parts []string
		for _, item := range content {
			block, _ := item.(map[string]any)
			if t, _ := block["type"].(string); t == "text" {
				if text, _ := block["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		output = strings.Join(parts, "")
	}
	if output == "" {
		return
	}
	id, _ := raw["tool_call_id"].(string)
	ks.emit(core.Event{Type: core.EventToolResult, RequestID: id, ToolResult: truncate(strings.TrimSpace(output), 500)})
}

func (ks *kimiSession) flushPendingAsThinking() {
	text := strings.Join(ks.pendingMsgs, "")
	ks.pendingMsgs = ks.pendingMsgs[:0]
	if text != "" {
		ks.emit(core.Event{Type: core.EventThinking, Content: text})
	}
}

func (ks *kimiSession) flushPendingAsText() {
	text := strings.Join(ks.pendingMsgs, "")
	ks.pendingMsgs = ks.pendingMsgs[:0]
	if text != "" {
		ks.emit(core.Event{Type: core.EventText, Content: text})
	}
}

// RespondPermission is a no-op: prompt mode never asks.
func (ks *kimiSession) RespondPermission(string, core.PermissionResult) error { return nil }

func (ks *kimiSession) Events() <-chan core.Event { return ks.events }

func (ks *kimiSession) CurrentSessionID() string {
	v, _ := ks.sessionID.Load().(string)
	return v
}

func (ks *kimiSession) Alive() bool { return ks.alive.Load() }

func (ks *kimiSession) Close() error {
	ks.startMu.Lock()
	ks.alive.Store(false)
	ks.startMu.Unlock()
	ks.cancel()
	done := make(chan struct{})
	go func() {
		ks.wg.Wait()
		ks.closeOnce.Do(func() { close(ks.events) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("kimiSession: close timed out")
	}
	return nil
}

func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "..."
}
