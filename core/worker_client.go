package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/config"
	"github.com/gorilla/websocket"
)

const (
	defaultEchoOrgID          = "coding-team"
	defaultWorkerPollInterval = 500 * time.Millisecond
)

type WorkerClient struct {
	serverURL         string
	authToken         string
	hostID            string
	label             string
	tags              []string
	heartbeatInterval time.Duration
	agents            []workerAgentRegistration
	jobMgr            *JobManager

	mu       sync.Mutex
	conn     *websocket.Conn
	sendMu   sync.Mutex
	doneCh   chan struct{}
	pending  map[string]chan map[string]any
	reqSeq   int
	watchers map[string]context.CancelFunc
	// assigned: jobs Echo assigned through this client (job ID -> when),
	// re-reported after every reconnect (T7.3).
	assigned map[string]time.Time
	// metadataFor returns a project's live registration metadata (T7.1).
	metadataFor AgentMetadataFunc
}

// AgentMetadataFunc returns a project's live metadata for Echo's agent
// registration: capabilities, CLI version, login status, defaults.
type AgentMetadataFunc func(ctx context.Context, project string) map[string]any

// SetAgentMetadata sets where registration metadata comes from.
func (c *WorkerClient) SetAgentMetadata(fn AgentMetadataFunc) {
	c.mu.Lock()
	c.metadataFor = fn
	c.mu.Unlock()
}

// workerRequestTimeout bounds a request to the gateway (hello, register,
// heartbeat); a gateway that stops answering ends the session.
var workerRequestTimeout = 30 * time.Second

// workerMetadataTimeout bounds gathering registration metadata.
var workerMetadataTimeout = 30 * time.Second

// workerResyncKeep is how long a finished job is reported again after a
// reconnect.
var workerResyncKeep = 6 * time.Hour

// maxAssignedJobs triggers pruning of old assignments.
const maxAssignedJobs = 1000

// workerFeatures are the protocol features this worker supports.
var workerFeatures = []string{"agent_options", "agent_session_id", "job_resync"}

// workerWriteTimeout bounds one websocket write; a peer that stops reading
// ends the session.
var workerWriteTimeout = 30 * time.Second

type workerAgentRegistration struct {
	ID             string         `json:"id"`
	OrgID          string         `json:"org_id"`
	Role           string         `json:"role"`
	CCProject      string         `json:"cc_project"`
	AgentType      string         `json:"agent_type"`
	PromptTemplate string         `json:"prompt_template"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

func NewWorkerClient(
	echoCfg config.EchoConfig,
	projects []config.ProjectConfig,
	jobMgr *JobManager,
) (*WorkerClient, error) {
	serverURL := strings.TrimSpace(echoCfg.ServerURL)
	if serverURL == "" {
		return nil, nil
	}
	if jobMgr == nil {
		return nil, fmt.Errorf("echo worker client requires job manager")
	}
	wsURL, err := workerGatewayWebSocketURL(serverURL)
	if err != nil {
		return nil, err
	}
	agents, err := buildWorkerAgentRegistrations(echoCfg, projects)
	if err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("echo worker client requires at least one enabled echo project")
	}

	hostID := strings.TrimSpace(echoCfg.HostID)
	if hostID == "" {
		hostname, err := os.Hostname()
		if err != nil || strings.TrimSpace(hostname) == "" {
			hostID = "host-local"
		} else {
			hostID = hostname
		}
	}
	label := strings.TrimSpace(echoCfg.Label)
	if label == "" {
		label = hostID
	}
	interval := time.Duration(echoCfg.HeartbeatIntervalSec) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}

	return &WorkerClient{
		serverURL:         wsURL,
		authToken:         strings.TrimSpace(echoCfg.AuthToken),
		hostID:            hostID,
		label:             label,
		tags:              append([]string(nil), echoCfg.Tags...),
		heartbeatInterval: interval,
		agents:            agents,
		jobMgr:            jobMgr,
		doneCh:            make(chan struct{}),
		pending:           make(map[string]chan map[string]any),
		watchers:          make(map[string]context.CancelFunc),
		assigned:          make(map[string]time.Time),
	}, nil
}

func (c *WorkerClient) Start(ctx context.Context) {
	go c.run(ctx)
}

func (c *WorkerClient) Stop(ctx context.Context) error {
	c.closeConn()
	c.stopAllWatchers()
	select {
	case <-c.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *WorkerClient) run(ctx context.Context) {
	defer close(c.doneCh)

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.runSession(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("echo worker client session ended", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (c *WorkerClient) runSession(ctx context.Context) error {
	header := http.Header{}
	if c.authToken != "" {
		header.Set("Authorization", "Bearer "+c.authToken)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, c.serverURL, header)
	if err != nil {
		return fmt.Errorf("dial echo worker gateway: %w", err)
	}
	c.setConn(conn)
	defer c.closeConn()

	readerErrCh := make(chan error, 1)
	go func() {
		readerErrCh <- c.readLoop(ctx, conn)
	}()

	if _, err := c.request(ctx, map[string]any{
		"type":           "hello",
		"worker_version": "echo-client",
	}, "hello_ack"); err != nil {
		return err
	}
	if _, err := c.request(ctx, map[string]any{
		"type": "register_host",
		"host": map[string]any{
			"id": c.hostID,
			"metadata": map[string]any{
				"label":    c.label,
				"platform": runtime.GOOS,
				"tags":     c.tags,
				// features tells Echo what this worker understands (an
				// older worker would ignore agent_options silently).
				"features": workerFeatures,
			},
		},
	}, "host_registered"); err != nil {
		return err
	}
	// Echo forgets a worker's jobs when it disconnects: report every job it
	// assigned again (state now, or the result it may have missed) right
	// away, before the slower agent registration.
	c.resyncJobs(ctx)
	if _, err := c.request(ctx, map[string]any{
		"type":    "register_agents",
		"host_id": c.hostID,
		"agents":  c.registrationAgents(ctx),
	}, "agents_registered"); err != nil {
		return err
	}

	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-readerErrCh:
			return err
		case <-ticker.C:
			if _, err := c.request(ctx, map[string]any{
				"type":      "heartbeat",
				"host_id":   c.hostID,
				"agent_ids": c.agentIDs(),
			}, "heartbeat_ack"); err != nil {
				return err
			}
		}
	}
}

func (c *WorkerClient) request(
	ctx context.Context,
	payload map[string]any,
	wantType string,
) (map[string]any, error) {
	requestID := c.nextRequestID()
	payload = cloneMap(payload)
	payload["request_id"] = requestID
	waitCh := make(chan map[string]any, 1)
	ctx, cancel := context.WithTimeout(ctx, workerRequestTimeout)
	defer cancel()

	c.mu.Lock()
	c.pending[requestID] = waitCh
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
	}()

	if err := c.sendJSON(payload); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case response := <-waitCh:
		gotType, _ := response["type"].(string)
		if gotType == "error" {
			raw, _ := json.Marshal(response)
			return nil, fmt.Errorf("echo worker gateway error: %s", strings.TrimSpace(string(raw)))
		}
		if gotType != wantType {
			return nil, fmt.Errorf("unexpected worker gateway response type %q, want %q", gotType, wantType)
		}
		return response, nil
	}
}

func (c *WorkerClient) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		var payload map[string]any
		if err := conn.ReadJSON(&payload); err != nil {
			c.failPending(err)
			return err
		}

		requestID, _ := payload["request_id"].(string)
		if requestID != "" {
			c.mu.Lock()
			waitCh := c.pending[requestID]
			c.mu.Unlock()
			if waitCh != nil {
				waitCh <- cloneMap(payload)
				continue
			}
		}

		msgType, _ := payload["type"].(string)
		switch msgType {
		case "assign_task":
			go c.handleAssignTask(ctx, payload)
		case "cancel_task":
			go c.handleCancelTask(payload)
		case "setup_workspace":
			go c.handleSetupWorkspace(payload)
		case "cleanup_workspace":
			go c.handleCleanupWorkspace(payload)
		case "capture_workspace_snapshot":
			go c.handleCaptureWorkspaceSnapshot(payload)
		case "inspect_workspace":
			go c.handleInspectWorkspace(payload)
		case "check_paths":
			go c.handleCheckPaths(payload)
		case "push_ref":
			go c.handlePushRef(payload)
		case "delete_remote_ref":
			go c.handleDeleteRemoteRef(payload)
		case "ensure_repo_checkout":
			go c.handleEnsureRepoCheckout(payload)
		case "write_repo_file":
			go c.handleWriteRepoFile(payload)
		case "finalize_source_commit":
			go c.handleFinalizeSourceCommit(payload)
		default:
			slog.Warn("worker client received unsupported gateway message", "type", msgType)
		}
	}
}

func (c *WorkerClient) handleAssignTask(ctx context.Context, payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	agentProject, _ := payload["agent_project"].(string)
	taskID, _ := payload["task_id"].(string)
	taskType, _ := payload["task_type"].(string)
	prompt, _ := payload["prompt"].(string)
	timeoutSec, _ := payload["timeout_sec"].(float64)
	workspaceRef := decodeWorkspaceRef(payload["workspace_ref"])
	agentOptions, optErr := decodeAgentOptions(payload["agent_options"])
	if optErr != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "task_assigned",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      optErr.Error(),
		})
		return
	}

	job, err := c.jobMgr.Start(JobRequest{
		Project:      strings.TrimSpace(agentProject),
		TaskID:       strings.TrimSpace(taskID),
		TaskType:     strings.TrimSpace(taskType),
		Prompt:       prompt,
		Timeout:      time.Duration(int(timeoutSec)) * time.Second,
		WorkspaceRef: workspaceRef,
		AgentOptions: agentOptions,
	})
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "task_assigned",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}

	c.mu.Lock()
	c.assigned[job.ID] = time.Now()
	over := len(c.assigned) > maxAssignedJobs
	c.mu.Unlock()
	if over {
		c.pruneAssigned()
	}
	_ = c.sendJSON(map[string]any{
		"type":       "task_assigned",
		"request_id": requestID,
		"host_id":    c.hostID,
		"job":        workerJobPayload(job),
	})
	c.ensureWatcher(ctx, job.ID)
}

// decodeAgentOptions reads assign_task.agent_options: model, effort (or
// reasoning_level), mode, resume_session_id. Unknown keys are refused so a
// misspelled option does not silently run with the defaults.
func decodeAgentOptions(raw any) (JobAgentOptions, error) {
	if raw == nil {
		return JobAgentOptions{}, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return JobAgentOptions{}, fmt.Errorf("agent_options must be an object")
	}
	var opts JobAgentOptions
	for key, value := range m {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return JobAgentOptions{}, fmt.Errorf("agent_options.%s must be a string", key)
		}
		switch key {
		case "model":
			opts.Model = text
		case "effort", "reasoning_level", "reasoning_effort":
			opts.Effort = text
		case "mode":
			opts.Mode = text
		case "resume_session_id":
			opts.ResumeSessionID = text
		default:
			return JobAgentOptions{}, fmt.Errorf("unknown agent_options key %q", key)
		}
	}
	return opts, nil
}

// registrationAgents is the agent list for register_agents, with each
// project's live metadata added (gathered in parallel, bounded).
func (c *WorkerClient) registrationAgents(ctx context.Context) []workerAgentRegistration {
	c.mu.Lock()
	metadataFor := c.metadataFor
	c.mu.Unlock()
	agents := make([]workerAgentRegistration, len(c.agents))
	copy(agents, c.agents)
	if metadataFor == nil {
		return agents
	}
	ctx, cancel := context.WithTimeout(ctx, workerMetadataTimeout)
	defer cancel()
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4) // probes start CLIs: a few at a time
	for i := range agents {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			live := metadataFor(ctx, agents[i].CCProject)
			merged := cloneMap(agents[i].Metadata)
			for k, v := range live {
				merged[k] = v
			}
			agents[i].Metadata = merged
		}(i)
	}
	wg.Wait()
	return agents
}

// resyncJobs reports the state of every job Echo assigned (after a
// reconnect), restarts watchers of jobs still running, and forgets jobs
// that finished long ago.
func (c *WorkerClient) resyncJobs(ctx context.Context) {
	c.mu.Lock()
	ids := make([]string, 0, len(c.assigned))
	for id := range c.assigned {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		job, ok := c.jobMgr.Get(id)
		if !ok {
			c.forgetAssigned(id)
			continue
		}
		if isTerminalWorkerJobStatus(job.Status) {
			if job.FinishedAt != nil && time.Since(*job.FinishedAt) > workerResyncKeep {
				c.forgetAssigned(id)
				continue
			}
			if err := c.sendJSON(map[string]any{"type": "task_result", "host_id": c.hostID, "job": workerJobPayload(job)}); err != nil {
				slog.Warn("worker client failed to resend task result", "job_id", id, "error", err)
			}
			continue
		}
		if err := c.sendJSON(map[string]any{"type": "task_progress", "host_id": c.hostID, "job": workerJobPayload(job)}); err != nil {
			slog.Warn("worker client failed to resend task progress", "job_id", id, "error", err)
		}
		c.ensureWatcher(ctx, id)
	}
}

// pruneAssigned bounds the assigned set: jobs that ended long ago go first,
// then the oldest finished ones until at most maxAssignedJobs remain. Jobs
// still queued or running are always kept.
func (c *WorkerClient) pruneAssigned() {
	c.mu.Lock()
	type entry struct {
		id string
		at time.Time
	}
	entries := make([]entry, 0, len(c.assigned))
	for id, at := range c.assigned {
		entries = append(entries, entry{id, at})
	}
	c.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	remaining := len(entries)
	for _, e := range entries {
		j, ok := c.jobMgr.Get(e.id)
		finished := !ok || isTerminalWorkerJobStatus(j.Status)
		old := !ok || (j.FinishedAt != nil && time.Since(*j.FinishedAt) > workerResyncKeep)
		if finished && (old || remaining > maxAssignedJobs) {
			c.forgetAssigned(e.id)
			remaining--
		}
	}
}

func (c *WorkerClient) forgetAssigned(jobID string) {
	c.mu.Lock()
	delete(c.assigned, jobID)
	c.mu.Unlock()
}

func (c *WorkerClient) handleCancelTask(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	jobID, _ := payload["job_id"].(string)
	job, err := c.jobMgr.Cancel(strings.TrimSpace(jobID))
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "cancel_ack",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "cancel_ack",
		"request_id": requestID,
		"host_id":    c.hostID,
		"job":        workerJobPayload(job),
	})
	c.ensureWatcher(context.Background(), job.ID)
}

func (c *WorkerClient) handleSetupWorkspace(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	baseBranch, _ := payload["base_branch"].(string)
	branchName, _ := payload["branch_name"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	result, err := SetupWorkspace(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(baseBranch),
		strings.TrimSpace(branchName),
		strings.TrimSpace(worktreePath),
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "workspace_ready",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "workspace_ready",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"repo_path":                 result.RepoPath,
			"base_branch":               result.BaseBranch,
			"branch_name":               result.BranchName,
			"worktree_path":             result.WorktreePath,
			"requested_base_commit_sha": result.RequestedBaseCommitSHA,
			"initial_head_commit_sha":   result.InitialHeadCommitSHA,
		},
	})
}

func (c *WorkerClient) handleCleanupWorkspace(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	if err := CleanupWorkspace(strings.TrimSpace(worktreePath)); err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "workspace_cleaned",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "workspace_cleaned",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"worktree_path": strings.TrimSpace(worktreePath),
		},
	})
}

func (c *WorkerClient) handleCaptureWorkspaceSnapshot(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	branchName, _ := payload["branch_name"].(string)
	result, err := CaptureWorkspaceSnapshot(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(worktreePath),
		strings.TrimSpace(branchName),
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "workspace_snapshot_captured",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "workspace_snapshot_captured",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result":     result,
	})
}

func (c *WorkerClient) handleInspectWorkspace(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	branchName, _ := payload["branch_name"].(string)
	result, err := InspectWorkspace(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(worktreePath),
		strings.TrimSpace(branchName),
		WorkspaceInspectionRequest{
			ResolveRefs:   decodeStringSlice(payload["resolve_refs"]),
			RemoteRefs:    decodeStringSlice(payload["remote_refs"]),
			WorktreePaths: decodeStringSlice(payload["worktree_paths"]),
			HeadPaths:     decodeStringSlice(payload["head_paths"]),
			IncludeHead:   decodeBool(payload["include_head"]),
			IncludeBranch: decodeBool(payload["include_branch"]),
		},
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "workspace_inspected",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "workspace_inspected",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result":     result,
	})
}

func (c *WorkerClient) handleCheckPaths(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	paths := decodeStringSlice(payload["paths"])
	result, err := CheckPaths(CheckPathsRequest{
		Paths: paths,
	})
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "paths_checked",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "paths_checked",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result":     result,
	})
}

func (c *WorkerClient) handlePushRef(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	sourceRef, _ := payload["source_ref"].(string)
	remoteRef, _ := payload["remote_ref"].(string)
	result, err := PushRef(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(worktreePath),
		strings.TrimSpace(sourceRef),
		strings.TrimSpace(remoteRef),
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "ref_pushed",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "ref_pushed",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result":     result,
	})
}

func (c *WorkerClient) handleDeleteRemoteRef(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	remoteRef, _ := payload["remote_ref"].(string)
	result, err := DeleteRemoteRef(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(worktreePath),
		strings.TrimSpace(remoteRef),
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "remote_ref_deleted",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "remote_ref_deleted",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result":     result,
	})
}

func (c *WorkerClient) handleEnsureRepoCheckout(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoURL, _ := payload["repo_url"].(string)
	repoPath, _ := payload["repo_path"].(string)
	defaultBranch, _ := payload["default_branch"].(string)
	if err := EnsureRepoCheckout(
		strings.TrimSpace(repoURL),
		strings.TrimSpace(repoPath),
		strings.TrimSpace(defaultBranch),
	); err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "repo_checkout_ready",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	branch := strings.TrimSpace(defaultBranch)
	if branch == "" {
		branch = "main"
	}
	_ = c.sendJSON(map[string]any{
		"type":       "repo_checkout_ready",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"repo_url":          strings.TrimSpace(repoURL),
			"repo_path":         strings.TrimSpace(repoPath),
			"default_branch":    branch,
			"checkout_state":    "ready",
			"provisioned":       true,
			"provisioning_mode": "worker_gateway",
		},
	})
}

func (c *WorkerClient) handleWriteRepoFile(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	relativePath, _ := payload["relative_path"].(string)
	content, _ := payload["content"].(string)
	absolutePath, err := WriteRepoFile(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(relativePath),
		content,
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "repo_file_written",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "repo_file_written",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"repo_path":     strings.TrimSpace(repoPath),
			"relative_path": strings.TrimSpace(relativePath),
			"absolute_path": absolutePath,
		},
	})
}

func (c *WorkerClient) handleFinalizeSourceCommit(payload map[string]any) {
	requestID, _ := payload["request_id"].(string)
	repoPath, _ := payload["repo_path"].(string)
	worktreePath, _ := payload["worktree_path"].(string)
	branchName, _ := payload["branch_name"].(string)
	commitMessage, _ := payload["commit_message"].(string)
	result, err := FinalizeSourceCommit(
		strings.TrimSpace(repoPath),
		strings.TrimSpace(worktreePath),
		strings.TrimSpace(branchName),
		strings.TrimSpace(commitMessage),
		decodeStringSlice(payload["artifact_paths"]),
	)
	if err != nil {
		_ = c.sendJSON(map[string]any{
			"type":       "source_commit_finalized",
			"request_id": requestID,
			"host_id":    c.hostID,
			"error":      err.Error(),
		})
		return
	}
	_ = c.sendJSON(map[string]any{
		"type":       "source_commit_finalized",
		"request_id": requestID,
		"host_id":    c.hostID,
		"result": map[string]any{
			"commit_sha":     result.CommitSHA,
			"branch_name":    result.BranchName,
			"created_commit": result.CreatedCommit,
		},
	})
}

func (c *WorkerClient) ensureWatcher(ctx context.Context, jobID string) {
	c.mu.Lock()
	if _, exists := c.watchers[jobID]; exists {
		c.mu.Unlock()
		return
	}
	watchCtx, cancel := context.WithCancel(ctx)
	c.watchers[jobID] = cancel
	c.mu.Unlock()

	go c.watchJob(watchCtx, jobID)
}

func (c *WorkerClient) watchJob(ctx context.Context, jobID string) {
	defer func() {
		c.mu.Lock()
		delete(c.watchers, jobID)
		c.mu.Unlock()
	}()

	ticker := time.NewTicker(defaultWorkerPollInterval)
	defer ticker.Stop()

	lastFingerprint := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job, ok := c.jobMgr.Get(jobID)
			if !ok {
				return
			}
			payload := map[string]any{
				"host_id": c.hostID,
				"job":     workerJobPayload(job),
			}
			fingerprint := fingerprintPayload(payload)
			if fingerprint == lastFingerprint {
				if isTerminalWorkerJobStatus(job.Status) {
					return
				}
				continue
			}
			lastFingerprint = fingerprint
			if isTerminalWorkerJobStatus(job.Status) {
				if err := c.sendJSON(map[string]any{
					"type":    "task_result",
					"host_id": c.hostID,
					"job":     workerJobPayload(job),
				}); err != nil {
					// Sent again after the reconnect (resyncJobs).
					slog.Warn("worker client failed to send task result; it is sent again after reconnecting", "job_id", jobID, "error", err)
				}
				return
			}
			if err := c.sendJSON(map[string]any{
				"type":    "task_progress",
				"host_id": c.hostID,
				"job":     workerJobPayload(job),
			}); err != nil {
				// Keep watching: the next snapshot goes out once connected.
				slog.Debug("worker client failed to send task progress", "job_id", jobID, "error", err)
				lastFingerprint = ""
			}
		}
	}
}

func (c *WorkerClient) sendJSON(payload map[string]any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("worker gateway websocket is not connected")
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(workerWriteTimeout))
	if err := conn.WriteJSON(payload); err != nil {
		// A write that failed or timed out leaves the connection unusable:
		// close it so the session ends and reconnects.
		_ = conn.Close()
		return fmt.Errorf("write %s: %w", payload["type"], err)
	}
	return nil
}

func (c *WorkerClient) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for requestID, waitCh := range c.pending {
		select {
		case waitCh <- map[string]any{
			"type":       "error",
			"request_id": requestID,
			"message":    err.Error(),
		}:
		default:
		}
	}
}

func (c *WorkerClient) stopAllWatchers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.watchers {
		cancel()
	}
	c.watchers = make(map[string]context.CancelFunc)
}

func (c *WorkerClient) nextRequestID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqSeq++
	return fmt.Sprintf("req-%d", c.reqSeq)
}

func (c *WorkerClient) agentIDs() []string {
	ids := make([]string, 0, len(c.agents))
	for _, agent := range c.agents {
		ids = append(ids, agent.ID)
	}
	return ids
}

func (c *WorkerClient) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
}

func (c *WorkerClient) closeConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func buildWorkerAgentRegistrations(
	echoCfg config.EchoConfig,
	projects []config.ProjectConfig,
) ([]workerAgentRegistration, error) {
	var agents []workerAgentRegistration
	counts := map[string]int{}
	for _, proj := range projects {
		enabled, role := resolveEchoProjectEnabled(proj)
		if !enabled {
			continue
		}
		if usesLegacyEchoRole(proj) {
			slog.Warn(
				"echo project is using deprecated [projects.echo].role; move role to the project top level",
				"project", proj.Name,
				"legacy_role", strings.TrimSpace(proj.Echo.Role),
			)
		}
		if role == "" {
			return nil, fmt.Errorf("project %q must set role (or legacy [projects.echo].role) or use an echo-* project name", proj.Name)
		}
		agentType := strings.TrimSpace(proj.Agent.Type)
		if agentType == "" {
			return nil, fmt.Errorf("project %q missing agent type", proj.Name)
		}
		if err := validateEchoWorkerMode(proj, role, agentType); err != nil {
			return nil, err
		}
		key := role + ":" + agentType
		counts[key]++
		agentID := strings.TrimSpace(proj.Echo.AgentID)
		if agentID == "" {
			agentID = fmt.Sprintf("agent-%s-%s-%d", role, agentType, counts[key])
		}
		promptTemplate := strings.TrimSpace(proj.Echo.PromptTemplate)
		if promptTemplate == "" {
			promptTemplate = fmt.Sprintf("prompts/coding/%s.md", role)
		}
		orgID := strings.TrimSpace(proj.Echo.OrgID)
		if orgID == "" {
			orgID = strings.TrimSpace(echoCfg.OrgID)
		}
		if orgID == "" {
			orgID = defaultEchoOrgID
		}
		agents = append(agents, workerAgentRegistration{
			ID:             agentID,
			OrgID:          orgID,
			Role:           role,
			CCProject:      proj.Name,
			AgentType:      agentType,
			PromptTemplate: promptTemplate,
			Metadata:       buildWorkerAgentMetadata(proj, agentType),
		})
	}
	return agents, nil
}

func buildWorkerAgentMetadata(
	proj config.ProjectConfig,
	agentType string,
) map[string]any {
	skillRoots := make([]string, 0, len(proj.SkillDirs))
	for _, root := range proj.SkillDirs {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		skillRoots = append(skillRoots, root)
	}
	return map[string]any{
		"skills": map[string]any{
			"project":                       strings.TrimSpace(proj.Name),
			"agent_type":                    strings.TrimSpace(agentType),
			"managed_native_skills_enabled": len(skillRoots) > 0,
			"include_default_skill_dirs":    proj.IncludeDefaultSkillDirs != nil && *proj.IncludeDefaultSkillDirs,
			"skill_roots":                   skillRoots,
			"native_target_kind":            nativeSkillTargetKind(agentType),
		},
	}
}

func validateEchoWorkerMode(
	proj config.ProjectConfig,
	role string,
	agentType string,
) error {
	if isEchoReviewerRole(role) {
		return nil
	}
	mode, _ := proj.Agent.Options["mode"].(string)
	if isEchoYoloEquivalentMode(agentType, mode) {
		return nil
	}
	return fmt.Errorf(
		"echo project %q role %q with agent type %q must use yolo-equivalent mode during worker registration; got %q",
		proj.Name,
		role,
		agentType,
		strings.TrimSpace(mode),
	)
}

func isEchoReviewerRole(role string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(role)), "review")
}

// isEchoYoloEquivalentMode reports whether a configured mode skips every
// approval for agentType, using the shared permission mode table.
func isEchoYoloEquivalentMode(agentType string, mode string) bool {
	return IsBypassPermissionMode(agentType, mode)
}

func resolveEchoProjectEnabled(proj config.ProjectConfig) (bool, string) {
	if proj.Echo.Enabled != nil && !*proj.Echo.Enabled {
		return false, ""
	}
	role := strings.TrimSpace(proj.Role)
	if role == "" {
		role = strings.TrimSpace(proj.Echo.Role)
	}
	if role != "" {
		return true, role
	}
	if inferredRole, ok := inferEchoRole(proj.Name); ok {
		return true, inferredRole
	}
	if proj.Echo.Enabled != nil && *proj.Echo.Enabled {
		return true, ""
	}
	return false, ""
}

func usesLegacyEchoRole(proj config.ProjectConfig) bool {
	return strings.TrimSpace(proj.Role) == "" && strings.TrimSpace(proj.Echo.Role) != ""
}

func inferEchoRole(projectName string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(projectName), "-")
	if len(parts) < 3 || parts[0] != "echo" {
		return "", false
	}
	role := strings.TrimSpace(parts[1])
	if role == "" {
		return "", false
	}
	return role, true
}

func workerGatewayWebSocketURL(serverURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return "", fmt.Errorf("parse echo server url: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported echo server url scheme %q", parsed.Scheme)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/workers/ws"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func decodeWorkspaceRef(raw any) JobWorkspaceRef {
	payload, ok := raw.(map[string]any)
	if !ok {
		return JobWorkspaceRef{}
	}
	ref := JobWorkspaceRef{}
	if value, ok := payload["repo_path"].(string); ok {
		ref.RepoPath = value
	}
	if value, ok := payload["worktree_path"].(string); ok {
		ref.WorktreePath = value
	}
	if value, ok := payload["branch"].(string); ok {
		ref.Branch = value
	}
	return ref
}

func decodeStringSlice(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		value, ok := item.(string)
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values
}

func decodeBool(raw any) bool {
	value, _ := raw.(bool)
	return value
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func fingerprintPayload(payload map[string]any) string {
	data, _ := json.Marshal(payload)
	return string(data)
}

func isTerminalWorkerJobStatus(status string) bool {
	switch status {
	case JobStatusCompleted, JobStatusFailed, JobStatusCancelled, JobStatusTimedOut, JobStatusOrphaned:
		return true
	default:
		return false
	}
}

func workerJobPayload(job *Job) map[string]any {
	if job == nil {
		return map[string]any{}
	}
	payload := map[string]any{
		"id":            job.ID,
		"project":       job.Project,
		"task_id":       job.TaskID,
		"task_type":     job.TaskType,
		"prompt":        job.Prompt,
		"workspace_ref": job.WorkspaceRef,
		"status":        job.Status,
		"output":        job.Output,
		"summary":       job.Summary,
		"session_id":    job.SessionID,
		// agent_session_id: resume it with agent_options.resume_session_id.
		"agent_session_id": job.SessionID,
		"skills_meta":      job.SkillsMeta,
		"error":            job.Error,
		"error_code":       job.ErrorCode,
		"created_at":       job.CreatedAt.Format(time.RFC3339Nano),
		"timeout_sec":      job.TimeoutSec,
		"event_count":      job.EventCount,
		"events":           []JobEvent{},
	}
	if len(job.Events) > 0 {
		payload["events"] = append([]JobEvent(nil), job.Events...)
	}
	if job.StartedAt != nil {
		payload["started_at"] = job.StartedAt.Format(time.RFC3339Nano)
	}
	if job.FinishedAt != nil {
		payload["finished_at"] = job.FinishedAt.Format(time.RFC3339Nano)
	}
	if !job.AgentOptions.IsZero() {
		payload["agent_options"] = job.AgentOptions
	}
	return payload
}
