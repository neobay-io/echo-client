//go:build live

// Package e2e runs the design doc's §7 acceptance list against the real
// agent CLIs through the real engine, with an in-process chat in place of
// Telegram / Feishu (go test -tags live -run TestParity ./e2e). It uses the
// accounts' quota: a few short turns per agent.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/chenhg5/cc-connect/agent/antigravity"
	_ "github.com/chenhg5/cc-connect/agent/claudecode"
	_ "github.com/chenhg5/cc-connect/agent/codex"
	_ "github.com/chenhg5/cc-connect/agent/kimi"
	"github.com/chenhg5/cc-connect/core"
)

// chat is the in-process platform: it records what the engine sends.
type chat struct {
	mu      sync.Mutex
	handler core.MessageHandler
	sent    []string
	log     func(string, ...any)
}

func (c *chat) Name() string                                      { return "live" }
func (c *chat) Start(h core.MessageHandler) error                 { c.handler = h; return nil }
func (c *chat) Stop() error                                       { return nil }
func (c *chat) Reply(_ context.Context, _ any, text string) error { c.add(text); return nil }
func (c *chat) Send(_ context.Context, _ any, text string) error  { c.add(text); return nil }
func (c *chat) ReconstructReplyCtx(string) (any, error)           { return "ctx", nil }
func (c *chat) add(text string) {
	c.mu.Lock()
	c.sent = append(c.sent, text)
	c.mu.Unlock()
	if c.log != nil {
		c.log("chat <- %s", text)
	}
}

// mark returns the number of messages so far; since returns what came after.
func (c *chat) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

func (c *chat) since(n int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > len(c.sent) {
		return ""
	}
	return strings.Join(c.sent[n:], "\n---\n")
}

const sessionKey = "live:chat:user"

type harness struct {
	t      *testing.T
	chat   *chat
	engine *core.Engine
	cron   *core.CronScheduler
	dir    string
}

func newHarness(t *testing.T, agentType string, opts map[string]any) *harness {
	t.Helper()
	dir := t.TempDir()
	opts["work_dir"] = dir
	agent, err := core.CreateAgent(agentType, opts)
	if err != nil {
		t.Skipf("%s: %v", agentType, err)
	}
	c := &chat{log: t.Logf}
	data := t.TempDir()
	e := core.NewEngine("live-"+agentType, agent, []core.Platform{c}, filepath.Join(data, "sessions.json"), core.LangEnglish)
	e.SetDataDir(data)
	store, err := core.NewCronStore(data)
	if err != nil {
		t.Fatal(err)
	}
	cs := core.NewCronScheduler(store)
	e.SetCronScheduler(cs)
	cs.RegisterEngine("live-"+agentType, e)
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Stop()
		_ = e.Stop()
	})
	return &harness{t: t, chat: c, engine: e, cron: cs, dir: dir}
}

// say sends a chat message and returns what the engine sent until want
// matches (or the timeout).
func (h *harness) say(text string, want *regexp.Regexp, timeout time.Duration) string {
	h.t.Helper()
	n := h.chat.mark()
	h.send(text)
	return h.wait(n, want, timeout)
}

// send sends a chat message without waiting for anything.
func (h *harness) send(text string) {
	h.chat.handler(h.chat, &core.Message{SessionKey: sessionKey, Platform: "live", UserID: "u", UserName: "tester", Content: text, ReplyCtx: "ctx"})
}

// count returns the number of messages since from.
func (c *chat) count(from int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent) - from
}

func (h *harness) wait(from int, want *regexp.Regexp, timeout time.Duration) string {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := h.chat.since(from)
		if want.MatchString(got) {
			return got
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("waited for %s; got:\n%s", want, got)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(s) }

// token matches a reply that is the word on a line of its own: the agent's
// thinking (text before a tool call) may quote the prompt, which says the
// word too.
func token(word string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[\s*_` + "`" + `]*` + regexp.QuoteMeta(word) + `[\s*_.!` + "`" + `]*$`)
}

type agentCase struct {
	agentType string
	opts      map[string]any
	steer     bool // the agent takes /steer
	// sandboxed: default mode works freely inside a sandbox (Codex) and
	// asks only to leave it.
	sandboxed bool
}

var cases = []agentCase{
	{"claudecode", map[string]any{"mode": "default"}, true, false},
	{"codex", map[string]any{"mode": "default"}, true, true},
	{"kimi", map[string]any{"backend": "acp", "mode": "default"}, false, false},
	{"antigravity", map[string]any{"backend": "acp", "mode": "default"}, true, false},
}

func TestParity(t *testing.T) {
	only := os.Getenv("PARITY_AGENT")
	for _, c := range cases {
		if only != "" && only != c.agentType {
			continue
		}
		t.Run(c.agentType, func(t *testing.T) { runParity(t, c) })
	}
}

var (
	permCard = re(`Permission Request`)
	idPat    = regexp.MustCompile("ID: `([0-9a-f]+)`")
)

// either matches a or b.
func either(a, b *regexp.Regexp) *regexp.Regexp {
	return regexp.MustCompile(a.String() + `|` + b.String())
}

// task sends a prompt that should end with the reply word tok, approving
// permission cards on the way; it reports how many cards it approved.
func (h *harness) task(prompt, tok string, timeout time.Duration) (string, int) {
	h.t.Helper()
	want := token(tok)
	got := h.say(prompt, either(permCard, want), timeout)
	approved := 0
	for !want.MatchString(got) && approved < 6 {
		approved++
		got = h.say("allow", either(permCard, want), timeout)
	}
	if !want.MatchString(got) {
		h.t.Fatalf("no %s after %d approvals:\n%s", tok, approved, got)
	}
	return got, approved
}

// waitN waits until at least n messages arrived since from.
func (h *harness) waitN(from, n int, timeout time.Duration) string {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for h.chat.count(from) < n {
		if time.Now().After(deadline) {
			h.t.Fatalf("waited for %d messages; got:\n%s", n, h.chat.since(from))
		}
		time.Sleep(200 * time.Millisecond)
	}
	return h.chat.since(from)
}

// pickModel returns the number of the current model in a /model list, or 1.
func pickModel(list string) string {
	cur := regexp.MustCompile(`(?m)^Current model: (.+)$`).FindStringSubmatch(list)
	for _, m := range regexp.MustCompile(`(?m)^\s*(\d+)\.\s+(\S+)`).FindAllStringSubmatch(list, -1) {
		if len(cur) == 2 && strings.Contains(cur[1], m[2]) {
			return m[1]
		}
	}
	return "1"
}

func runParity(t *testing.T, c agentCase) {
	opts := map[string]any{}
	for k, v := range c.opts {
		opts[k] = v
	}
	h := newHarness(t, c.agentType, opts)
	turn := 4 * time.Minute
	fileHas := func(name, want string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(h.dir, name))
		if err != nil || !strings.Contains(string(data), want) {
			t.Errorf("%s = %q, %v; want it to contain %q", name, data, err, want)
		}
	}

	// §7.2 the model list; pick a model; the conversation keeps its context
	h.say("Remember the word ZEBRA. Reply with exactly: OK", token("OK"), turn)
	list := h.say("/model", re(`(?m)^\s*1\.|did not return its model list`), time.Minute)
	if !re(`(?m)^\s*1\.`).MatchString(list) {
		t.Logf("NOTE: no model list on the first /model; asking again")
		list = h.say("/model", re(`(?m)^\s*1\.`), time.Minute)
	}
	h.say("/model "+pickModel(list), re(`(?i)model`), time.Minute)
	h.say("Which word did I ask you to remember? Reply with just the word.", re(`(?i)zebra`), turn)

	// §7.3 the effort levels; pick one; it applies
	got := h.say("/effort", re(`(?i)effort`), time.Minute)
	if !strings.Contains(got, "low") {
		t.Errorf("/effort offers no low level:\n%s", got)
	}
	h.say("/effort low", re(`(?i)low`), time.Minute)
	h.say("Reply with exactly: EFFORTOK", token("EFFORTOK"), turn)
	h.say("/effort", re(`(?i)current effort:\W*low`), time.Minute)

	// §7.4 default: approve in the chat; auto and bypass: a task that writes
	// a file and runs a command without asking
	if _, n := h.task("Use a tool to create a file named hello.txt containing the text hi. Then reply DONE.", "DONE", turn); n == 0 && !c.sandboxed {
		t.Errorf("default mode wrote the file without asking")
	}
	fileHas("hello.txt", "hi")
	if c.sandboxed {
		if _, n := h.task("Run the shell command `curl -sI https://example.com` — it needs network access, so request it. Then reply NETDONE.", "NETDONE", turn); n == 0 {
			t.Errorf("default mode did not ask before leaving the sandbox")
		}
	}
	for _, m := range []struct{ mode, file, tok string }{{"auto", "auto.txt", "AUTOOK"}, {"bypassPermissions", "bypass.txt", "BYPASSOK"}} {
		h.say("/mode "+m.mode, re(`(?i)mode`), time.Minute)
		_, n := h.task(fmt.Sprintf("Use a tool to create a file named %s containing the text %s, then run the shell command `ls -l %s`. Then reply %s.", m.file, m.mode, m.file, m.tok), m.tok, turn)
		switch {
		case n > 0 && m.mode == "bypassPermissions":
			t.Errorf("bypass mode asked %d times", n)
		case n > 0:
			t.Logf("NOTE: %s mode asked %d times for a workspace file and ls", m.mode, n)
		}
		fileHas(m.file, m.mode)
	}

	// §7.5 sessions: new, list, switch back, context still there
	h.say("/new", re(`(?i)new session|session`), time.Minute)
	h.say("/list", re(`(?i)\d`), time.Minute)
	h.say("/switch 1", re(`(?i)switch`), time.Minute)
	h.say("Which word did I ask you to remember at the start? Reply with just the word.", re(`(?i)zebra`), turn)

	// §7.6 a cron job runs in this chat's session; the chat goes on from its
	// result. A timer, too.
	got = h.say("/cron add * * * * * Reply with exactly: CRONOK", re(`(?i)cron job created`), time.Minute)
	from := h.chat.mark()
	h.wait(from, token("CRONOK"), 75*time.Second+turn)
	if id := idPat.FindStringSubmatch(got); len(id) == 2 {
		h.say("/cron del "+id[1], re(`(?i)deleted`), time.Minute)
	} else {
		t.Errorf("no cron id in:\n%s", got)
	}
	h.say("What exact word did you reply with in your previous message? Reply with just that word.", token("CRONOK"), turn)
	h.say("/timer 5s Reply with exactly: TIMEROK", re(`(?i)timer`), time.Minute)
	h.wait(h.chat.mark(), token("TIMEROK"), turn)

	// §7.7 a fixed loop runs 3 times and is paused
	got = h.say("/loop 20s Reply with exactly: LOOPOK", re(`(?i)loop created`), time.Minute)
	loopID := idPat.FindStringSubmatch(got)
	from = h.chat.mark()
	runs := func(p *regexp.Regexp) int { return len(p.FindAllString(h.chat.since(from), -1)) }
	deadline := time.Now().Add(3 * turn)
	for runs(token("LOOPOK")) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if n := runs(token("LOOPOK")); n < 3 {
		t.Errorf("fixed loop ran %d times", n)
	}
	if len(loopID) == 2 {
		h.say("/loop disable "+loopID[1], re(`(?i)disabled|paused`), time.Minute)
		time.Sleep(40 * time.Second) // a run under way when paused may still finish
		n := runs(token("LOOPOK"))
		time.Sleep(40 * time.Second)
		if m := runs(token("LOOPOK")); m != n {
			t.Errorf("paused loop ran %d more times", m-n)
		}
		h.say("/loop del "+loopID[1], re(`(?i)deleted`), time.Minute)
	}

	// §7.7 a self-paced loop: the agent picks the next run (1m, the least)
	// and pauses the loop itself after the third run. The harness brings the
	// first run forward instead of waiting the 30m fallback.
	got = h.say("/loop auto Reply with SELFOK and the number of times you have replied SELFOK in this conversation so far, counting this one (SELFOK 1, SELFOK 2, ...). After the third, pause the loop; before that, run next in 1m.", re(`(?i)loop created`), time.Minute)
	selfID := idPat.FindStringSubmatch(got)
	if len(selfID) != 2 {
		t.Fatalf("no loop id in:\n%s", got)
	}
	from = h.chat.mark()
	if err := h.cron.RescheduleLoopAfter(selfID[1], time.Second); err != nil {
		t.Fatal(err)
	}
	selfRun := regexp.MustCompile("(?m)^[\\s*_`]*SELFOK\\s+\\d+")
	deadline = time.Now().Add(3*time.Minute + 2*turn)
	for runs(selfRun) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if n := runs(selfRun); n < 3 {
		t.Errorf("self-paced loop ran %d times", n)
	}
	time.Sleep(5 * time.Second)
	if j := h.cron.Store().Get(selfID[1]); j == nil || j.Enabled {
		t.Errorf("self-paced loop did not pause itself after 3 runs")
	}
	h.say("/loop del "+selfID[1], re(`(?i)deleted`), time.Minute)

	// §7.8 a message during a long turn: steered into it where the agent
	// takes steering, else queued; /queue reorders what waits
	h.send("Write the numbers from 1 to 40 as words, one per line. Do not use tools.")
	time.Sleep(4 * time.Second)
	if c.steer {
		h.say("/steer Also end your answer with the word BANANA.", re(`BANANA`), turn)
	} else {
		h.say("Also say BANANA.", re(`BANANA`), 2*turn) // queued, runs after
	}
	// A turn long enough to queue behind (Claude Code moves a long `sleep`
	// to the background and ends the turn at once).
	h.send("Write the numbers from 1 to 300 as words, one per line. Do not use tools.")
	time.Sleep(5 * time.Second)
	h.say("Reply with exactly: QONE", re(`(?i)queued`), time.Minute)
	h.say("Reply with exactly: QTWO", re(`(?i)queued`), time.Minute)
	h.say("/queue", re(`QTWO`), time.Minute)
	h.say("/queue up 2", re(`(?i)queue order updated`), time.Minute)
	from = h.chat.mark()
	got = h.wait(from, regexp.MustCompile(token("QONE").String()+`[\s\S]*`+token("QTWO").String()+`|`+token("QTWO").String()+`[\s\S]*`+token("QONE").String()), 2*turn)
	if a, b := token("QTWO").FindStringIndex(got), token("QONE").FindStringIndex(got); a == nil || b == nil || a[0] > b[0] {
		t.Errorf("/queue up did not run QTWO first:\n%s", got)
	}

	// §7.9 stop, then go on; compact, usage, status
	h.send("Write the numbers from 1 to 300 as words, one per line. Do not use tools.")
	time.Sleep(4 * time.Second)
	h.say("/stop", re(`(?i)stop`), time.Minute)
	h.say("Reply with exactly: AFTERSTOP", token("AFTERSTOP"), turn)
	from = h.chat.mark()
	h.send("/compact")
	got = h.waitN(from, 2, turn) // "compressing", then the outcome
	if !re(`(?i)context compressed|compaction completed`).MatchString(got) {
		t.Errorf("/compact did not report completion:\n%s", got)
	}
	h.say("Reply with exactly: AFTERCOMPACT", token("AFTERCOMPACT"), turn)
	got = h.say("/usage", re(`.`), time.Minute)
	t.Logf("/usage: %s", got)
	h.say("/status", re(`(?i)session`), time.Minute)
	t.Logf("%s: parity run complete", c.agentType)
}
