package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Config starts one ACP session.
type Config struct {
	Agent    string   // agent type for logs and mode names ("kimi", "antigravity")
	Command  string   // CLI path
	Args     []string // arguments that start the ACP server (e.g. "acp")
	Env      []string // the process environment
	WorkDir  string
	ResumeID string // agent session to continue; "" starts a new one
	Mode     string // canonical permission mode; "" keeps the agent's default
	Model    string // value of the agent's model option; "" keeps its default
	Effort   string // value of the agent's thinking-level option; "" keeps its default
	// ExtraDirs are more directories the session may use (when the agent
	// supports additionalDirectories).
	ExtraDirs []string
}

// Session is a running `<cli> acp` for one chat. It implements
// core.AgentSession plus core.Interrupter and core.ContextReporter; see
// SteeringSession for agents that take messages during a turn.
type Session struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	stdin  io.Closer
	exited chan struct{} // closed when the process has exited
	rpc    *rpcClient
	alive  atomic.Bool
	// replaying: session/load replays the history as updates; those are
	// not new output.
	replaying atomic.Bool

	eventsMu     sync.Mutex
	events       chan core.Event
	eventsClosed bool
	closeOnce    sync.Once
	prompts      sync.WaitGroup // finishPrompt goroutines
	order        sync.Mutex     // orders prompt and cancel writes

	mu           sync.Mutex
	sid          string
	modes        []modeInfo
	currentMode  string
	modelOpt     *configOption
	effortOpt    *configOption
	models       []string // session/set_model ids (agy's models block)
	autoApprove  bool     // bypass on an agent without a bypass mode
	inflight     int      // session/prompt calls running
	interrupting bool
	compacting   bool // the turn is a /compact
	// compactReturned: the /compact prompt returned and the CLI is still
	// compacting; compactSignal is closed when it reports.
	compactReturned bool
	compactSignal   chan struct{}
	// compactReported: the CLI already reported the compaction (it can come
	// before the /compact prompt's answer is handled).
	compactReported bool
	ended           bool            // the process ended: no new prompts (guards prompts.Add)
	steerErrs       []string        // steered prompts that failed during the turn
	lastActivity    time.Time       // last activity event of the turn
	text            strings.Builder // the turn's answer so far
	thought         strings.Builder
	requests        map[string]*pendingRequest
	usage           *core.ContextReport
}

type modeInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type configOption struct {
	ID           string `json:"id"`
	Category     string `json:"category"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value string `json:"value"`
		Name  string `json:"name"`
	} `json:"options"`
}

func (o *configOption) has(value string) bool {
	for _, v := range o.Options {
		if v.Value == value {
			return true
		}
	}
	return false
}

type pendingRequest struct {
	id          json.RawMessage
	options     []permissionOption
	elicitation *elicitation
	done        chan struct{}
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
}

var (
	_ core.AgentSession    = (*Session)(nil)
	_ core.Interrupter     = (*Session)(nil)
	_ core.ContextReporter = (*Session)(nil)
	_ core.NativeCompactor = (*Session)(nil)
)

// activityEvery spaces the activity events sent while text streams.
var activityEvery = 20 * time.Second

// startTimeout bounds starting the process and the handshake.
var startTimeout = 60 * time.Second

// Start runs the CLI's ACP server and opens (or resumes) the session with
// the configured settings. A mode that cannot be applied fails the start:
// the session must not run more permissive than asked.
func Start(ctx context.Context, cfg Config) (*Session, error) {
	sctx, cancel := context.WithCancel(ctx)
	s := &Session{cfg: cfg, ctx: sctx, cancel: cancel, events: make(chan core.Event, 256), requests: map[string]*pendingRequest{}}
	cmd := exec.CommandContext(sctx, cfg.Command, cfg.Args...)
	cmd.Dir = cfg.WorkDir
	cmd.Env = cfg.Env
	core.UseProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("acp: start %s: %w", cfg.Agent, err)
	}
	s.cmd = cmd
	s.stdin = stdin
	s.exited = make(chan struct{})
	s.rpc = newRPCClient(stdin)
	s.rpc.onNotify = s.onNotification
	s.rpc.onRequest = s.onRequest
	s.rpc.onBroken = func() { _ = s.Close() }
	s.alive.Store(true)
	go func() {
		s.rpc.readLoop(stdout)
		_ = cmd.Wait()
		close(s.exited)
		s.processEnded()
	}()

	hctx, hcancel := context.WithTimeout(sctx, startTimeout)
	defer hcancel()
	if err := s.handshake(hctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.apply(hctx, cfg.Model, cfg.Effort, cfg.Mode); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

type sessionSetup struct {
	SessionID string `json:"sessionId"`
	Modes     *struct {
		CurrentModeID  string     `json:"currentModeId"`
		AvailableModes []modeInfo `json:"availableModes"`
	} `json:"modes"`
	Models *struct {
		AvailableModels []struct {
			ModelID string `json:"modelId"`
		} `json:"availableModels"`
	} `json:"models"`
	ConfigOptions []configOption `json:"configOptions"`
}

func (s *Session) handshake(ctx context.Context) error {
	raw, err := s.rpc.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
			// Questions arrive as forms (elicitation/create) and become the
			// chat's question cards.
			"elicitation": map[string]any{"form": map[string]any{}},
		},
		"clientInfo": map[string]any{"name": "echo-client", "version": "1"},
	}, true)
	if err != nil {
		return err
	}
	var init struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				Resume                json.RawMessage `json:"resume"`
				AdditionalDirectories json.RawMessage `json:"additionalDirectories"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &init)
	cwd := s.cfg.WorkDir
	params := func(extra map[string]any) map[string]any {
		p := map[string]any{"cwd": cwd, "mcpServers": []any{}}
		if len(s.cfg.ExtraDirs) > 0 && supported(init.AgentCapabilities.SessionCapabilities.AdditionalDirectories) {
			p["additionalDirectories"] = s.cfg.ExtraDirs
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	var setup sessionSetup
	if id := strings.TrimSpace(s.cfg.ResumeID); id != "" {
		method := ""
		switch {
		case supported(init.AgentCapabilities.SessionCapabilities.Resume):
			method = "session/resume"
		case init.AgentCapabilities.LoadSession:
			method = "session/load"
			s.replaying.Store(true)
		default:
			return fmt.Errorf("acp: %s cannot resume sessions", s.cfg.Agent)
		}
		raw, err = s.rpc.call(ctx, method, params(map[string]any{"sessionId": id}), true)
		s.replaying.Store(false)
		if err != nil {
			if isNotFound(err) {
				return fmt.Errorf("%w: %v", core.ErrAgentSessionNotFound, err)
			}
			return err
		}
		_ = json.Unmarshal(raw, &setup)
		if setup.SessionID == "" {
			setup.SessionID = id
		}
	} else {
		raw, err = s.rpc.call(ctx, "session/new", params(nil), true)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &setup); err != nil || setup.SessionID == "" {
			return fmt.Errorf("acp: session/new returned no session id")
		}
	}
	s.mu.Lock()
	s.sid = setup.SessionID
	if setup.Modes != nil {
		s.modes = setup.Modes.AvailableModes
		s.currentMode = setup.Modes.CurrentModeID
	}
	if setup.Models != nil {
		for _, m := range setup.Models.AvailableModels {
			s.models = append(s.models, m.ModelID)
		}
	}
	s.absorbOptionsLocked(setup.ConfigOptions)
	s.mu.Unlock()
	return nil
}

// supported reports an advertised capability: present and not false/null.
func supported(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "null" && v != "false"
}

func isNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such session") || strings.Contains(msg, "does not exist")
}

func (s *Session) absorbOptionsLocked(options []configOption) {
	for i := range options {
		o := options[i]
		switch {
		case o.Category == "model" || o.ID == "model":
			s.modelOpt = &o
		case o.Category == "thought_level" || o.ID == "thinking" || o.ID == "thought_level":
			s.effortOpt = &o
		case o.Category == "mode" || o.ID == "mode":
			if len(s.modes) == 0 {
				for _, v := range o.Options {
					s.modes = append(s.modes, modeInfo{ID: v.Value, Name: v.Name})
				}
			}
			if o.CurrentValue != "" {
				s.currentMode = o.CurrentValue
			}
		}
	}
}

// modeCandidates are ACP mode ids for each canonical mode, best first.
var modeCandidates = map[string][]string{
	core.PermissionModePlan:        {"plan"},
	core.PermissionModeDefault:     {"default"},
	core.PermissionModeAcceptEdits: {"accept-edits", "acceptEdits", "accept_edits"},
	core.PermissionModeAuto:        {"auto"},
	core.PermissionModeBypass:      {"yolo", "bypassPermissions", "bypass"},
}

// bypassFallback: without a native bypass mode, the session uses the most
// permissive of these and approves the remaining requests itself.
var bypassFallback = []string{"auto", "accept-edits", "acceptEdits", "default"}

// resolveMode maps a canonical mode to one of the agent's mode ids.
func (s *Session) resolveModeLocked(mode string) (id string, autoApprove bool, ok bool) {
	has := func(id string) bool {
		for _, m := range s.modes {
			if strings.EqualFold(m.ID, id) {
				return true
			}
		}
		return false
	}
	_, canonical, known := core.ResolvePermissionMode(s.cfg.Agent, mode)
	if !known {
		canonical = mode
	}
	for _, c := range modeCandidates[canonical] {
		if has(c) {
			return c, false, true
		}
	}
	if canonical == core.PermissionModeBypass {
		for _, c := range bypassFallback {
			if has(c) {
				return c, true, true
			}
		}
	}
	return "", false, false
}

// apply sets model, thinking level and mode (empty values are left alone).
// A mode it cannot set is an error; a model or level the agent does not
// list is refused too, rather than silently ignored.
func (s *Session) apply(ctx context.Context, model, effort, mode string) error {
	s.mu.Lock()
	sid := s.sid
	modelOpt := s.modelOpt
	models := append([]string(nil), s.models...)
	var modeID string
	var autoApprove, setMode bool
	if mode != "" {
		var ok bool
		modeID, autoApprove, ok = s.resolveModeLocked(mode)
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("acp: %s has no mode for %q", s.cfg.Agent, mode)
		}
		setMode = !strings.EqualFold(modeID, s.currentMode) || autoApprove != s.autoApprove
	}
	s.mu.Unlock()

	if setMode {
		// Leaving a client-approved bypass: stop approving before the mode
		// changes, so no request slips through in between.
		if !autoApprove {
			s.mu.Lock()
			s.autoApprove = false
			s.mu.Unlock()
		}
		if _, err := s.rpc.call(ctx, "session/set_mode", map[string]any{"sessionId": sid, "modeId": modeID}, true); err != nil {
			return fmt.Errorf("acp: set mode %s: %w", modeID, err)
		}
		s.mu.Lock()
		s.currentMode, s.autoApprove = modeID, autoApprove
		s.mu.Unlock()
	}
	if model != "" {
		switch {
		case modelOpt != nil && modelOpt.has(model):
			if err := s.setOption(ctx, sid, modelOpt.ID, model); err != nil {
				return err
			}
		case containsString(models, model):
			if _, err := s.rpc.call(ctx, "session/set_model", map[string]any{"sessionId": sid, "modelId": model}, true); err != nil {
				return fmt.Errorf("acp: set model %s: %w", model, err)
			}
		default:
			return fmt.Errorf("acp: %s does not offer model %q", s.cfg.Agent, model)
		}
	}
	if effort != "" {
		// The levels can depend on the model just set: read them now.
		s.mu.Lock()
		effortOpt := s.effortOpt
		s.mu.Unlock()
		if effortOpt == nil || !effortOpt.has(effort) {
			return fmt.Errorf("acp: %s does not offer thinking level %q", s.cfg.Agent, effort)
		}
		if err := s.setOption(ctx, sid, effortOpt.ID, effort); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) setOption(ctx context.Context, sid, id, value string) error {
	raw, err := s.rpc.call(ctx, "session/set_config_option", map[string]any{"sessionId": sid, "configId": id, "value": value}, true)
	if err != nil {
		return fmt.Errorf("acp: set %s %s: %w", id, value, err)
	}
	var resp struct {
		ConfigOptions []configOption `json:"configOptions"`
	}
	if json.Unmarshal(raw, &resp) == nil && len(resp.ConfigOptions) > 0 {
		s.mu.Lock()
		s.absorbOptionsLocked(resp.ConfigOptions)
		s.mu.Unlock()
	}
	return nil
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Apply changes settings of the running session between turns (the agent's
// LiveSettingsApplier).
func (s *Session) Apply(ctx context.Context, model, effort, mode string) error {
	if !s.Alive() {
		return errors.New("acp: session is closed")
	}
	return s.apply(ctx, model, effort, mode)
}

// ── turns ──────────────────────────────────────────────────────

func (s *Session) promptParams(prompt string, images []core.ImageAttachment, files []core.FileAttachment) map[string]any {
	if paths := core.SaveFilesToDisk(s.cfg.WorkDir, files); len(paths) > 0 {
		prompt = core.AppendFileRefs(prompt, paths)
	}
	blocks := []any{map[string]any{"type": "text", "text": core.WrapInteractionOptions(prompt)}}
	for _, img := range images {
		mime := img.MimeType
		if mime == "" {
			mime = "image/png"
		}
		blocks = append(blocks, map[string]any{"type": "image", "mimeType": mime, "data": base64.StdEncoding.EncodeToString(img.Data)})
	}
	return map[string]any{"sessionId": s.CurrentSessionID(), "prompt": blocks}
}

// Send starts a turn. The prompt is written before Send returns (so a
// later /stop cannot overtake it); the output and the turn's end
// (EventResult) come as events.
func (s *Session) Send(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	if !s.Alive() {
		return errors.New("acp: session is closed")
	}
	return s.startPrompt(s.ctx, s.promptParams(prompt, images, files), true, false)
}

// Compact implements core.NativeCompactor: the CLI's own /compact command,
// sent as it is (a wrapped prompt would not be a command). Kimi finishes
// compacting after the prompt returns; the turn ends when it reports so.
func (s *Session) Compact(ctx context.Context) error {
	if !s.Alive() {
		return errors.New("acp: session is closed")
	}
	params := map[string]any{"sessionId": s.CurrentSessionID(), "prompt": []any{map[string]any{"type": "text", "text": "/compact"}}}
	return s.startPrompt(ctx, params, true, true)
}

// SteeringSession is a Session of an agent that takes a prompt sent during
// a turn as part of it (core.Steerer). Agents that refuse such prompts
// (Kimi) use the plain Session, so the engine queues instead.
type SteeringSession struct{ *Session }

var _ core.Steerer = SteeringSession{}

// Steer sends a message into the running turn.
func (s SteeringSession) Steer(prompt string, images []core.ImageAttachment, files []core.FileAttachment) error {
	return s.startPrompt(s.ctx, s.promptParams(prompt, images, files), false, false)
}

// startPrompt writes a session/prompt and waits for its answer in the
// background. order keeps prompts and cancellations in the order they were
// asked for.
func (s *Session) startPrompt(ctx context.Context, params map[string]any, newTurn, compact bool) error {
	s.order.Lock()
	defer s.order.Unlock()
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return errClosed
	}
	if newTurn {
		if s.inflight > 0 {
			s.mu.Unlock()
			return errors.New("acp: a turn is already running")
		}
		s.interrupting = false
		s.compacting = compact
		s.compactReported = false
		s.steerErrs = nil
		s.text.Reset()
		s.thought.Reset()
	} else if s.inflight == 0 || s.compacting {
		s.mu.Unlock()
		return errors.New("no turn is running")
	}
	s.inflight++
	s.prompts.Add(1)
	s.mu.Unlock()
	wait, err := s.rpc.start(ctx, "session/prompt", params)
	if err != nil {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
		s.prompts.Done()
		return err
	}
	go s.finishPrompt(wait)
	return nil
}

// compactWait bounds waiting for a compaction the CLI finishes after its
// /compact prompt returned.
var compactWait = 2 * time.Minute

// compactsInBackground reports whether a /compact prompt's reply means the
// compaction goes on after the prompt returned: no reply yet, or one that
// says it runs in the background.
func compactsInBackground(reply string) bool {
	reply = strings.ToLower(strings.TrimSpace(reply))
	return reply == "" || strings.Contains(reply, "in the background")
}

// finishPrompt waits for one session/prompt. The turn ends (one
// EventResult or EventError) when the last prompt in flight returns: a
// steered prompt continues the turn.
func (s *Session) finishPrompt(wait func(context.Context) (json.RawMessage, error)) {
	defer s.prompts.Done()
	raw, err := wait(s.ctx)
	var resp struct {
		StopReason string `json:"stopReason"`
	}
	if err == nil {
		_ = json.Unmarshal(raw, &resp)
	}
	s.mu.Lock()
	if s.inflight == 1 && s.compacting && err == nil && resp.StopReason != "cancelled" && !s.compactReported && compactsInBackground(s.text.String()) {
		// The CLI compacts in the background and reports when done (Kimi
		// Code: 2.5 says nothing until then, 2.6 says it started). A prompt
		// sent meanwhile would be dropped, so the turn waits.
		s.compactReturned = true
		signal := make(chan struct{})
		s.compactSignal = signal
		s.mu.Unlock()
		select {
		case <-signal:
		case <-time.After(compactWait):
		case <-s.ctx.Done():
		}
		s.mu.Lock()
		s.compactSignal = nil
	}
	s.inflight--
	failed := err != nil || resp.StopReason == "refusal"
	if s.inflight > 0 {
		// An earlier prompt that a steered one replaced, or a steered prompt
		// that failed while the turn goes on: report it when the turn ends.
		if failed && !s.interrupting {
			reason := "refused"
			if err != nil {
				reason = err.Error()
			}
			s.steerErrs = append(s.steerErrs, reason)
		}
		s.mu.Unlock()
		return
	}
	interrupted := s.interrupting || resp.StopReason == "cancelled"
	s.interrupting = false
	s.compacting = false
	s.compactReturned = false
	text, thought := s.text.String(), s.thought.String()
	s.text.Reset()
	s.thought.Reset()
	steerErrs := s.steerErrs
	s.steerErrs = nil
	// Requests still open belong to the turn that ended: retire them.
	pending := s.requests
	s.requests = map[string]*pendingRequest{}
	s.mu.Unlock()
	for _, req := range pending {
		_ = s.rpc.respond(s.ctx, req.id, cancelledOutcome(req))
		close(req.done)
	}

	if thought != "" {
		s.emit(core.Event{Type: core.EventThinking, Content: thought})
	}
	if text != "" {
		s.emit(core.Event{Type: core.EventText, Content: text})
	}
	if len(steerErrs) > 0 && !interrupted {
		s.emit(core.Event{Type: core.EventText, Content: "⚠️ A message sent during this turn was not taken: " + strings.Join(steerErrs, "; ")})
	}
	switch {
	case err != nil && !interrupted:
		s.emit(core.Event{Type: core.EventError, Error: err})
	case resp.StopReason == "refusal":
		s.emit(core.Event{Type: core.EventError, Error: errors.New("the agent refused the request")})
	default:
		s.emit(core.Event{Type: core.EventResult, SessionID: s.CurrentSessionID(), Done: true, Interrupted: interrupted})
	}
}

// Interrupt implements core.Interrupter (session/cancel; the turn ends with
// stopReason "cancelled"). Requests waiting for an answer are cancelled
// too. Every write is bounded by ctx.
func (s *Session) Interrupt(ctx context.Context) error {
	s.order.Lock()
	defer s.order.Unlock()
	s.mu.Lock()
	if s.inflight == 0 {
		s.mu.Unlock()
		return errors.New("no turn is running")
	}
	s.interrupting = true
	pending := s.requests
	s.requests = map[string]*pendingRequest{}
	sid := s.sid
	if s.compactSignal != nil {
		close(s.compactSignal)
		s.compactSignal = nil
	}
	s.mu.Unlock()
	for _, req := range pending {
		_ = s.rpc.respond(ctx, req.id, cancelledOutcome(req))
		close(req.done)
	}
	return s.rpc.notify(ctx, "session/cancel", map[string]any{"sessionId": sid})
}

func cancelledOutcome(req *pendingRequest) any {
	if req.elicitation != nil {
		return map[string]any{"action": "cancel"}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

// ContextUsage implements core.ContextReporter from the agent's last
// usage_update.
func (s *Session) ContextUsage(context.Context) (*core.ContextReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usage == nil {
		return nil, errors.New("no context usage reported yet")
	}
	r := *s.usage
	return &r, nil
}

// ── updates ────────────────────────────────────────────────────

func (s *Session) onNotification(method string, params json.RawMessage) {
	if method != "session/update" || s.replaying.Load() {
		return
	}
	var wrap struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &wrap) != nil {
		return
	}
	if wrap.SessionID != "" && wrap.SessionID != s.CurrentSessionID() {
		return // another session on the same connection
	}
	var u struct {
		Kind    string `json:"sessionUpdate"`
		Content json.RawMessage
		// tool_call / tool_call_update
		ToolCallID string          `json:"toolCallId"`
		Title      string          `json:"title"`
		ToolKind   string          `json:"kind"`
		Status     string          `json:"status"`
		RawInput   json.RawMessage `json:"rawInput"`
		// plan
		Entries []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"entries"`
		// usage_update
		Used int `json:"used"`
		Size int `json:"size"`
		// current_mode_update
		CurrentModeID string         `json:"currentModeId"`
		ConfigOptions []configOption `json:"configOptions"`
	}
	if json.Unmarshal(wrap.Update, &u) != nil {
		return
	}
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		text := contentText(u.Content)
		if text == "" {
			return
		}
		s.mu.Lock()
		inTurn := s.inflight > 0
		if inTurn {
			if u.Kind == "agent_message_chunk" {
				s.text.WriteString(text)
			} else {
				s.thought.WriteString(text)
			}
		}
		if s.compacting && inTurn && u.Kind == "agent_message_chunk" && !compactsInBackground(text) {
			s.compactReported = true // a report, not the "started" notice
		}
		if s.compactReturned && s.compactSignal != nil && u.Kind == "agent_message_chunk" {
			close(s.compactSignal) // the background compaction reported
			s.compactSignal = nil
		}
		// Text is sent when the turn ends; meanwhile an empty event shows
		// the engine the turn is alive (its first-event and idle timers).
		active := inTurn && time.Since(s.lastActivity) > activityEvery
		if active {
			s.lastActivity = time.Now()
		}
		s.mu.Unlock()
		if active {
			s.emit(core.Event{Type: core.EventThinking})
		}
		if !inTurn {
			slog.Debug("acp: output outside a turn", "agent", s.cfg.Agent, "kind", u.Kind)
		}
	case "tool_call":
		s.flushInterim()
		name := u.Title
		if name == "" {
			name = u.ToolKind
		}
		s.emit(core.Event{Type: core.EventToolUse, ToolName: name, ToolInput: summarizeInput(u.ToolKind, u.RawInput, u.Title)})
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return
		}
		result := toolContentText(u.Content)
		if result == "" {
			result = u.Status
		}
		s.emit(core.Event{Type: core.EventToolResult, ToolName: u.Title, Content: truncate(result, 800)})
	case "plan":
		var b strings.Builder
		for _, e := range u.Entries {
			mark := "☐"
			switch e.Status {
			case "completed":
				mark = "☑"
			case "in_progress":
				mark = "▸"
			}
			fmt.Fprintf(&b, "%s %s\n", mark, e.Content)
		}
		if b.Len() > 0 {
			s.emit(core.Event{Type: core.EventThinking, Content: strings.TrimSpace(b.String())})
		}
	case "usage_update":
		if u.Size > 0 {
			s.mu.Lock()
			s.usage = &core.ContextReport{UsedTokens: u.Used, MaxTokens: u.Size, Percent: u.Used * 100 / u.Size}
			s.mu.Unlock()
		}
	case "current_mode_update":
		if u.CurrentModeID != "" {
			s.mu.Lock()
			s.currentMode = u.CurrentModeID
			s.mu.Unlock()
		}
	case "config_option_update":
		s.mu.Lock()
		s.absorbOptionsLocked(u.ConfigOptions)
		s.mu.Unlock()
	}
}

// flushInterim reports text written before a tool call as thinking (it is
// commentary, not the answer).
func (s *Session) flushInterim() {
	s.mu.Lock()
	text := strings.TrimSpace(s.thought.String() + "\n" + s.text.String())
	s.thought.Reset()
	s.text.Reset()
	s.mu.Unlock()
	if text != "" {
		s.emit(core.Event{Type: core.EventThinking, Content: text})
	}
}

func contentText(raw json.RawMessage) string {
	var c struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &c) == nil {
		return c.Text
	}
	return ""
}

func toolContentText(raw json.RawMessage) string {
	var items []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Path    string          `json:"path"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		switch it.Type {
		case "content":
			if t := contentText(it.Content); t != "" {
				parts = append(parts, t)
			}
		case "diff":
			parts = append(parts, "edited "+it.Path)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// summarizeInput is a readable form of a tool's input.
func summarizeInput(kind string, raw json.RawMessage, title string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || len(m) == 0 {
		return title
	}
	for _, key := range []string{"command", "cmd", "file_path", "path", "pattern", "query", "url"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	data, _ := json.Marshal(m)
	return truncate(string(data), 300)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ── requests from the agent ────────────────────────────────────

func (s *Session) onRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "session/request_permission":
		s.handlePermission(id, params)
	case "elicitation/create":
		s.handleElicitation(id, params)
	default:
		s.rpc.respondError(id, -32601, "not supported by echo-client")
	}
}

func (s *Session) handlePermission(id json.RawMessage, params json.RawMessage) {
	var p struct {
		ToolCall struct {
			Title    string          `json:"title"`
			Kind     string          `json:"kind"`
			RawInput json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []permissionOption `json:"options"`
	}
	if json.Unmarshal(params, &p) != nil {
		s.rpc.respondError(id, -32602, "invalid params")
		return
	}
	req := &pendingRequest{id: id, options: p.Options, done: make(chan struct{})}
	key := strings.TrimSpace(string(id))
	s.mu.Lock()
	auto := s.autoApprove
	if !auto {
		s.requests[key] = req
	}
	s.mu.Unlock()
	if auto {
		// Bypass on an agent without a bypass mode: approve here.
		if opt := pickOptionFrom(p.Options, true, true); opt != "" {
			_ = s.rpc.respond(s.ctx, id, selectedOutcome(opt))
		} else {
			s.rpc.respondError(id, -32603, "no option to approve")
		}
		return
	}
	name := p.ToolCall.Title
	if name == "" {
		name = p.ToolCall.Kind
	}
	if name == "" {
		name = "permission"
	}
	var rawInput map[string]any
	_ = json.Unmarshal(p.ToolCall.RawInput, &rawInput)
	s.emit(core.Event{Type: core.EventPermissionRequest, RequestID: key, ToolName: name,
		ToolInput: summarizeInput(p.ToolCall.Kind, p.ToolCall.RawInput, p.ToolCall.Title), ToolInputRaw: rawInput})
}

// pickOption chooses the agent's reject or allow-once option. A user's
// "allow" never becomes a standing grant (allow_always); only the client's
// own bypass (standing) may use it when nothing else is offered.
func pickOption(options []permissionOption, allow bool) string {
	return pickOptionFrom(options, allow, false)
}

func pickOptionFrom(options []permissionOption, allow, standing bool) string {
	order := []string{"reject_once", "reject_always"}
	if allow {
		order = []string{"allow_once"}
		if standing {
			order = append(order, "allow_always")
		}
	}
	for _, kind := range order {
		for _, o := range options {
			if o.Kind == kind {
				return o.OptionID
			}
		}
	}
	return ""
}

func selectedOutcome(optionID string) any {
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}}
}

// RespondPermission answers a permission request or a question form.
func (s *Session) RespondPermission(requestID string, result core.PermissionResult) error {
	s.mu.Lock()
	req := s.requests[requestID]
	delete(s.requests, requestID)
	s.mu.Unlock()
	if req == nil {
		return fmt.Errorf("acp: no pending request %s", requestID)
	}
	defer close(req.done)
	allow := strings.EqualFold(result.Behavior, "allow")
	if req.elicitation != nil {
		return s.rpc.respond(s.ctx, req.id, req.elicitation.response(allow, result.UpdatedInput))
	}
	if opt := pickOption(req.options, allow); opt != "" {
		return s.rpc.respond(s.ctx, req.id, selectedOutcome(opt))
	}
	// No matching option: the request is answered as cancelled.
	return s.rpc.respond(s.ctx, req.id, cancelledOutcome(req))
}

// RequestDone implements core.PendingRequestWatcher: closed when the
// request was answered or cancelled (e.g. by /stop).
func (s *Session) RequestDone(requestID string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req := s.requests[requestID]; req != nil {
		return req.done
	}
	done := make(chan struct{})
	close(done)
	return done
}

// ── lifecycle ──────────────────────────────────────────────────

func (s *Session) emit(ev core.Event) {
	if ev.SessionID == "" {
		ev.SessionID = s.CurrentSessionID()
	}
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

// processEnded runs when the process exits: waiting requests are released,
// a turn in progress ends with an error (its session/prompt call fails),
// and then the event stream closes.
func (s *Session) processEnded() {
	s.alive.Store(false)
	s.mu.Lock()
	s.ended = true // after this no prompts.Add can race the Wait below
	pending := s.requests
	s.requests = map[string]*pendingRequest{}
	s.mu.Unlock()
	for _, req := range pending {
		close(req.done)
	}
	done := make(chan struct{})
	go func() { s.prompts.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	s.closeEvents()
}

func (s *Session) closeEvents() {
	s.closeOnce.Do(func() {
		s.cancel() // unblocks emit
		s.eventsMu.Lock()
		s.eventsClosed = true
		close(s.events)
		s.eventsMu.Unlock()
	})
}

func (s *Session) Events() <-chan core.Event { return s.events }

func (s *Session) CurrentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sid
}

func (s *Session) Alive() bool { return s.alive.Load() }

// closeGrace is how long Close lets an idle CLI exit on its own once its
// input ends: it records the last turn in its session store on the way
// out, and a process killed right after a turn can lose that turn (Kimi
// Code 2.5 then resumes with the prompt but not the reply).
var closeGrace = 5 * time.Second

// Close stops the agent process: an idle one is asked to exit first (end
// of input), a busy one is killed at once.
func (s *Session) Close() error {
	s.alive.Store(false)
	s.mu.Lock()
	idle := s.inflight == 0 && !s.ended
	s.ended = true // no prompt starts once Close decided
	s.mu.Unlock()
	if idle && s.stdin != nil && s.exited != nil {
		_ = s.stdin.Close()
		select {
		case <-s.exited:
		case <-time.After(closeGrace):
			slog.Debug("acp: the agent did not exit when its input ended; stopping it", "agent", s.cfg.Agent)
		}
	}
	s.cancel()
	if s.cmd != nil && s.cmd.Process != nil {
		_ = core.KillProcessGroup(s.cmd)
	}
	select {
	case <-s.rpc.done:
	case <-time.After(5 * time.Second):
	}
	s.closeEvents()
	return nil
}

// Modes lists the agent's mode ids (sorted) as seen in this session.
func (s *Session) Modes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.modes))
	for _, m := range s.modes {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}
