package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/gorilla/websocket"
)

// ── T7.2 per-task agent options ────────────────────────────────

func TestDecodeAgentOptions(t *testing.T) {
	opts, err := decodeAgentOptions(map[string]any{"model": "m", "reasoning_level": "high", "mode": "yolo", "resume_session_id": "s1"})
	if err != nil || opts != (JobAgentOptions{Model: "m", Effort: "high", Mode: "yolo", ResumeSessionID: "s1"}) {
		t.Fatalf("opts=%+v err=%v", opts, err)
	}
	if opts, err := decodeAgentOptions(nil); err != nil || !opts.IsZero() {
		t.Fatalf("nil: %+v %v", opts, err)
	}
	for _, bad := range []any{"x", map[string]any{"modle": "m"}, map[string]any{"model": 3}} {
		if _, err := decodeAgentOptions(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// validatingRunner normalizes options like an engine would.
type validatingRunner struct{ workerTestRunner }

func (validatingRunner) ValidateJobOptions(opts JobAgentOptions) (JobAgentOptions, error) {
	if opts.Mode == "nonsense" {
		return opts, os.ErrInvalid
	}
	opts.Effort = strings.ToLower(opts.Effort)
	return opts, nil
}

func TestJobAgentOptionsValidatedAndKept(t *testing.T) {
	dir := t.TempDir()
	jm, err := NewJobManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	jm.RegisterProject("p", "codex", validatingRunner{})
	jm.RegisterProject("plain", "gemini", workerTestRunner{})

	if _, err := jm.Start(JobRequest{Project: "p", Prompt: "x", AgentOptions: JobAgentOptions{Mode: "nonsense"}}); err == nil {
		t.Fatal("invalid options accepted")
	}
	if _, err := jm.Start(JobRequest{Project: "plain", Prompt: "x", AgentOptions: JobAgentOptions{Model: "m"}}); err == nil {
		t.Fatal("options accepted by a runner that cannot apply them")
	}
	job, err := jm.Start(JobRequest{Project: "p", Prompt: "x", TaskType: "review", AgentOptions: JobAgentOptions{Model: "m", Effort: "HIGH"}})
	if err != nil {
		t.Fatal(err)
	}
	if job.AgentOptions.Effort != "high" {
		t.Fatalf("options not normalized: %+v", job.AgentOptions)
	}
	if p := workerJobPayload(job); p["agent_options"] == nil {
		t.Fatalf("payload without agent_options: %v", p)
	}
	// A queued job keeps its options and task type across a reload.
	reloaded, err := NewJobManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.mu.RLock()
	managed := reloaded.jobs[job.ID]
	reloaded.mu.RUnlock()
	if managed == nil {
		t.Fatal("job not reloaded")
	}
	if managed.job.AgentOptions.Model != "m" {
		t.Fatalf("reloaded job lost its options: %+v", managed.job.AgentOptions)
	}
	if managed.job.Status == JobStatusQueued && (managed.req.AgentOptions.Model != "m" || managed.req.TaskType != "review") {
		t.Fatalf("reloaded request = %+v", managed.req)
	}
}

func TestEchoJobUsesItsOwnSettingsAndResumes(t *testing.T) {
	agent := &optsCronAgent{recordingCronAgent: recordingCronAgent{forkID: "s1"}}
	e := NewEngine("p", agent, nil, "", LangEnglish)
	runner := e.JobRunner().(engineJobRunner)
	opts, err := runner.ValidateJobOptions(JobAgentOptions{Model: "gpt-x", ResumeSessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.ValidateJobOptions(JobAgentOptions{ResumeSessionID: "--bad"}); err == nil {
		t.Fatal("option-like session id accepted")
	}
	res, err := runner.Run(context.Background(), JobRequest{Project: "p", Prompt: "go", AgentOptions: opts}, "job1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if agent.lastResume() != "s1" || agent.lastOpts().Model != "gpt-x" || res.SessionID != "s1" {
		t.Fatalf("resume=%q opts=%+v session=%q", agent.lastResume(), agent.lastOpts(), res.SessionID)
	}
	// Without options: a new session with the project's settings.
	if _, err := runner.Run(context.Background(), JobRequest{Project: "p", Prompt: "go"}, "job2", nil); err != nil {
		t.Fatal(err)
	}
	if agent.lastResume() != "" || agent.lastOpts().Model != "" {
		t.Fatalf("defaults not used: resume=%q opts=%+v", agent.lastResume(), agent.lastOpts())
	}
}

// ── T7.3 reliability ───────────────────────────────────────────

// fakeGateway is an Echo worker gateway; script decides per connection.
type fakeGateway struct {
	srv   *httptest.Server
	conns atomic.Int32
	mu    sync.Mutex
	got   []map[string]any // messages, with "_conn" added
}

func newFakeGateway(t *testing.T, onMessage func(conn *websocket.Conn, n int32, msg map[string]any)) *fakeGateway {
	t.Helper()
	g := &fakeGateway{}
	upgrader := websocket.Upgrader{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		n := g.conns.Add(1)
		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			g.mu.Lock()
			rec := cloneMap(msg)
			rec["_conn"] = n
			g.got = append(g.got, rec)
			g.mu.Unlock()
			if onMessage != nil {
				onMessage(conn, n, msg)
			}
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) find(pred func(map[string]any) bool) map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, m := range g.got {
		if pred(m) {
			return m
		}
	}
	return nil
}

func ackRegistration(conn *websocket.Conn, msg map[string]any) bool {
	reply := map[string]string{"hello": "hello_ack", "register_host": "host_registered", "register_agents": "agents_registered", "heartbeat": "heartbeat_ack"}
	if typ, ok := reply[msg["type"].(string)]; ok {
		_ = conn.WriteJSON(map[string]any{"type": typ, "request_id": msg["request_id"]})
		return true
	}
	return false
}

func newTestWorker(t *testing.T, url string, runner JobRunner) (*WorkerClient, *JobManager) {
	t.Helper()
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jm.RegisterProject("echo-manager-claude", "claudecode", runner)
	c, err := NewWorkerClient(config.EchoConfig{ServerURL: url, HostID: "h", HeartbeatIntervalSec: 60},
		[]config.ProjectConfig{{Name: "echo-manager-claude", Agent: config.AgentConfig{Type: "claudecode", Options: map[string]any{"mode": "bypassPermissions"}}}}, jm)
	if err != nil {
		t.Fatal(err)
	}
	return c, jm
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Echo forgets a worker's jobs when it disconnects: after reconnecting the
// worker reports them again, including a result produced while offline.
// Registration carries the live metadata; results carry the agent session.
func TestWorkerResyncsJobsAfterReconnect(t *testing.T) {
	g := newFakeGateway(t, func(conn *websocket.Conn, n int32, msg map[string]any) {
		ackRegistration(conn, msg)
		if n == 1 && msg["type"] == "register_agents" {
			_ = conn.WriteJSON(map[string]any{"type": "assign_task", "request_id": "a1", "agent_project": "echo-manager-claude", "task_id": "t1", "prompt": "go",
				"agent_options": map[string]any{"model": "m1"}})
		}
		if n == 1 && msg["type"] == "task_assigned" {
			_ = conn.Close() // drop the connection while the job runs
		}
	})
	c, _ := newTestWorker(t, g.srv.URL, validatingRunner{})
	c.SetAgentMetadata(func(_ context.Context, project string) map[string]any {
		return map[string]any{"cli_version": "test 1.0", "project_seen": project}
	})
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	t.Cleanup(func() { cancel(); _ = c.Stop(context.Background()) })

	result := func() map[string]any {
		return g.find(func(m map[string]any) bool { return m["type"] == "task_result" && m["_conn"] == int32(2) })
	}
	waitFor(t, "the result on the second connection", func() bool { return result() != nil })
	job, _ := result()["job"].(map[string]any)
	if job["status"] != JobStatusCompleted || job["agent_session_id"] != "session-1" {
		t.Fatalf("result job = %v", job)
	}
	if opts, _ := job["agent_options"].(map[string]any); opts["model"] != "m1" {
		t.Fatalf("agent_options = %v", job["agent_options"])
	}
	// Jobs are re-reported before the (slower) agent registration, and the
	// worker tells Echo what it supports.
	g.mu.Lock()
	resultAt, registerAt := -1, -1
	for i, m := range g.got {
		if m["_conn"] == int32(2) && m["type"] == "task_result" && resultAt < 0 {
			resultAt = i
		}
		if m["_conn"] == int32(2) && m["type"] == "register_agents" && registerAt < 0 {
			registerAt = i
		}
	}
	g.mu.Unlock()
	if resultAt < 0 || registerAt < 0 || resultAt > registerAt {
		t.Fatalf("result at %d, registration at %d", resultAt, registerAt)
	}
	host := g.find(func(m map[string]any) bool { return m["type"] == "register_host" })
	hostInfo, _ := host["host"].(map[string]any)
	hostMeta, _ := hostInfo["metadata"].(map[string]any)
	if features, _ := hostMeta["features"].([]any); len(features) == 0 || features[0] != "agent_options" {
		t.Fatalf("features = %v", hostMeta["features"])
	}
	reg := g.find(func(m map[string]any) bool { return m["type"] == "register_agents" })
	agents, _ := reg["agents"].([]any)
	md, _ := agents[0].(map[string]any)["metadata"].(map[string]any)
	if md["cli_version"] != "test 1.0" || md["project_seen"] != "echo-manager-claude" || md["skills"] == nil {
		t.Fatalf("registration metadata = %v", md)
	}
}

// A gateway that stops answering ends the session (and the worker
// reconnects) instead of waiting forever.
func TestWorkerRequestsTimeOut(t *testing.T) {
	old := workerRequestTimeout
	workerRequestTimeout = 200 * time.Millisecond
	t.Cleanup(func() { workerRequestTimeout = old })
	g := newFakeGateway(t, nil) // never answers
	c, _ := newTestWorker(t, g.srv.URL, workerTestRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	t.Cleanup(func() { cancel(); _ = c.Stop(context.Background()) })
	waitFor(t, "a reconnect", func() bool { return g.conns.Load() >= 2 })
}

// A progress update that cannot be sent does not stop the watcher.
func TestWatcherSurvivesSendFailure(t *testing.T) {
	jm, err := NewJobManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jm.RegisterProject("echo-manager-claude", "claudecode", workerTestRunner{})
	c := &WorkerClient{jobMgr: jm, watchers: map[string]context.CancelFunc{}, assigned: map[string]time.Time{}, pending: map[string]chan map[string]any{}}
	job, err := jm.Start(JobRequest{Project: "echo-manager-claude", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ensureWatcher(ctx, job.ID) // no connection: every send fails
	time.Sleep(300 * time.Millisecond)
	c.mu.Lock()
	_, watching := c.watchers[job.ID]
	c.mu.Unlock()
	if !watching {
		t.Fatal("watcher stopped after a failed send")
	}
}

// ── T7.1 registration metadata ─────────────────────────────────

func TestEchoAgentMetadata(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "fakecli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho 'fakecli 9.9.9'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := agentCLIIdentityOf
	agentCLIIdentityOf = func(Agent) (agentCLIIdentity, bool) {
		st, err := os.Stat(cli)
		if err != nil {
			return agentCLIIdentity{}, false
		}
		return agentCLIIdentity{path: cli, size: st.Size(), mtime: st.ModTime()}, true
	}
	t.Cleanup(func() { agentCLIIdentityOf = old })

	e := NewEngine("p", &stubModelModeAgent{}, nil, "", LangEnglish)
	md := e.EchoAgentMetadata(context.Background())
	if md["cli_version"] != "fakecli 9.9.9" || md["capabilities"] == nil {
		t.Fatalf("metadata = %v", md)
	}
	if md["default_model"] != "model-a" || md["default_mode"] != "yolo" {
		t.Fatalf("defaults = %v", md)
	}
	if _, ok := md["auth_status"]; ok {
		t.Fatal("auth status without an auth manager")
	}
}

// stubModelModeAgent reports a default model and mode.
type stubModelModeAgent struct{ stubAgent }

func (a *stubModelModeAgent) SetModel(string)                               {}
func (a *stubModelModeAgent) GetModel() string                              { return "model-a" }
func (a *stubModelModeAgent) AvailableModels(context.Context) []ModelOption { return nil }
func (a *stubModelModeAgent) SetMode(string)                                {}
func (a *stubModelModeAgent) GetMode() string                               { return "yolo" }
func (a *stubModelModeAgent) PermissionModes() []PermissionModeInfo {
	return []PermissionModeInfo{{Key: "yolo"}}
}

// ── P7 review fixes ────────────────────────────────────────────

// laneAgent is a Claude-named agent configured with a lane mode.
type laneAgent struct {
	namedStubAgent
	mode string
}

func (a *laneAgent) SetSessionOptions(AgentSessionOptions) {}
func (a *laneAgent) SetMode(m string)                      { a.mode = m }
func (a *laneAgent) GetMode() string                       { return a.mode }
func (a *laneAgent) PermissionModes() []PermissionModeInfo {
	return []PermissionModeInfo{{Key: "plan"}, {Key: "default"}, {Key: "acceptEdits"}, {Key: "auto"}, {Key: "bypassPermissions"}}
}

// Codex review 1: a task cannot make a lane more permissive than its
// configured mode.
func TestTaskModeStaysWithinTheLane(t *testing.T) {
	reviewer := NewEngine("rev", &laneAgent{namedStubAgent: namedStubAgent{name: "claudecode"}, mode: "plan"}, nil, "", LangEnglish)
	r := reviewer.JobRunner().(engineJobRunner)
	for _, mode := range []string{"bypassPermissions", "yolo", "default", "acceptEdits"} {
		if _, err := r.ValidateJobOptions(JobAgentOptions{Mode: mode}); err == nil {
			t.Errorf("plan lane accepted %q", mode)
		}
	}
	if _, err := r.ValidateJobOptions(JobAgentOptions{Mode: "plan"}); err != nil {
		t.Fatal(err)
	}
	coder := NewEngine("dev", &laneAgent{namedStubAgent: namedStubAgent{name: "claudecode"}, mode: "bypassPermissions"}, nil, "", LangEnglish)
	if opts, err := coder.JobRunner().(engineJobRunner).ValidateJobOptions(JobAgentOptions{Mode: "plan"}); err != nil || opts.Mode != "plan" {
		t.Fatalf("bypass lane refused a stricter mode: %+v %v", opts, err)
	}
}

// Codex review 2 and 11: only sessions of this project's own jobs can be
// resumed, and a job that failed keeps the session it ran in.
func TestResumeOnlyOwnJobSessions(t *testing.T) {
	jm, err := NewJobManager(jobTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	jm.RegisterProject("p", "codex", workerBlockingRunner{})
	jm.RegisterProject("q", "codex", validatingRunner{})
	if _, err := jm.Start(JobRequest{Project: "q", Prompt: "x", AgentOptions: JobAgentOptions{ResumeSessionID: "chat-session"}}); err == nil {
		t.Fatal("resumed a session no job ran in")
	}
	first, err := jm.Start(JobRequest{Project: "p", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job's session", func() bool {
		j, _ := jm.Get(first.ID)
		return j.SessionID == "session-blocking"
	})
	if _, err := jm.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to end", func() bool {
		j, _ := jm.Get(first.ID)
		return isTerminalJobStatus(j.Status)
	})
	if j, _ := jm.Get(first.ID); j.SessionID != "session-blocking" {
		t.Fatalf("cancelled job lost its session: %q", j.SessionID)
	}
	// Another project cannot take it over; validatingRunner lives on q.
	if _, err := jm.Start(JobRequest{Project: "q", Prompt: "x", AgentOptions: JobAgentOptions{ResumeSessionID: "session-blocking"}}); err == nil {
		t.Fatal("another project resumed the session")
	}
}

// Ownership allows the project's own sessions.
func TestResumeOwnSessionAllowed(t *testing.T) {
	jm, err := NewJobManager(jobTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	jm.RegisterProject("q", "codex", validatingRunner{})
	first, err := jm.Start(JobRequest{Project: "q", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first job", func() bool {
		j, _ := jm.Get(first.ID)
		return j.Status == JobStatusCompleted
	})
	second, err := jm.Start(JobRequest{Project: "q", Prompt: "x", AgentOptions: JobAgentOptions{ResumeSessionID: "session-1"}})
	if err != nil {
		t.Fatalf("own session refused: %v", err)
	}
	waitFor(t, "the second job", func() bool {
		j, _ := jm.Get(second.ID)
		return isTerminalJobStatus(j.Status)
	})
}

// validatingBlockingRunner takes options and blocks until cancelled.
type validatingBlockingRunner struct{ workerBlockingRunner }

func (validatingBlockingRunner) ValidateJobOptions(opts JobAgentOptions) (JobAgentOptions, error) {
	return opts, nil
}

// Kimi review 5: a queued job restored after a restart is checked again
// against the agent it now runs on.
func TestRestoredJobOptionsRevalidated(t *testing.T) {
	dir := jobTestDir(t)
	jm, err := NewJobManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	jm.RegisterProject("p", "claudecode", validatingBlockingRunner{})
	busy, err := jm.Start(JobRequest{Project: "p", Prompt: "busy"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = jm.Cancel(busy.ID)
		waitFor(t, "the busy job to stop", func() bool {
			j, _ := jm.Get(busy.ID)
			return isTerminalJobStatus(j.Status)
		})
	})
	queued, err := jm.Start(JobRequest{Project: "p", Prompt: "later", AgentOptions: JobAgentOptions{Mode: "plan"}})
	if err != nil {
		t.Fatal(err)
	}
	// Restart: the project now runs an agent that cannot take options.
	restored, err := NewJobManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored.RegisterProject("p", "gemini", workerTestRunner{})
	waitFor(t, "the restored job to end", func() bool {
		j, ok := restored.Get(queued.ID)
		return ok && isTerminalJobStatus(j.Status)
	})
	if j, _ := restored.Get(queued.ID); j.Status != JobStatusFailed || !strings.Contains(j.Error, "agent_options") {
		t.Fatalf("restored job = %s %q", j.Status, j.Error)
	}
}

// Kimi review 6: projects sharing a CLI probe it once.
func TestMetadataProbesShared(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "sharedcli")
	count := filepath.Join(dir, "count")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho x >> '"+count+"'\necho 'sharedcli 1.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := agentCLIIdentityOf
	agentCLIIdentityOf = func(Agent) (agentCLIIdentity, bool) {
		st, err := os.Stat(cli)
		if err != nil {
			return agentCLIIdentity{}, false
		}
		return agentCLIIdentity{path: cli, size: st.Size(), mtime: st.ModTime()}, true
	}
	t.Cleanup(func() { agentCLIIdentityOf = old })
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := NewEngine("p", &stubAgent{}, nil, "", LangEnglish)
			if v := e.agentCLIVersion(context.Background()); v != "sharedcli 1.0" {
				t.Errorf("version = %q", v)
			}
		}()
	}
	wg.Wait()
	data, _ := os.ReadFile(count)
	if n := strings.Count(string(data), "x"); n != 1 {
		t.Fatalf("CLI probed %d times", n)
	}
}

// Kimi review 7: the assigned set is bounded; running jobs stay.
func TestAssignedJobsBounded(t *testing.T) {
	jm, err := NewJobManager(jobTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	jm.RegisterProject("echo-manager-claude", "claudecode", workerBlockingRunner{})
	running, err := jm.Start(JobRequest{Project: "echo-manager-claude", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = jm.Cancel(running.ID)
		waitFor(t, "the job to stop", func() bool {
			j, _ := jm.Get(running.ID)
			return isTerminalJobStatus(j.Status)
		})
	})
	c := &WorkerClient{jobMgr: jm, assigned: map[string]time.Time{}}
	old := time.Now().Add(-time.Hour)
	c.assigned[running.ID] = old.Add(-time.Hour) // the oldest, but running
	for i := 0; i < maxAssignedJobs+50; i++ {
		c.assigned["gone-"+strings.Repeat("x", i%7)+time.Duration(i).String()] = old
	}
	c.pruneAssigned()
	if len(c.assigned) > maxAssignedJobs {
		t.Fatalf("%d assigned jobs kept", len(c.assigned))
	}
	if _, ok := c.assigned[running.ID]; !ok {
		t.Fatal("running job dropped")
	}
}

// jobTestDir is a data dir for a JobManager whose jobs may still write
// their final state while the test ends: removal is retried briefly.
func jobTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p7-jobs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
	return dir
}
