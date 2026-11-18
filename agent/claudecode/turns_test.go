package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// scriptedClaude runs readLoop over output the test writes, as Claude
// would print it with --replay-user-messages.
type scriptedClaude struct {
	t   *testing.T
	cs  *claudeSession
	out *io.PipeWriter
	in  *lockedBuffer
	bg  chan string
}

// lastUUID is the uuid of the last user message written to Claude.
func (s *scriptedClaude) lastUUID() string {
	s.t.Helper()
	lines := strings.Split(strings.TrimSpace(s.in.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) == nil && m["type"] == "user" {
			id, _ := m["uuid"].(string)
			return id
		}
	}
	s.t.Fatal("no user message written")
	return ""
}

func echoID(uuid, text string) map[string]any {
	return map[string]any{"type": "user", "isReplay": true, "uuid": uuid, "message": map[string]any{"role": "user", "content": text}}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) Close() error { return nil }
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newScriptedClaude(t *testing.T) *scriptedClaude {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a no-op process")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, w := io.Pipe()
	in := &lockedBuffer{}
	cs := &claudeSession{cmd: cmd, stdin: in, events: make(chan core.Event, 64), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	cs.alive.Store(true)
	s := &scriptedClaude{t: t, cs: cs, out: w, in: in, bg: make(chan string, 8)}
	cs.OnBackgroundOutput(func(out core.BackgroundOutput) { s.bg <- out.Text })
	go cs.readLoop(r, &bytes.Buffer{})
	t.Cleanup(func() {
		_ = w.Close()
		<-cs.done
		cancel()
	})
	return s
}

func (s *scriptedClaude) emit(lines ...map[string]any) {
	s.t.Helper()
	for _, l := range lines {
		data, _ := json.Marshal(l)
		if _, err := s.out.Write(append(data, '\n')); err != nil {
			s.t.Fatal(err)
		}
	}
}

func echo(text string) map[string]any {
	return map[string]any{"type": "user", "isReplay": true, "message": map[string]any{"role": "user", "content": text}}
}
func say(text string) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
}
func result(text string) map[string]any {
	return map[string]any{"type": "result", "subtype": "success", "result": text, "session_id": "s1"}
}
func taskDone() map[string]any {
	return map[string]any{"type": "system", "subtype": "task_notification", "task_id": "bg1", "status": "completed", "output_file": "/tmp/bg1.output"}
}

// nextResult waits for the next EventResult (other events are skipped).
func (s *scriptedClaude) nextResult() core.Event {
	s.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.cs.events:
			if ev.Type == core.EventResult || ev.Type == core.EventError {
				return ev
			}
		case <-timeout:
			s.t.Fatal("no result")
		}
	}
}

func (s *scriptedClaude) background() string {
	s.t.Helper()
	select {
	case text := <-s.bg:
		return text
	case <-time.After(5 * time.Second):
		s.t.Fatal("no background output")
		return ""
	}
}

// noResultQueued checks that no result is waiting on the event stream.
func (s *scriptedClaude) noResultQueued() {
	s.t.Helper()
	for {
		select {
		case ev := <-s.cs.events:
			if ev.Type == core.EventResult {
				s.t.Fatalf("unexpected result on the event stream: %+v", ev)
			}
		default:
			return
		}
	}
}

// A background task finishes after our turn: Claude's own turn goes to the
// chat, not to the engine's next turn.
func TestTurnClaudeStartsAfterTaskNotification(t *testing.T) {
	s := newScriptedClaude(t)
	if err := s.cs.Send("start it", nil, nil); err != nil {
		t.Fatal(err)
	}
	s.emit(echo("start it"), say("STARTED"), result("STARTED"))
	if ev := s.nextResult(); ev.Content != "STARTED" {
		t.Fatalf("result = %+v", ev)
	}
	s.emit(taskDone(), say("The task finished."), result("The task finished."))
	if got := s.background(); got != "The task finished." {
		t.Fatalf("background = %q", got)
	}
	s.noResultQueued()
}

// Our prompt arrives while Claude's own turn runs: that turn's result is not
// the answer; ours comes after Claude takes the prompt.
func TestPromptQueuedBehindClaudesOwnTurn(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("start it", nil, nil)
	s.emit(echo("start it"), result("STARTED"))
	s.nextResult()
	s.emit(taskDone())
	_ = s.cs.Send("Reply USERMSG", nil, nil)
	s.emit(say("background done"), result("background done"))
	s.emit(echo("Reply USERMSG"), say("USERMSG"), result("USERMSG"))
	// The waiting turn reads the earlier output first, as its own event.
	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-s.cs.events:
			switch ev.Type {
			case core.EventBackground:
				got = append(got, "bg:"+ev.Content)
			case core.EventResult:
				got = append(got, "result:"+ev.Content)
			}
		case <-timeout:
			t.Fatalf("events = %v", got)
		}
	}
	if got[0] != "bg:background done" || got[1] != "result:USERMSG" {
		t.Fatalf("events = %v", got)
	}
}

// A steered message Claude takes after a text-only turn's result: its
// answer is a separate message, and the turn's own answer stands.
func TestSteerAnsweredAfterResult(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("count to three", nil, nil)
	s.emit(echo("count to three"), say("one two three"))
	if err := s.cs.Steer("also say BANANA", nil, nil); err != nil {
		t.Fatal(err)
	}
	s.emit(result("one two three"))
	if ev := s.nextResult(); ev.Content != "one two three" {
		t.Fatalf("result = %+v", ev)
	}
	s.emit(echo("also say BANANA"), say("BANANA"), result("BANANA"))
	if got := s.background(); got != "BANANA" {
		t.Fatalf("background = %q", got)
	}
	s.noResultQueued()
	// The next prompt is ours again.
	_ = s.cs.Send("next", nil, nil)
	s.emit(echo("next"), result("NEXT"))
	if ev := s.nextResult(); ev.Content != "NEXT" {
		t.Fatalf("result = %+v", ev)
	}
}

// A steered message taken during the turn (at a tool step) is part of it.
func TestSteerTakenDuringTurn(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("work", nil, nil)
	s.emit(echo("work"))
	_ = s.cs.Steer("and BANANA", nil, nil)
	s.emit(echo("and BANANA"), say("done BANANA"), result("done BANANA"))
	if ev := s.nextResult(); ev.Content != "done BANANA" {
		t.Fatalf("result = %+v", ev)
	}
	select {
	case text := <-s.bg:
		t.Fatalf("unexpected background output %q", text)
	default:
	}
}

// /compact echoes its output, not the command: still our turn.
func TestSlashCommandEchoOpensTurn(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("hello", nil, nil)
	s.emit(echo("hello"), result("hi"))
	s.nextResult()
	_ = s.cs.Send("/compact", nil, nil)
	s.emit(echo("<local-command-stdout>Compacted</local-command-stdout>"), echoID(s.lastUUID(), "<command-name>/compact</command-name>"),
		map[string]any{"type": "result", "subtype": "success", "result": "", "local_command": "compact", "session_id": "s1"})
	if ev := s.nextResult(); ev.Type != core.EventResult {
		t.Fatalf("result = %+v", ev)
	}
}

// A CLI that does not echo: every result ends the engine's turn, as before.
func TestNoEchoKeepsResultsInTurn(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("hello", nil, nil)
	s.emit(taskDone(), result("hi"))
	if ev := s.nextResult(); ev.Content != "hi" {
		t.Fatalf("result = %+v", ev)
	}
}

// A tool request in Claude's own turn is declined (no card can be shown).
func TestToolRequestInClaudesOwnTurnDeclined(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("start", nil, nil)
	s.emit(echo("start"), result("ok"))
	s.nextResult()
	s.emit(taskDone(), map[string]any{"type": "control_request", "request_id": "r1",
		"request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "rm -rf x"}}},
		result("could not run it"))
	s.background()
	if in := s.in.String(); !strings.Contains(in, `"request_id":"r1"`) || !strings.Contains(in, `"behavior":"deny"`) {
		t.Fatalf("no denial written: %s", in)
	}
	for {
		select {
		case ev := <-s.cs.events:
			if ev.Type == core.EventPermissionRequest {
				t.Fatalf("permission request reached the engine: %+v", ev)
			}
		default:
			return
		}
	}
}

// A failed write drops exactly its own record.
func TestUnsentRecordDropped(t *testing.T) {
	cs := &claudeSession{}
	a := cs.noteSent("u-a", "a", false)
	b := cs.noteSent("u-b", "b", true)
	cs.noteSent("u-c", "c", false)
	cs.noteUnsent(a)
	cs.noteUnsent(b)
	if len(cs.sentMsgs) != 1 || cs.sentMsgs[0].text != "c" {
		t.Fatalf("records = %+v", cs.sentMsgs)
	}
}

// A turn of Claude's own that ends in an error (e.g. stopped) posts only
// the text it wrote, not the error.
func TestFailedBackgroundTurnPostsItsTextOnly(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("start", nil, nil)
	s.emit(echo("start"), result("ok"))
	s.nextResult()
	s.emit(taskDone(), say("Checking the log"),
		map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": "Request interrupted", "session_id": "s1"})
	if got := s.background(); got != "Checking the log" {
		t.Fatalf("background = %q", got)
	}
}

// A steered message Claude dropped does not capture the echo of a later
// slash command (which echoes its output, not its text).
func TestDroppedSteerDoesNotCaptureSlashCommand(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("count", nil, nil)
	s.emit(echo("count"))
	_ = s.cs.Steer("also BANANA", nil, nil) // never taken
	s.emit(result("one two"))
	s.nextResult()
	_ = s.cs.Send("/compact", nil, nil)
	s.emit(echo("<local-command-stdout>Compacted</local-command-stdout>"), echoID(s.lastUUID(), "<command-name>/compact</command-name>"),
		map[string]any{"type": "result", "subtype": "success", "result": "", "local_command": "compact", "session_id": "s1"})
	if ev := s.nextResult(); ev.Type != core.EventResult {
		t.Fatalf("result = %+v", ev)
	}
	s.cs.trackMu.Lock()
	n := len(s.cs.sentMsgs)
	s.cs.trackMu.Unlock()
	if n != 0 {
		t.Fatalf("stale records left: %d", n)
	}
}

// /stop while our prompt waits behind a turn Claude started itself: the
// interrupt is refused so the engine stops the process.
func TestInterruptRefusedWhilePromptWaits(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("start", nil, nil)
	s.emit(echo("start"), result("ok"))
	s.nextResult()
	s.emit(taskDone())
	_ = s.cs.Send("next", nil, nil)
	deadline := time.Now().Add(2 * time.Second)
	for !s.cs.inBackgroundTurn() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.cs.Interrupt(ctx); err == nil {
		t.Fatal("interrupt accepted while the prompt has not started")
	}
}

// System messages repeat the session id; the engine hears it once.
func TestSystemSessionIDSentOnce(t *testing.T) {
	s := newScriptedClaude(t)
	init := map[string]any{"type": "system", "subtype": "init", "session_id": "s1"}
	s.emit(init, init, map[string]any{"type": "system", "subtype": "status", "session_id": "s1"})
	_ = s.cs.Send("x", nil, nil)
	s.emit(echo("x"), result("done"))
	got := 0
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev := <-s.cs.events:
			if ev.Type == core.EventText && ev.SessionID == "s1" && ev.Content == "" {
				got++
			}
			if ev.Type == core.EventResult {
				if got != 1 {
					t.Fatalf("session id events = %d, want 1", got)
				}
				return
			}
		case <-timeout:
			t.Fatal("no result")
		}
	}
}

// A background task finishing during our turn is taken into it (echoed):
// no turn of Claude's own follows, and the next result is ours.
func TestNotificationTakenIntoOurTurn(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("work", nil, nil)
	s.emit(echo("work"),
		map[string]any{"type": "system", "subtype": "task_started", "task_id": "bg1", "is_backgrounded": true},
		taskDone(), echo("<task-notification>\n<task-id>bg1</task-id>"), say("FGDONE BGDONE"), result("FGDONE BGDONE"))
	if ev := s.nextResult(); ev.Content != "FGDONE BGDONE" {
		t.Fatalf("result = %+v", ev)
	}
	_ = s.cs.Send("next", nil, nil)
	s.emit(echo("next"), result("NEXT"))
	if ev := s.nextResult(); ev.Content != "NEXT" {
		t.Fatalf("result = %+v", ev)
	}
}

// A background task finishing during a turn that cannot take it in (here
// /compact) gets a turn of Claude's own after ours: background, even if
// our next prompt is already written.
func TestNotificationDuringCompactRunsAfterIt(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("start", nil, nil)
	s.emit(echo("start"), map[string]any{"type": "system", "subtype": "task_started", "task_id": "bg1", "is_backgrounded": true}, result("STARTED"))
	s.nextResult()
	_ = s.cs.Send("/compact", nil, nil)
	s.emit(echo("<local-command-stdout>Compacted</local-command-stdout>"), taskDone(), echoID(s.lastUUID(), "<command-name>/compact</command-name>"),
		map[string]any{"type": "result", "subtype": "success", "result": "", "local_command": "compact", "session_id": "s1"})
	s.nextResult()
	_ = s.cs.Send("Reply AFTERCOMPACT", nil, nil)
	s.emit(say("SLEPT"), result("SLEPT"), echo("Reply AFTERCOMPACT"), result("AFTERCOMPACT"))
	var got []string
	timeout := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-s.cs.events:
			if ev.Type == core.EventBackground || ev.Type == core.EventResult {
				got = append(got, string(ev.Type)+":"+ev.Content)
			}
		case <-timeout:
			t.Fatalf("events = %v", got)
		}
	}
	if got[0] != "background:SLEPT" || got[1] != "result:AFTERCOMPACT" {
		t.Fatalf("events = %v", got)
	}
}

// A foreground command's completion is its tool result: no turn follows.
func TestForegroundTaskNotificationIgnored(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("work", nil, nil)
	s.emit(echo("work"), map[string]any{"type": "system", "subtype": "task_started", "task_id": "fg1", "is_backgrounded": false},
		map[string]any{"type": "system", "subtype": "task_notification", "task_id": "fg1", "status": "completed", "output_file": ""},
		result("done"))
	s.nextResult()
	_ = s.cs.Send("next", nil, nil)
	s.emit(result("NEXT")) // no echo yet, nothing pending: still ours
	if ev := s.nextResult(); ev.Content != "NEXT" {
		t.Fatalf("result = %+v", ev)
	}
}

// Claude echoes our uuid; /compact's echo (with our uuid) carries other text,
// and its output comes first under a uuid of Claude's own.
func TestEchoMatchedByUUID(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("/compact", nil, nil)
	id := s.lastUUID()
	if len(id) != 36 {
		t.Fatalf("uuid = %q", id)
	}
	s.emit(echoID("11111111-2222-4333-8444-555555555555", "<local-command-stdout>Compacted</local-command-stdout>"),
		echoID(id, "<command-name>/compact</command-name>"),
		map[string]any{"type": "result", "subtype": "success", "result": "", "local_command": "compact", "session_id": "s1"})
	if ev := s.nextResult(); ev.Type != core.EventResult {
		t.Fatalf("result = %+v", ev)
	}
	if s.cs.promptWaiting() {
		t.Fatal("the /compact prompt still counts as waiting")
	}
}

// A dropped steered message with the same text as a later prompt does not
// take that prompt's echo.
func TestDroppedSteerSameTextAsLaterPrompt(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("work", nil, nil)
	s.emit(echoID(s.lastUUID(), "work"))
	_ = s.cs.Steer("continue", nil, nil) // dropped
	s.emit(result("done"))
	s.nextResult()
	_ = s.cs.Send("continue", nil, nil)
	s.emit(echoID(s.lastUUID(), "continue"), result("CONTINUED"))
	if ev := s.nextResult(); ev.Content != "CONTINUED" {
		t.Fatalf("result = %+v", ev)
	}
}

// A user message that happens to start like a task notice is still ours
// (matched by uuid).
func TestUserMessageLikeNotificationIsOurs(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("hi", nil, nil)
	s.emit(echoID(s.lastUUID(), "hi"), result("hello"))
	s.nextResult()
	_ = s.cs.Send("<task-notification> what is this tag?", nil, nil)
	s.emit(echoID(s.lastUUID(), "<task-notification> what is this tag?"), result("A tag."))
	if ev := s.nextResult(); ev.Content != "A tag." {
		t.Fatalf("result = %+v", ev)
	}
}

// /stop right after a prompt is written, with no turn of Claude's own
// running, interrupts normally.
func TestPromptNotWaitingWithoutBackgroundTurn(t *testing.T) {
	s := newScriptedClaude(t)
	_ = s.cs.Send("hi", nil, nil)
	s.emit(echoID(s.lastUUID(), "hi"), result("hello"))
	s.nextResult()
	_ = s.cs.Send("next", nil, nil) // not echoed yet
	if s.cs.promptWaiting() {
		t.Fatal("prompt counted as waiting behind a turn that does not exist")
	}
}
