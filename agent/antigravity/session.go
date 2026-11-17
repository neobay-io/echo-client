package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type sessionConfig struct {
	cmd       string
	extraArgs []string
	workDir   string
	model     string
	effort    string
	mode      string
	extraDirs []string
	extraEnv  []string
	timeout   time.Duration
}

// antigravitySession runs one `agy -p` process per Send and continues the
// conversation with --conversation on later turns.
type antigravitySession struct {
	cfg              sessionConfig
	events           chan core.Event
	closeOnce        sync.Once
	chatID           atomic.Value // string
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	alive            atomic.Bool
	permissionBridge *agyPermissionBridge
}

func newAntigravitySession(ctx context.Context, cfg sessionConfig, resumeID string) (*antigravitySession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	as := &antigravitySession{
		cfg:    cfg,
		events: make(chan core.Event, 64),
		ctx:    sessionCtx,
		cancel: cancel,
	}
	as.alive.Store(true)
	if cfg.mode == core.PermissionModeDefault {
		bridge, err := newAgyPermissionBridge(sessionCtx, as.events)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("antigravity: initialize permission bridge: %w", err)
		}
		as.permissionBridge = bridge
	}
	if resumeID = strings.TrimSpace(resumeID); resumeID != "" {
		as.chatID.Store(resumeID)
	}
	return as, nil
}

func modeArgs(mode string) []string {
	switch mode {
	case core.PermissionModeAcceptEdits:
		return []string{"--mode", "accept-edits"}
	case core.PermissionModePlan:
		return []string{"--mode", "plan"}
	case core.PermissionModeAuto:
		return []string{"--mode", "auto"}
	case core.PermissionModeBypass:
		return []string{"--dangerously-skip-permissions"}
	}
	return nil
}

// buildArgs keeps the prompt last: agy's flag parser takes the next argument
// as the value of -p.
func (as *antigravitySession) buildArgs(prompt string) []string {
	args := append([]string{}, as.cfg.extraArgs...)
	if as.permissionBridge != nil {
		// Hidden compatibility flag: point agy at a config overlay whose
		// hooks.json adds the approval hook (see permission_bridge.go).
		args = append(args, "--gemini_dir="+as.permissionBridge.AgyConfigDir())
	}
	if id := as.CurrentSessionID(); id != "" {
		args = append(args, "--conversation", id)
	}
	args = append(args, "--output-format", "stream-json")
	args = append(args, modeArgs(as.cfg.mode)...)
	args = append(args, modelArgs(as.cfg.model, as.cfg.effort)...)
	for _, dir := range as.cfg.extraDirs {
		if strings.TrimSpace(dir) != "" {
			args = append(args, "--add-dir", dir)
		}
	}
	return append(args, "-p", prompt)
}

func (as *antigravitySession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	imagePaths := core.SaveImagesToDisk(as.cfg.workDir, images)
	filePaths := core.SaveFilesToDisk(as.cfg.workDir, files)
	if strings.TrimSpace(prompt) == "" && len(imagePaths) > 0 {
		prompt = "Please analyze the attached image(s)."
	}
	if len(imagePaths) > 0 {
		prompt += "\n\n(Images saved locally, please read them: " + strings.Join(imagePaths, ", ") + ")"
	}
	prompt = core.AppendInteractionOptions(core.AppendFileRefs(prompt, filePaths))

	ctx, cancel := context.WithCancel(as.ctx)
	if as.cfg.timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(as.ctx, as.cfg.timeout)
	}
	args := as.buildArgs(prompt)
	slog.Debug("antigravitySession: launching", "resume", as.CurrentSessionID() != "", "args", core.RedactArgs(args))
	cmd := exec.CommandContext(ctx, as.cfg.cmd, args...)
	cmd.WaitDelay = time.Second
	cmd.Dir = as.cfg.workDir
	env := core.MergeEnv(os.Environ(), as.cfg.extraEnv)
	if as.permissionBridge != nil {
		env = core.MergeEnv(env, as.permissionBridge.Env())
	}
	cmd.Env = env
	// stdin stays /dev/null: agy -p reads piped stdin to EOF before starting.

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("antigravitySession: stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("antigravitySession: start: %w", err)
	}

	as.wg.Add(1)
	go func() {
		defer cancel()
		as.readLoop(ctx, cmd, stdout, &stderrBuf)
		for _, path := range imagePaths {
			_ = os.Remove(path)
		}
	}()
	return nil
}

func (as *antigravitySession) readLoop(ctx context.Context, cmd *exec.Cmd, stdout io.ReadCloser, stderrBuf *bytes.Buffer) {
	defer as.wg.Done()
	go func() {
		<-ctx.Done()
		_ = stdout.Close()
	}()

	parser := newAgyOutputParser()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	streamSeen := false
	for scanner.Scan() {
		recognized, events := parser.parseLine(scanner.Bytes())
		if recognized {
			streamSeen = true
		} else if !streamSeen && len(bytes.TrimSpace(scanner.Bytes())) > 0 {
			// A wrapper or old CLI ignoring --output-format: plain text.
			events = []core.Event{{Type: core.EventText, Content: scanner.Text() + "\n"}}
		}
		for _, event := range events {
			if event.SessionID != "" {
				as.chatID.Store(event.SessionID)
			}
			as.emit(event)
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	if as.ctx.Err() != nil {
		return // closed
	}
	var failure string
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		failure = fmt.Sprintf("agy turn timed out after %s", as.cfg.timeout)
	case parser.failure != "":
		failure = parser.failure
	case waitErr != nil:
		failure = strings.TrimSpace(stderrBuf.String())
		if failure == "" {
			failure = waitErr.Error()
		}
	case scanErr != nil:
		failure = "read stdout: " + scanErr.Error()
	}
	if failure != "" {
		slog.Error("antigravitySession: turn failed", "error", waitErr, "detail", truncate(failure, 500))
		as.emit(core.Event{Type: core.EventError, Error: fmt.Errorf("%s", failure)})
		return
	}
	as.emit(core.Event{Type: core.EventResult, SessionID: as.CurrentSessionID(), Done: true})
}

func (as *antigravitySession) emit(evt core.Event) {
	select {
	case as.events <- evt:
	case <-as.ctx.Done():
	}
}

func (as *antigravitySession) RespondPermission(requestID string, result core.PermissionResult) error {
	if !as.alive.Load() {
		return fmt.Errorf("session is closed")
	}
	if as.permissionBridge == nil {
		return fmt.Errorf("antigravity: permission responses are only available in default mode")
	}
	return as.permissionBridge.RespondPermission(requestID, result)
}

func (as *antigravitySession) Events() <-chan core.Event { return as.events }

func (as *antigravitySession) CurrentSessionID() string {
	v, _ := as.chatID.Load().(string)
	return v
}

func (as *antigravitySession) Alive() bool { return as.alive.Load() }

func (as *antigravitySession) Close() error {
	as.alive.Store(false)
	as.cancel()
	if as.permissionBridge != nil {
		as.permissionBridge.Close()
	}
	done := make(chan struct{})
	go func() {
		as.wg.Wait()
		as.closeOnce.Do(func() { close(as.events) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("antigravitySession: close timed out")
	}
	return nil
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}
