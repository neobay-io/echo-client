package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Codex over `codex app-server` (P4): one long-running process speaking
// JSON-RPC 2.0 over stdio, one thread per chat session, a turn/start per
// message. Unlike `codex exec` it can steer and interrupt a running turn,
// forward approvals and questions to the chat, switch settings between turns
// without a new process, and report token and rate-limit usage.

// ── JSON-RPC client ───────────────────────────────────────────

// rpcCallTimeout bounds one request, including writing it.
var rpcCallTimeout = 30 * time.Second

type rpcReply struct {
	result json.RawMessage
	err    error
}

// rpcError is a JSON-RPC error response.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

type rpcClient struct {
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan rpcReply
	done    chan struct{} // closed when the reader stops
	// Set before readLoop starts.
	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
	// onBroken is called once when a write does not finish in time: the
	// message may still go out later, so the transport cannot be trusted
	// (a late turn/start could run with settings the user has since
	// changed). The owner kills the process.
	onBroken   func()
	brokenOnce sync.Once
}

func newRPCClient(stdin io.WriteCloser) *rpcClient {
	return &rpcClient{stdin: stdin, pending: make(map[int64]chan rpcReply), done: make(chan struct{})}
}

var errAppServerClosed = errors.New("codex app-server exited")

func (c *rpcClient) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

// writeBounded writes v, giving up when ctx ends or the reader stops: a
// server that stopped reading must not hang the caller.
func (c *rpcClient) writeBounded(ctx context.Context, v any) error {
	errc := make(chan error, 1)
	go func() { errc <- c.write(v) }()
	select {
	case err := <-errc:
		return err
	case <-c.done:
		return errAppServerClosed
	case <-ctx.Done():
		c.brokenOnce.Do(func() {
			slog.Warn("codex app-server: a write timed out; stopping the process")
			if c.onBroken != nil {
				c.onBroken()
			}
		})
		return ctx.Err()
	}
}

// call sends a request and waits for its result.
func (c *rpcClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, rpcCallTimeout)
	defer cancel()
	if err := c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("codex %s: %w", method, err)
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("codex %s: %w", method, r.err)
		}
		return r.result, nil
	case <-c.done:
		return nil, fmt.Errorf("codex %s: %w", method, errAppServerClosed)
	case <-ctx.Done():
		return nil, fmt.Errorf("codex %s: %w", method, ctx.Err())
	}
}

func (c *rpcClient) notify(method string, params any) error {
	ctx, cancel := context.WithTimeout(context.Background(), rpcCallTimeout)
	defer cancel()
	return c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *rpcClient) respond(ctx context.Context, id json.RawMessage, result any) error {
	return c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (c *rpcClient) respondError(id json.RawMessage, code int, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcCallTimeout)
	defer cancel()
	_ = c.writeBounded(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
}

// readLoop dispatches responses, server requests and notifications until r
// ends. Lines can be large (aggregated command output).
func (c *rpcClient) readLoop(r io.Reader) {
	defer close(c.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			if c.onRequest != nil {
				c.onRequest(msg.ID, msg.Method, msg.Params)
			} else {
				c.respondError(msg.ID, -32601, "not supported")
			}
		case msg.Method != "":
			if c.onNotify != nil {
				c.onNotify(msg.Method, msg.Params)
			}
		case len(msg.ID) > 0:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch == nil {
				continue
			}
			reply := rpcReply{result: msg.Result}
			if msg.Error != nil {
				reply.err = msg.Error
			}
			select {
			case ch <- reply:
			default:
			}
		}
	}
	if err := sc.Err(); err != nil {
		slog.Warn("codex app-server: read failed", "error", err)
	}
}

// errAppServerUnavailable: this Codex CLI could not run app-server; the
// agent falls back to `codex exec`.
var errAppServerUnavailable = errors.New("codex app-server unavailable")

// appServerProc is a running `codex app-server`.
type appServerProc struct {
	cmd *exec.Cmd
	rpc *rpcClient
	out io.Reader
}

// startAppServer starts `codex app-server --stdio` (not yet reading).
func startAppServer(ctx context.Context, workDir string, env []string) (*appServerProc, error) {
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--stdio")
	cmd.Dir = workDir
	cmd.Env = env
	core.UseProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %v", errAppServerUnavailable, err)
	}
	return &appServerProc{cmd: cmd, rpc: newRPCClient(stdin), out: stdout}, nil
}

// serve starts reading and initializes the connection.
func (p *appServerProc) serve(ctx context.Context) error {
	go func() {
		p.rpc.readLoop(p.out)
		_ = p.cmd.Wait()
	}()
	_, err := p.rpc.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "echo-client", "version": "1"},
		// Experimental API: collaboration (plan) mode.
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err != nil {
		// Only a CLI that clearly has no app-server (it exited before
		// answering, or does not know the method) counts as unavailable;
		// a timeout is an ordinary failure, not a reason to downgrade.
		var rerr *rpcError
		if errors.Is(err, errAppServerClosed) || (errors.As(err, &rerr) && rerr.Code == -32601) {
			return fmt.Errorf("%w: %v", errAppServerUnavailable, err)
		}
		return err
	}
	return p.rpc.notify("initialized", map[string]any{})
}

func (p *appServerProc) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.rpc.stdin.Close()
	_ = core.KillProcessGroup(p.cmd)
	select {
	case <-p.rpc.done:
	case <-time.After(5 * time.Second):
	}
}

// ── Session ───────────────────────────────────────────────────

// codexDefaults are what "default" means for a chat: the user's Codex
// configuration (config/read), not the thread's current settings, which keep
// whatever an earlier turn overrode (turn/start overrides persist, also
// across thread/resume). They are sent back to leave a mode or setting.
type codexDefaults struct {
	Model             string
	ReasoningEffort   string
	ApprovalPolicy    any // AskForApproval
	ApprovalsReviewer string
	SandboxMode       string // read-only | workspace-write | danger-full-access
	WritableRoots     []string
	NetworkAccess     bool
}

// readCodexDefaults reads the effective configuration for cwd. Missing
// values fall back to Codex's usual interactive defaults: on-request
// approvals in a workspace-write sandbox, medium effort.
func readCodexDefaults(ctx context.Context, rpc *rpcClient, cwd string) codexDefaults {
	d := codexDefaults{ApprovalPolicy: "on-request", ApprovalsReviewer: "user", SandboxMode: "workspace-write", ReasoningEffort: "medium"}
	raw, err := rpc.call(ctx, "config/read", map[string]any{"cwd": cwd})
	if err != nil {
		slog.Warn("codex app-server: config/read failed; using built-in defaults for default mode", "error", err)
		return d
	}
	var resp struct {
		Config struct {
			ApprovalPolicy        any    `json:"approval_policy"`
			ApprovalsReviewer     string `json:"approvals_reviewer"`
			SandboxMode           string `json:"sandbox_mode"`
			Model                 string `json:"model"`
			ModelReasoningEffort  string `json:"model_reasoning_effort"`
			SandboxWorkspaceWrite *struct {
				WritableRoots []string `json:"writable_roots"`
				NetworkAccess bool     `json:"network_access"`
			} `json:"sandbox_workspace_write"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return d
	}
	c := resp.Config
	if c.ApprovalPolicy != nil {
		d.ApprovalPolicy = c.ApprovalPolicy
	}
	if c.ApprovalsReviewer != "" {
		d.ApprovalsReviewer = c.ApprovalsReviewer
	}
	if c.SandboxMode != "" {
		d.SandboxMode = c.SandboxMode
	}
	d.Model = c.Model
	if c.ModelReasoningEffort != "" {
		d.ReasoningEffort = c.ModelReasoningEffort
	}
	if w := c.SandboxWorkspaceWrite; w != nil {
		d.WritableRoots, d.NetworkAccess = w.WritableRoots, w.NetworkAccess
	}
	return d
}

// sandboxPolicy is the SandboxPolicy for a configured sandbox mode, with the
// session's extra directories writable in workspace-write.
func (d codexDefaults) sandboxPolicy(extraDirs []string) map[string]any {
	switch d.SandboxMode {
	case "read-only":
		return map[string]any{"type": "readOnly"}
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}
	default:
		roots := append(append([]string{}, d.WritableRoots...), extraDirs...)
		return map[string]any{"type": "workspaceWrite", "writableRoots": roots, "networkAccess": d.NetworkAccess}
	}
}

// serverRequest is an approval or question Codex is waiting on.
type serverRequest struct {
	id        json.RawMessage
	method    string
	params    map[string]any
	questions map[string]string // requestUserInput: question text → id
	// done closes when Codex stops waiting (answered, resolved by itself,
	// or the process ended); the engine stops waiting on the prompt then.
	done chan struct{}
}

type agentText struct {
	text  string
	phase string
}

type tokenBreakdown struct {
	TotalTokens  int `json:"totalTokens"`
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
}

type threadTokenUsage struct {
	Total              tokenBreakdown `json:"total"`
	Last               tokenBreakdown `json:"last"`
	ModelContextWindow int            `json:"modelContextWindow"`
}

type appServerSession struct {
	proc    *appServerProc
	events  chan core.Event
	ctx     context.Context
	cancel  context.CancelFunc
	alive   atomic.Bool
	workDir string

	threadID atomic.Value // string

	mu        sync.Mutex
	turnID    string // running turn, "" between turns
	endedTurn string // last turn that completed (a late turn/start reply must not revive it)
	model     string
	effort    string
	mode      string
	extraDirs []string
	defaults  codexDefaults
	// threadModel is the model the thread reported (used when neither the
	// chat nor the config names one).
	threadModel string
	// inPlan: the thread is (confirmed) in plan collaboration mode.
	inPlan    bool
	requests  map[string]serverRequest
	fileItems map[string]string // fileChange item id → changed paths
	pending   []agentText       // agent messages awaiting classification
	usage     *threadTokenUsage

	eventsMu     sync.Mutex
	eventsClosed bool
	closeOnce    sync.Once
}

func newAppServerSession(ctx context.Context, workDir, model, effort, mode, resumeID string, extraDirs, extraEnv []string) (*appServerSession, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	proc, err := startAppServer(sessionCtx, workDir, core.MergeEnv(os.Environ(), extraEnv))
	if err != nil {
		cancel()
		return nil, err
	}
	s := &appServerSession{
		proc: proc, ctx: sessionCtx, cancel: cancel, workDir: workDir,
		events: make(chan core.Event, 64), model: model, effort: effort, mode: mode,
		extraDirs: append([]string(nil), extraDirs...),
		requests:  make(map[string]serverRequest), fileItems: make(map[string]string),
		// A resumed thread may still be in plan mode from an earlier
		// process: the first turn states the collaboration mode either way.
		inPlan: true,
	}
	proc.rpc.onNotify = s.handleNotification
	proc.rpc.onRequest = s.handleServerRequest
	proc.rpc.onBroken = func() { go func() { _ = s.Close() }() }
	if err := proc.serve(sessionCtx); err != nil {
		proc.stop()
		cancel()
		return nil, err
	}
	s.defaults = readCodexDefaults(sessionCtx, proc.rpc, workDir)
	if err := s.openThread(resumeID); err != nil {
		proc.stop()
		cancel()
		return nil, err
	}
	s.alive.Store(true)
	go func() {
		<-proc.rpc.done
		s.alive.Store(false)
		s.mu.Lock()
		running := s.turnID != ""
		for id, req := range s.requests {
			closeRequest(req)
			delete(s.requests, id)
		}
		s.mu.Unlock()
		if running && s.ctx.Err() == nil {
			s.emit(core.Event{Type: core.EventError, Error: errAppServerClosed})
		}
		s.closeEvents()
	}()
	return s, nil
}

// openThread starts a thread, or resumes resumeID, with the session's
// workspace; settings are sent per turn.
func (s *appServerSession) openThread(resumeID string) error {
	params := map[string]any{"cwd": s.workDir}
	method := "thread/start"
	if resumeID != "" {
		method = "thread/resume"
		params["threadId"] = resumeID
		// Only the thread's metadata is needed; a long history could
		// exceed the line limit.
		params["excludeTurns"] = true
	}
	raw, err := s.proc.rpc.call(s.ctx, method, params)
	if err != nil {
		var rerr *rpcError
		if errors.As(err, &rerr) && rerr.Code == -32601 {
			return fmt.Errorf("%w: %v", errAppServerUnavailable, err)
		}
		if resumeID != "" && isThreadNotFound(err) {
			return fmt.Errorf("%w: %v", core.ErrAgentSessionNotFound, err)
		}
		return err
	}
	var resp struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("codex %s: %w", method, err)
	}
	if resp.Thread.ID == "" {
		return fmt.Errorf("codex %s: no thread id", method)
	}
	s.threadID.Store(resp.Thread.ID)
	s.threadModel = resp.Model
	return nil
}

// isThreadNotFound matches only a missing-thread error, not other "not
// found" failures (a model or provider) that must not reset the session.
func isThreadNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return (strings.Contains(msg, "thread") || strings.Contains(msg, "rollout") || strings.Contains(msg, "conversation")) &&
		(strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || strings.Contains(msg, "does not exist"))
}

func (s *appServerSession) CurrentSessionID() string {
	v, _ := s.threadID.Load().(string)
	return v
}

func (s *appServerSession) Alive() bool { return s.alive.Load() }

func (s *appServerSession) Events() <-chan core.Event { return s.events }

func (s *appServerSession) emit(ev core.Event) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if s.eventsClosed {
		return
	}
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}

func (s *appServerSession) closeEvents() {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if !s.eventsClosed {
		s.eventsClosed = true
		close(s.events)
	}
}

func (s *appServerSession) Close() error {
	s.closeOnce.Do(func() {
		s.alive.Store(false)
		s.cancel()
		s.proc.stop()
		s.closeEvents()
	})
	return nil
}

// setSettings changes model, effort and mode for the next turns (they are
// turn/start parameters, so no new process is needed).
func (s *appServerSession) setSettings(model, effort, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model, s.effort, s.mode = model, effort, mode
}

// userInput builds a turn's input: text plus local images.
func (s *appServerSession) userInput(prompt string, images []core.ImageAttachment, files []core.FileAttachment) []map[string]any {
	if len(files) > 0 {
		prompt = core.AppendFileRefs(prompt, core.SaveFilesToDisk(s.workDir, files))
	}
	input := []map[string]any{}
	if strings.TrimSpace(prompt) == "" && len(images) > 0 {
		prompt = "Please analyze the attached image(s)."
	}
	if prompt != "" {
		input = append(input, map[string]any{"type": "text", "text": prompt, "text_elements": []any{}})
	}
	for _, path := range core.SaveImagesToDisk(s.workDir, images) {
		input = append(input, map[string]any{"type": "localImage", "path": path})
	}
	return input
}

// turnSettingsLocked are the per-turn overrides for the session's settings.
// Overrides persist on the thread, so every turn states all of them; the
// user's Codex configuration stands for "default". It returns whether the
// turn asks for plan mode.
func (s *appServerSession) turnSettingsLocked(params map[string]any) bool {
	model := firstNonEmpty(s.model, s.defaults.Model, s.threadModel)
	if model != "" {
		params["model"] = model
	}
	effort := firstNonEmpty(s.effort, s.defaults.ReasoningEffort)
	params["effort"] = effort
	plan := false
	switch s.mode {
	case "acceptEdits":
		params["approvalPolicy"] = "never"
		params["approvalsReviewer"] = "user"
		params["sandboxPolicy"] = codexDefaults{SandboxMode: "workspace-write"}.sandboxPolicy(s.extraDirs)
	case "auto":
		params["approvalPolicy"] = "on-request"
		params["approvalsReviewer"] = "auto_review"
		params["sandboxPolicy"] = codexDefaults{SandboxMode: "workspace-write"}.sandboxPolicy(s.extraDirs)
	case "bypassPermissions":
		params["approvalPolicy"] = "never"
		params["approvalsReviewer"] = "user"
		params["sandboxPolicy"] = map[string]any{"type": "dangerFullAccess"}
	default: // default, plan
		params["approvalPolicy"] = s.defaults.ApprovalPolicy
		params["approvalsReviewer"] = s.defaults.ApprovalsReviewer
		params["sandboxPolicy"] = s.defaults.sandboxPolicy(s.extraDirs)
		plan = s.mode == "plan"
	}
	// Collaboration mode is sent while entering plan, while in it, and until
	// leaving it is confirmed by a started turn.
	if plan || s.inPlan {
		kind := "default"
		if plan {
			kind = "plan"
		}
		params["collaborationMode"] = map[string]any{"mode": kind, "settings": map[string]any{"model": model, "reasoning_effort": effort}}
	}
	return plan
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Send starts a turn with the message.
func (s *appServerSession) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.Alive() {
		return fmt.Errorf("session is closed")
	}
	prompt = core.WrapInteractionOptions(prompt)
	params := map[string]any{"threadId": s.CurrentSessionID(), "input": s.userInput(prompt, images, files)}
	s.mu.Lock()
	plan := s.turnSettingsLocked(params)
	s.pending = nil
	s.mu.Unlock()
	raw, err := s.proc.rpc.call(s.ctx, "turn/start", params)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// Codex may have accepted the turn and run it unseen: stop the
			// process; the next message starts a new one and resumes.
			slog.Warn("codex app-server: turn/start did not answer in time; stopping the process")
			_ = s.Close()
		}
		return err
	}
	var resp struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(raw, &resp)
	s.mu.Lock()
	s.inPlan = plan // the server accepted this turn's collaboration mode
	// The turn may already have started and even completed (notifications
	// can overtake this reply): never revive a finished turn.
	if resp.Turn.ID != "" && s.turnID == "" && resp.Turn.ID != s.endedTurn {
		s.turnID = resp.Turn.ID
	}
	s.mu.Unlock()
	return nil
}

// Steer implements core.Steerer (turn/steer into the running turn).
func (s *appServerSession) Steer(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	s.mu.Lock()
	turn := s.turnID
	s.mu.Unlock()
	if turn == "" {
		return errors.New("no turn is running")
	}
	_, err := s.proc.rpc.call(s.ctx, "turn/steer", map[string]any{
		"threadId": s.CurrentSessionID(), "expectedTurnId": turn, "input": s.userInput(prompt, images, files),
	})
	return err
}

// Interrupt implements core.Interrupter (turn/interrupt; the turn then
// completes with status "interrupted").
func (s *appServerSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	turn := s.turnID
	s.mu.Unlock()
	if turn == "" {
		return errors.New("no turn is running")
	}
	_, err := s.proc.rpc.call(ctx, "turn/interrupt", map[string]any{"threadId": s.CurrentSessionID(), "turnId": turn})
	return err
}

// ContextUsage implements core.ContextReporter from the last token usage.
func (s *appServerSession) ContextUsage(context.Context) (*core.ContextReport, error) {
	s.mu.Lock()
	u := s.usage
	s.mu.Unlock()
	if u == nil {
		return nil, errors.New("no turn has run in this session yet")
	}
	r := &core.ContextReport{UsedTokens: u.Last.TotalTokens, MaxTokens: u.ModelContextWindow}
	if u.ModelContextWindow > 0 {
		r.Percent = u.Last.TotalTokens * 100 / u.ModelContextWindow
	}
	r.Categories = []core.ContextCategory{
		{Name: "Input (last request)", Tokens: u.Last.InputTokens},
		{Name: "Output (last request)", Tokens: u.Last.OutputTokens},
		{Name: "Total this session", Tokens: u.Total.TotalTokens},
	}
	return r, nil
}

// SessionUsage implements core.SessionUsageReporter (rate limits).
func (s *appServerSession) SessionUsage(ctx context.Context) (*core.UsageReport, error) {
	return readRateLimits(ctx, s.proc.rpc)
}

func readRateLimits(ctx context.Context, rpc *rpcClient) (*core.UsageReport, error) {
	raw, err := rpc.call(ctx, "account/rateLimits/read", map[string]any{})
	if err != nil {
		return nil, err
	}
	return parseCodexRateLimits(raw)
}

type rateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins int      `json:"windowDurationMins"`
	ResetsAt           int64    `json:"resetsAt"`
}

func parseCodexRateLimits(raw json.RawMessage) (*core.UsageReport, error) {
	var resp struct {
		RateLimits struct {
			Primary   *rateLimitWindow `json:"primary"`
			Secondary *rateLimitWindow `json:"secondary"`
			PlanType  string           `json:"planType"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	report := &core.UsageReport{Plan: resp.RateLimits.PlanType}
	for _, w := range []*rateLimitWindow{resp.RateLimits.Primary, resp.RateLimits.Secondary} {
		if w == nil {
			continue
		}
		line := core.UsageLine{Name: windowName(w.WindowDurationMins), UsedPercent: -1}
		if w.UsedPercent != nil {
			line.UsedPercent = int(*w.UsedPercent)
		}
		if w.ResetsAt > 0 {
			line.ResetsAt = time.Unix(w.ResetsAt, 0)
		}
		report.Lines = append(report.Lines, line)
	}
	return report, nil
}

func windowName(mins int) string {
	switch {
	case mins <= 0:
		return "Limit"
	case mins%(24*60) == 0:
		return fmt.Sprintf("%d-day limit", mins/(24*60))
	case mins%60 == 0:
		return fmt.Sprintf("%d-hour limit", mins/60)
	default:
		return fmt.Sprintf("%d-minute limit", mins)
	}
}

// ── Notifications ─────────────────────────────────────────────

func (s *appServerSession) handleNotification(method string, raw json.RawMessage) {
	var params map[string]any
	_ = json.Unmarshal(raw, &params)
	// Sub-agent threads report on the same connection; only this thread's
	// turns and items belong to the chat.
	if tid, _ := params["threadId"].(string); tid != "" && tid != s.CurrentSessionID() {
		return
	}
	switch method {
	case "serverRequest/resolved":
		id := strings.TrimSpace(string(mustJSON(params["requestId"])))
		s.mu.Lock()
		if req, ok := s.requests[id]; ok {
			closeRequest(req)
			delete(s.requests, id)
		}
		s.mu.Unlock()
	case "turn/started":
		if turn, _ := params["turn"].(map[string]any); turn != nil {
			if id, _ := turn["id"].(string); id != "" {
				s.mu.Lock()
				s.turnID = id
				s.mu.Unlock()
			}
		}
	case "item/started":
		item, _ := params["item"].(map[string]any)
		s.itemStarted(item)
	case "item/completed":
		item, _ := params["item"].(map[string]any)
		s.itemCompleted(item)
	case "turn/completed":
		turn, _ := params["turn"].(map[string]any)
		s.turnCompleted(turn)
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage threadTokenUsage `json:"tokenUsage"`
		}
		if json.Unmarshal(raw, &p) == nil {
			s.mu.Lock()
			s.usage = &p.TokenUsage
			s.mu.Unlock()
		}
	case "error":
		if retry, _ := params["willRetry"].(bool); !retry {
			if e, _ := params["error"].(map[string]any); e != nil {
				msg, _ := e["message"].(string)
				slog.Warn("codex app-server: turn error", "message", msg)
			}
		}
	}
}

func (s *appServerSession) flushPending(as core.EventType) {
	s.mu.Lock()
	msgs := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, m := range msgs {
		typ := as
		if m.phase == "commentary" {
			typ = core.EventThinking
		}
		s.emit(core.Event{Type: typ, Content: m.text})
	}
}

func (s *appServerSession) itemStarted(item map[string]any) {
	typ, _ := item["type"].(string)
	switch typ {
	case "commandExecution":
		s.flushPending(core.EventThinking)
		command, _ := item["command"].(string)
		s.emit(core.Event{Type: core.EventToolUse, ToolName: "Bash", ToolInput: command})
	case "fileChange":
		s.flushPending(core.EventThinking)
		paths := fileChangePaths(item)
		if id, _ := item["id"].(string); id != "" {
			s.mu.Lock()
			s.fileItems[id] = paths
			s.mu.Unlock()
		}
		s.emit(core.Event{Type: core.EventToolUse, ToolName: "Edit", ToolInput: paths})
	case "mcpToolCall":
		s.flushPending(core.EventThinking)
		server, _ := item["server"].(string)
		tool, _ := item["tool"].(string)
		s.emit(core.Event{Type: core.EventToolUse, ToolName: strings.Trim(server+"."+tool, "."), ToolInput: compactJSON(item["arguments"])})
	case "webSearch":
		s.flushPending(core.EventThinking)
		query, _ := item["query"].(string)
		s.emit(core.Event{Type: core.EventToolUse, ToolName: "WebSearch", ToolInput: query})
	}
}

func (s *appServerSession) itemCompleted(item map[string]any) {
	typ, _ := item["type"].(string)
	switch typ {
	case "agentMessage":
		text, _ := item["text"].(string)
		if strings.TrimSpace(text) == "" {
			return
		}
		phase, _ := item["phase"].(string)
		s.mu.Lock()
		s.pending = append(s.pending, agentText{text: text, phase: phase})
		s.mu.Unlock()
	case "reasoning":
		if text := joinStrings(item["summary"]); text != "" {
			s.emit(core.Event{Type: core.EventThinking, Content: text})
		}
	case "plan":
		// The proposed plan is the answer of a plan-mode turn.
		if text, _ := item["text"].(string); strings.TrimSpace(text) != "" {
			s.mu.Lock()
			s.pending = append(s.pending, agentText{text: text, phase: "final_answer"})
			s.mu.Unlock()
		}
	case "commandExecution":
		output, _ := item["aggregatedOutput"].(string)
		result := strings.TrimSpace(output)
		if result == "" {
			status, _ := item["status"].(string)
			exit, _ := item["exitCode"].(float64)
			result = fmt.Sprintf("status=%s exit_code=%d", status, int(exit))
		}
		s.emit(core.Event{Type: core.EventToolResult, ToolName: "Bash", Content: result})
	case "fileChange":
		status, _ := item["status"].(string)
		s.emit(core.Event{Type: core.EventToolResult, ToolName: "Edit", Content: strings.TrimSpace(status + " " + fileChangePaths(item))})
	case "mcpToolCall":
		status, _ := item["status"].(string)
		s.emit(core.Event{Type: core.EventToolResult, ToolName: "ToolCall", Content: status})
	}
}

func (s *appServerSession) turnCompleted(turn map[string]any) {
	status, _ := turn["status"].(string)
	s.mu.Lock()
	s.turnID = ""
	if id, _ := turn["id"].(string); id != "" {
		s.endedTurn = id
	}
	s.mu.Unlock()
	switch status {
	case "failed":
		s.flushPending(core.EventText)
		msg := "turn failed"
		if e, _ := turn["error"].(map[string]any); e != nil {
			if m, _ := e["message"].(string); m != "" {
				msg = m
			}
		}
		s.emit(core.Event{Type: core.EventError, Error: errors.New(msg)})
	default:
		s.flushPending(core.EventText)
		s.emit(core.Event{Type: core.EventResult, SessionID: s.CurrentSessionID(), Done: true, Interrupted: status == "interrupted"})
	}
}

func fileChangePaths(item map[string]any) string {
	changes, _ := item["changes"].([]any)
	var paths []string
	for _, c := range changes {
		if m, _ := c.(map[string]any); m != nil {
			if p, _ := m["path"].(string); p != "" {
				paths = append(paths, p)
			}
		}
	}
	return strings.Join(paths, ", ")
}

func joinStrings(v any) string {
	list, _ := v.([]any)
	var parts []string
	for _, x := range list {
		if s, _ := x.(string); strings.TrimSpace(s) != "" {
			parts = append(parts, strings.TrimSpace(s))
		}
	}
	return strings.Join(parts, "\n")
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func closeRequest(req serverRequest) {
	if req.done != nil {
		select {
		case <-req.done:
		default:
			close(req.done)
		}
	}
}

// RequestDone implements core.PendingRequestWatcher: closed when Codex no
// longer waits for the answer to requestID.
func (s *appServerSession) RequestDone(requestID string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req, ok := s.requests[requestID]; ok {
		return req.done
	}
	closed := make(chan struct{})
	close(closed)
	return closed
}

func compactJSON(v any) string {
	if v == nil {
		return ""
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return truncate(string(data), 300)
}

// ── Approvals and questions (T4.4) ────────────────────────────

func (s *appServerSession) handleServerRequest(id json.RawMessage, method string, raw json.RawMessage) {
	var params map[string]any
	_ = json.Unmarshal(raw, &params)
	req := serverRequest{id: id, method: method, params: params, done: make(chan struct{})}
	ev := core.Event{Type: core.EventPermissionRequest, RequestID: string(id)}
	switch method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		command := firstString(params, "command")
		if command == "" && params["command"] != nil {
			command = compactJSONFull(params["command"])
		}
		kind, _ := params["kind"].(string)
		cwd, _ := params["cwd"].(string)
		if command == "" {
			// e.g. input to a running terminal ("writeStdin"): without the
			// command there is nothing to judge, so it is declined.
			if kind == "" {
				s.answerUnreviewable(id, method)
				return
			}
			command = "(" + kind + ")"
		} else if kind != "" && kind != "command" {
			command = "(" + kind + ") " + command
		}
		if cwd != "" {
			command += "\n(in " + cwd + ")"
		}
		ev.ToolName, ev.ToolInput = "Bash", withReason(command, params)
		ev.ToolInputRaw = map[string]any{"command": command}
	case "item/fileChange/requestApproval", "applyPatchApproval":
		itemID, _ := params["itemId"].(string)
		s.mu.Lock()
		paths := s.fileItems[itemID]
		s.mu.Unlock()
		if paths == "" {
			paths = compactJSON(params["fileChanges"])
		}
		ev.ToolName, ev.ToolInput = "Edit", withReason(paths, params)
		ev.ToolInputRaw = map[string]any{"files": paths}
	case "item/permissions/requestApproval":
		// The card must show everything an approval grants; a request too
		// large to show in full is declined.
		summary := summarizePermissions(params["permissions"])
		if len(summary) > maxPermissionSummary {
			ctx, cancel := context.WithTimeout(s.ctx, rpcCallTimeout)
			_ = s.proc.rpc.respond(ctx, id, map[string]any{"permissions": map[string]any{}, "scope": "turn"})
			cancel()
			s.emit(core.Event{Type: core.EventText, Content: "⚠️ Codex asked for additional permissions too broad to show here in full; declined.\n"})
			return
		}
		ev.ToolName, ev.ToolInput = "Permissions", withReason(summary, params)
		ev.ToolInputRaw = map[string]any{"permissions": params["permissions"]}
	case "item/tool/requestUserInput":
		if hasSecretQuestion(params) {
			// A secret typed into chat would stay in the chat's history.
			s.proc.rpc.respondError(id, -32603, "secret input is not collected over chat")
			s.emit(core.Event{Type: core.EventText, Content: "⚠️ Codex asked for secret input, which echo-client does not collect in chat; declined.\n"})
			return
		}
		input, questions := userInputAsAskUser(params)
		if len(questions) == 0 {
			s.proc.rpc.respondError(id, -32602, "no questions")
			return
		}
		req.questions = questions
		ev.ToolName, ev.ToolInputRaw = core.ToolAskUserQuestion, input
	default:
		s.proc.rpc.respondError(id, -32601, "echo-client does not handle "+method)
		return
	}
	s.mu.Lock()
	s.requests[string(id)] = req
	s.mu.Unlock()
	s.emit(ev)
}

// answerUnreviewable declines a request the chat could not show.
func (s *appServerSession) answerUnreviewable(id json.RawMessage, method string) {
	var payload any = map[string]any{"decision": "decline"}
	if method == "execCommandApproval" || method == "applyPatchApproval" {
		payload = map[string]any{"decision": map[string]any{"denied": map[string]any{"rejection": "Nothing to show for review."}}}
	}
	ctx, cancel := context.WithTimeout(s.ctx, rpcCallTimeout)
	defer cancel()
	_ = s.proc.rpc.respond(ctx, id, payload)
	s.emit(core.Event{Type: core.EventText, Content: "⚠️ Codex asked to approve an action without saying what it is; declined.\n"})
}

func firstString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func withReason(text string, params map[string]any) string {
	if reason, _ := params["reason"].(string); strings.TrimSpace(reason) != "" {
		return strings.TrimSpace(text + "\n(" + reason + ")")
	}
	return text
}

// maxPermissionSummary is the longest permission request shown in chat (the
// engine's permission prompt shows 800 characters by default).
const maxPermissionSummary = 600

// summarizePermissions lists every grant in a permission profile: network
// access and each read / write path.
func summarizePermissions(v any) string {
	profile, _ := v.(map[string]any)
	var parts []string
	if net, _ := profile["network"].(map[string]any); net != nil {
		parts = append(parts, "network: "+compactJSONFull(net))
	}
	if fs, _ := profile["fileSystem"].(map[string]any); fs != nil {
		keys := make([]string, 0, len(fs))
		for k := range fs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, k+": "+compactJSONFull(fs[k]))
		}
	}
	for k, v := range profile {
		if k != "network" && k != "fileSystem" && v != nil {
			parts = append(parts, k+": "+compactJSONFull(v))
		}
	}
	if len(parts) == 0 {
		return "(no details)"
	}
	return strings.Join(parts, "\n")
}

func compactJSONFull(v any) string { return string(mustJSON(v)) }

func hasSecretQuestion(params map[string]any) bool {
	list, _ := params["questions"].([]any)
	for _, item := range list {
		if q, _ := item.(map[string]any); q != nil {
			if secret, _ := q["isSecret"].(bool); secret {
				return true
			}
		}
	}
	return false
}

// userInputAsAskUser turns a requestUserInput into the AskUserQuestion
// shape core's question cards use; it returns the question text → id map.
// Question texts are made unique (answers are keyed by text).
func userInputAsAskUser(params map[string]any) (map[string]any, map[string]string) {
	list, _ := params["questions"].([]any)
	var questions []any
	ids := map[string]string{}
	for _, item := range list {
		q, _ := item.(map[string]any)
		if q == nil {
			continue
		}
		text, _ := q["question"].(string)
		id, _ := q["id"].(string)
		if text == "" || id == "" {
			continue
		}
		for n := 2; ids[text] != ""; n++ {
			base, _ := q["question"].(string)
			text = fmt.Sprintf("%s (%d)", base, n)
		}
		ids[text] = id
		header, _ := q["header"].(string)
		var options []any
		if opts, ok := q["options"].([]any); ok {
			for _, o := range opts {
				if om, _ := o.(map[string]any); om != nil {
					options = append(options, map[string]any{"label": om["label"], "description": om["description"]})
				}
			}
		}
		questions = append(questions, map[string]any{"question": text, "header": header, "options": options})
	}
	return map[string]any{"questions": questions}, ids
}

// RespondPermission answers an approval or question.
func (s *appServerSession) RespondPermission(requestID string, result core.PermissionResult) error {
	s.mu.Lock()
	req, ok := s.requests[requestID]
	delete(s.requests, requestID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no pending codex request %s", requestID)
	}
	defer closeRequest(req)
	allow := result.Behavior == "allow"
	var payload any
	switch req.method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		decision := "decline"
		if allow {
			decision = "accept"
		}
		payload = map[string]any{"decision": decision}
	case "execCommandApproval", "applyPatchApproval":
		var decision any = "approved"
		if !allow {
			reason := result.Message
			if reason == "" {
				reason = "The user denied this."
			}
			decision = map[string]any{"denied": map[string]any{"rejection": reason}}
		}
		payload = map[string]any{"decision": decision}
	case "item/permissions/requestApproval":
		granted := map[string]any{}
		if allow {
			if p, ok := req.params["permissions"].(map[string]any); ok {
				granted = p
			}
		}
		payload = map[string]any{"permissions": granted, "scope": "turn"}
	case "item/tool/requestUserInput":
		answers := map[string]any{}
		if allow {
			given, _ := result.UpdatedInput["answers"].(map[string]any)
			texts := make([]string, 0, len(given))
			for text := range given {
				texts = append(texts, text)
			}
			sort.Strings(texts)
			for _, text := range texts {
				if id := req.questions[text]; id != "" {
					answer, _ := given[text].(string)
					answers[id] = map[string]any{"answers": []string{answer}}
				}
			}
		}
		payload = map[string]any{"answers": answers}
	default:
		return fmt.Errorf("unexpected codex request %s", req.method)
	}
	ctx, cancel := context.WithTimeout(s.ctx, rpcCallTimeout)
	defer cancel()
	return s.proc.rpc.respond(ctx, req.id, payload)
}
