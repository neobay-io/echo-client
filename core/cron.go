package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// CronJob represents a persisted scheduled task.
type CronJob struct {
	ID                 string `json:"id"`
	Project            string `json:"project"`
	SessionKey         string `json:"session_key"`
	Kind               string `json:"kind,omitempty"`
	CronExpr           string `json:"cron_expr"`
	LoopInterval       string `json:"loop_interval,omitempty"`
	Prompt             string `json:"prompt"`
	Exec               string `json:"exec,omitempty"`     // shell command; mutually exclusive with Prompt
	WorkDir            string `json:"work_dir,omitempty"` // working directory for exec; empty = agent work_dir
	Description        string `json:"description"`
	Enabled            bool   `json:"enabled"`
	Silent             *bool  `json:"silent,omitempty"` // suppress start notification; nil = use global default
	Mute               bool   `json:"mute,omitempty"`   // suppress all outbound messages for this job
	AutoPausePrimitive bool   `json:"auto_pause_primitive,omitempty"`
	SessionMode        string `json:"session_mode,omitempty"`
	AgentSessionID     string `json:"agent_session_id,omitempty"` // cron's own agent session lineage; resumed each run, updated to the forked id
	SessionLabel       string `json:"session_label,omitempty"`    // display label for the associated session
	TimeoutMins        *int   `json:"timeout_mins,omitempty"`     // nil=30m, 0=unlimited, >0=minutes
	// RunAt is when a one-shot timer (kind "timer") fires.
	RunAt time.Time `json:"run_at,omitempty"`
	// QueuedAt: the timer fired while its chat was busy and waits in the
	// chat's queue (cleared when that run starts). A restart then runs it
	// instead of treating it as missed.
	QueuedAt time.Time `json:"queued_at,omitempty"`
	// SelfPaced: the agent picks each next run of this loop with
	// <loop-control>next 20m</loop-control> (LoopInterval is the fallback).
	SelfPaced bool `json:"self_paced,omitempty"`
	// Per-job agent settings; empty = the bound chat's current setting.
	Model          string    `json:"model,omitempty"`
	ReasoningLevel string    `json:"reasoning_level,omitempty"`
	Mode           string    `json:"mode,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	LastRun        time.Time `json:"last_run,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
}

// IsShellJob returns true if the job executes a shell command directly.
func (j *CronJob) IsShellJob() bool {
	return strings.TrimSpace(j.Exec) != ""
}

func NormalizeCronJobKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "cron":
		return "cron"
	case "loop":
		return "loop"
	case "timer":
		return "timer"
	default:
		return strings.ToLower(strings.TrimSpace(kind))
	}
}

// IsTimerJob reports a one-shot timer (/timer).
func (j *CronJob) IsTimerJob() bool {
	return j.JobKind() == "timer"
}

func (j *CronJob) JobKind() string {
	if j == nil {
		return "cron"
	}
	return NormalizeCronJobKind(j.Kind)
}

func (j *CronJob) IsLoopJob() bool {
	return j.JobKind() == "loop"
}

func (j *CronJob) ScheduleSpec() (string, error) {
	if j.IsLoopJob() {
		d, _, err := ParseLoopIntervalSpec(j.LoopInterval)
		if err != nil {
			return "", err
		}
		return "@every " + d.String(), nil
	}
	return j.CronExpr, nil
}

const defaultCronJobTimeout = 30 * time.Minute

func (j *CronJob) ExecutionTimeout() time.Duration {
	if j.TimeoutMins == nil {
		return defaultCronJobTimeout
	}
	if *j.TimeoutMins <= 0 {
		return 0
	}
	return time.Duration(*j.TimeoutMins) * time.Minute
}

func NormalizeCronSessionMode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "reuse":
		return ""
	case "new_per_run", "new-per-run":
		return "new_per_run"
	default:
		return s
	}
}

func (j *CronJob) UsesNewSessionPerRun() bool {
	return NormalizeCronSessionMode(j.SessionMode) == "new_per_run"
}

func validateCronJob(j *CronJob) error {
	j.Kind = NormalizeCronJobKind(j.Kind)
	switch j.Kind {
	case "cron", "loop", "timer":
	default:
		return fmt.Errorf("invalid job kind %q", j.Kind)
	}
	if j.IsTimerJob() {
		if j.RunAt.IsZero() {
			return fmt.Errorf("timer needs a time to run at")
		}
		if strings.TrimSpace(j.Prompt) == "" && strings.TrimSpace(j.Exec) == "" {
			return fmt.Errorf("timer prompt is required")
		}
	}
	mode := NormalizeCronSessionMode(j.SessionMode)
	if mode != "" && mode != "new_per_run" {
		return fmt.Errorf("invalid session_mode %q (want reuse, new_per_run, or new-per-run)", j.SessionMode)
	}
	if j.TimeoutMins != nil && *j.TimeoutMins < 0 {
		return fmt.Errorf("timeout_mins must be >= 0")
	}
	if j.IsLoopJob() {
		if strings.TrimSpace(j.Exec) != "" {
			return fmt.Errorf("loop jobs do not support exec")
		}
		if strings.TrimSpace(j.Prompt) == "" {
			return fmt.Errorf("loop prompt is required")
		}
		if _, normalized, err := ParseLoopIntervalSpec(j.LoopInterval); err != nil {
			return err
		} else {
			j.LoopInterval = normalized
		}
	}
	return nil
}

// CronStore persists cron jobs to a JSON file.
type CronStore struct {
	path string
	mu   sync.Mutex
	jobs []*CronJob
}

func NewCronStore(dataDir string) (*CronStore, error) {
	dir := filepath.Join(dataDir, "crons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "jobs.json")
	s := &CronStore{path: path}
	s.load()
	return s, nil
}

func (s *CronStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &s.jobs); err != nil {
		slog.Error("cron: failed to load jobs", "path", s.path, "error", err)
	}
}

func (s *CronStore) save() error {
	data, err := json.MarshalIndent(s.jobs, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteFile(s.path, data, 0o644)
}

func (s *CronStore) Add(job *CronJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, job)
	return s.save()
}

func (s *CronStore) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.jobs {
		if j.ID == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			if err := s.save(); err != nil {
				slog.Error("cron: failed to save jobs after remove", "id", id, "error", err)
			}
			return true
		}
	}
	return false
}

func (s *CronStore) SetEnabled(id string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.Enabled = enabled
			if err := s.save(); err != nil {
				slog.Error("cron: failed to save jobs after toggle", "id", id, "enabled", enabled, "error", err)
			}
			return true
		}
	}
	return false
}

// SetQueuedAt records (or with a zero time clears) that a timer waits in
// its chat's queue.
func (s *CronStore) SetQueuedAt(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.QueuedAt = at
			if err := s.save(); err != nil {
				slog.Error("cron: failed to save jobs after queueing a timer", "id", id, "error", err)
			}
			return
		}
	}
}

func (s *CronStore) MarkRun(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.LastRun = time.Now()
			if err != nil {
				j.LastError = err.Error()
			} else {
				j.LastError = ""
			}
			if j.IsTimerJob() {
				j.Enabled = false // a timer runs once
				j.QueuedAt = time.Time{}
			}
			if saveErr := s.save(); saveErr != nil {
				slog.Error("cron: failed to save jobs after mark run", "id", id, "error", saveErr)
			}
			return
		}
	}
}

func (s *CronStore) List() []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*CronJob, len(s.jobs))
	copy(out, s.jobs)
	return out
}

func (s *CronStore) ListByProject(project string) []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*CronJob
	for _, j := range s.jobs {
		if j.Project == project {
			out = append(out, j)
		}
	}
	return out
}

func (s *CronStore) ListBySessionKey(sessionKey string) []*CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*CronJob
	for _, j := range s.jobs {
		if j.SessionKey == sessionKey {
			out = append(out, j)
		}
	}
	return out
}

func (s *CronStore) Get(id string) *CronJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (s *CronStore) Update(id, field string, value any) bool {
	readOnly := map[string]bool{
		"id": true, "created_at": true, "last_run": true, "last_error": true,
	}
	if readOnly[strings.ToLower(strings.TrimSpace(field))] {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			if err := updateCronJobField(j, field, value); err != nil {
				return false
			}
			if err := s.save(); err != nil {
				slog.Error("cron: failed to save jobs after update", "id", id, "field", field, "error", err)
			}
			return true
		}
	}
	return false
}

func updateCronJobField(job *CronJob, field string, value any) error {
	switch field {
	case "project":
		if v, ok := value.(string); ok {
			job.Project = v
			return nil
		}
	case "session_key":
		if v, ok := value.(string); ok {
			job.SessionKey = v
			return nil
		}
	case "cron_expr":
		if v, ok := value.(string); ok {
			job.CronExpr = v
			return nil
		}
	case "loop_interval":
		if v, ok := value.(string); ok {
			job.LoopInterval = v
			return nil
		}
	case "prompt":
		if v, ok := value.(string); ok {
			job.Prompt = v
			return nil
		}
	case "exec":
		if v, ok := value.(string); ok {
			job.Exec = v
			return nil
		}
	case "work_dir":
		if v, ok := value.(string); ok {
			job.WorkDir = v
			return nil
		}
	case "description":
		if v, ok := value.(string); ok {
			job.Description = v
			return nil
		}
	case "enabled":
		if v, ok := value.(bool); ok {
			job.Enabled = v
			return nil
		}
	case "silent":
		if v, ok := value.(bool); ok {
			job.Silent = &v
			return nil
		}
	case "mute":
		if v, ok := value.(bool); ok {
			job.Mute = v
			return nil
		}
	case "auto_pause_primitive":
		if v, ok := value.(bool); ok {
			job.AutoPausePrimitive = v
			return nil
		}
	case "session_mode":
		if v, ok := value.(string); ok {
			job.SessionMode = NormalizeCronSessionMode(v)
			return nil
		}
	case "model":
		if v, ok := value.(string); ok {
			job.Model = strings.TrimSpace(v)
			return nil
		}
	case "reasoning_level", "effort":
		if v, ok := value.(string); ok {
			job.ReasoningLevel = strings.TrimSpace(v)
			return nil
		}
	case "mode":
		if v, ok := value.(string); ok {
			job.Mode = strings.TrimSpace(v)
			return nil
		}
	case "self_paced":
		if v, ok := value.(bool); ok {
			job.SelfPaced = v
			return nil
		}
	case "agent_session_id":
		if v, ok := value.(string); ok {
			job.AgentSessionID = v
			return nil
		}
	case "session_label":
		if v, ok := value.(string); ok {
			job.SessionLabel = v
			return nil
		}
	case "timeout_mins":
		switch v := value.(type) {
		case int:
			job.TimeoutMins = &v
			return nil
		case float64:
			n := int(v)
			job.TimeoutMins = &n
			return nil
		}
	}

	return fmt.Errorf("unknown or invalid field: %s", field)
}

// CronScheduler runs cron jobs by injecting synthetic messages into engines.
type CronScheduler struct {
	store         *CronStore
	cron          *cron.Cron
	engines       map[string]*Engine // project name → engine
	mu            sync.RWMutex
	entries       map[string]cron.EntryID // job ID → cron entry
	defaultSilent bool                    // global default for suppressing cron start notifications
}

func NewCronScheduler(store *CronStore) *CronScheduler {
	return &CronScheduler{
		store:   store,
		cron:    cron.New(),
		engines: make(map[string]*Engine),
		entries: make(map[string]cron.EntryID),
	}
}

func (cs *CronScheduler) RegisterEngine(name string, e *Engine) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.engines[name] = e
}

func (cs *CronScheduler) SetDefaultSilent(silent bool) {
	cs.defaultSilent = silent
}

// IsSilent returns whether the cron job should suppress the start notification.
func (cs *CronScheduler) IsSilent(job *CronJob) bool {
	if job.Silent != nil {
		return *job.Silent
	}
	return cs.defaultSilent
}

func (cs *CronScheduler) Start() error {
	cs.pruneFiredTimers()
	jobs := cs.store.List()
	for _, job := range jobs {
		if job.Enabled {
			if err := cs.scheduleJob(job); err != nil {
				slog.Warn("cron: failed to schedule job", "id", job.ID, "error", err)
			}
		}
	}
	cs.cron.Start()
	slog.Info("cron: scheduler started", "jobs", len(jobs))
	return nil
}

// pruneFiredTimers deletes timers that ran more than a day ago.
func (cs *CronScheduler) pruneFiredTimers() {
	for _, job := range cs.store.List() {
		if job.IsTimerJob() && !job.Enabled && !job.LastRun.IsZero() && time.Since(job.LastRun) > 24*time.Hour {
			cs.store.Remove(job.ID)
		}
	}
}

func (cs *CronScheduler) Stop() {
	cs.cron.Stop()
}

func (cs *CronScheduler) AddJob(job *CronJob) error {
	if err := validateCronJob(job); err != nil {
		return err
	}
	job.Kind = NormalizeCronJobKind(job.Kind)
	job.SessionMode = NormalizeCronSessionMode(job.SessionMode)
	if !job.IsLoopJob() && !job.IsTimerJob() {
		if _, err := cron.ParseStandard(job.CronExpr); err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", job.CronExpr, err)
		}
	}
	if err := cs.store.Add(job); err != nil {
		return err
	}
	if job.Enabled {
		return cs.scheduleJob(job)
	}
	return nil
}

func (cs *CronScheduler) RemoveJob(id string) bool {
	// Store first: RescheduleLoopAfter checks the store under cs.mu, so it
	// cannot schedule a job that is being removed.
	removed := cs.store.Remove(id)
	cs.mu.Lock()
	if entryID, ok := cs.entries[id]; ok {
		cs.cron.Remove(entryID)
		delete(cs.entries, id)
	}
	cs.mu.Unlock()
	return removed
}

func (cs *CronScheduler) EnableJob(id string) error {
	if j := cs.store.Get(id); j != nil && j.IsTimerJob() && !j.LastRun.IsZero() {
		return fmt.Errorf("timer %q already ran; set a new one", id)
	}
	if !cs.store.SetEnabled(id, true) {
		return fmt.Errorf("job %q not found", id)
	}
	job := cs.store.Get(id)
	if job != nil {
		return cs.scheduleJob(job)
	}
	return nil
}

func (cs *CronScheduler) DisableJob(id string) error {
	if !cs.store.SetEnabled(id, false) {
		return fmt.Errorf("job %q not found", id)
	}
	cs.mu.Lock()
	if entryID, ok := cs.entries[id]; ok {
		cs.cron.Remove(entryID)
		delete(cs.entries, id)
	}
	cs.mu.Unlock()
	return nil
}

func (cs *CronScheduler) UpdateJob(id, field string, value any) error {
	job := cs.store.Get(id)
	if job == nil {
		return fmt.Errorf("job %q not found", id)
	}
	proposed := cloneCronJob(job)
	if field == "enabled" && value == true && job.IsTimerJob() && !job.LastRun.IsZero() {
		return fmt.Errorf("timer %q already ran; set a new one", id)
	}
	if field == "cron_expr" && proposed.JobKind() != "cron" {
		return fmt.Errorf("cron_expr applies to cron jobs only")
	}
	if field == "cron_expr" {
		expr, ok := value.(string)
		if !ok {
			return fmt.Errorf("cron_expr must be a string")
		}
		if _, err := cron.ParseStandard(expr); err != nil {
			return fmt.Errorf("invalid cron expression %q: %w", expr, err)
		}
	}
	if field == "loop_interval" {
		spec, ok := value.(string)
		if !ok {
			return fmt.Errorf("loop_interval must be a string")
		}
		if _, _, err := ParseLoopIntervalSpec(spec); err != nil {
			return err
		}
	}
	if err := updateCronJobField(proposed, field, value); err != nil {
		return fmt.Errorf("failed to update field %q (may be read-only or invalid type)", field)
	}
	if err := validateCronJob(proposed); err != nil {
		return err
	}

	needsReschedule := field == "cron_expr" || field == "loop_interval" || field == "enabled"
	if needsReschedule {
		cs.mu.Lock()
		if entryID, ok := cs.entries[id]; ok {
			cs.cron.Remove(entryID)
			delete(cs.entries, id)
		}
		cs.mu.Unlock()
	}

	if !cs.store.Update(id, field, value) {
		return fmt.Errorf("failed to update field %q (may be read-only or invalid type)", field)
	}

	updated := cs.store.Get(id)
	if needsReschedule && updated != nil && updated.Enabled {
		if err := cs.scheduleJob(updated); err != nil {
			return fmt.Errorf("reschedule failed: %w", err)
		}
	}
	return nil
}

func cloneCronJob(job *CronJob) *CronJob {
	if job == nil {
		return nil
	}
	clone := *job
	if job.TimeoutMins != nil {
		v := *job.TimeoutMins
		clone.TimeoutMins = &v
	}
	if job.Silent != nil {
		v := *job.Silent
		clone.Silent = &v
	}
	return &clone
}

func (cs *CronScheduler) Store() *CronStore {
	return cs.store
}

// NextRun returns the next scheduled run time for a job, or zero if not scheduled.
func (cs *CronScheduler) NextRun(jobID string) time.Time {
	cs.mu.RLock()
	entryID, ok := cs.entries[jobID]
	cs.mu.RUnlock()
	if !ok {
		return time.Time{}
	}
	for _, e := range cs.cron.Entries() {
		if e.ID == entryID {
			return e.Next
		}
	}
	return time.Time{}
}

func (cs *CronScheduler) scheduleJob(job *CronJob) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Remove existing schedule if any
	if old, ok := cs.entries[job.ID]; ok {
		cs.cron.Remove(old)
	}

	if job.IsTimerJob() {
		// A timer missed while the service was down fires shortly after
		// start, unless it is long overdue: then it is recorded as missed.
		at := job.RunAt
		// A timer that had fired and was waiting in the chat's queue still
		// runs: it was due, only the restart lost its place in the queue.
		if overdue := time.Since(at); overdue > timerStaleAfter && job.QueuedAt.IsZero() {
			slog.Warn("cron: timer missed while the service was down", "id", job.ID, "due", at)
			cs.store.MarkRun(job.ID, fmt.Errorf("missed: due %s while the service was down", at.Format("2006-01-02 15:04")))
			return nil
		}
		if now := time.Now(); !at.After(now) {
			at = now.Add(timerMissedGrace)
		}
		jobID := job.ID
		cs.entries[jobID] = cs.cron.Schedule(onceSchedule{at: at}, cron.FuncJob(func() { cs.executeJob(jobID) }))
		return nil
	}

	spec, err := job.ScheduleSpec()
	if err != nil {
		return err
	}
	jobID := job.ID
	entryID, err := cs.cron.AddFunc(spec, func() {
		cs.executeJob(jobID)
	})
	if err != nil {
		return err
	}
	cs.entries[jobID] = entryID
	return nil
}

// timerMissedGrace delays a timer whose time passed (service was down).
var timerMissedGrace = 30 * time.Second

// timerStaleAfter: a timer overdue by more than this is not run late.
const timerStaleAfter = 15 * time.Minute

// onceSchedule runs once at at (a /timer).
type onceSchedule struct{ at time.Time }

func (s onceSchedule) Next(t time.Time) time.Time {
	if t.Before(s.at) {
		return s.at
	}
	return time.Time{} // never again
}

// firstThenEvery runs at first, then every interval (a loop whose agent
// asked for its next run).
type firstThenEvery struct {
	first time.Time
	every time.Duration
}

func (s firstThenEvery) Next(t time.Time) time.Time {
	if t.Before(s.first) {
		return s.first
	}
	return t.Add(s.every)
}

// RescheduleLoopAfter makes a loop's next run happen after d (the agent's
// <loop-control>next …</loop-control>), then continue at its interval.
func (cs *CronScheduler) RescheduleLoopAfter(jobID string, d time.Duration) error {
	// Checked under cs.mu: DisableJob / RemoveJob change the store before
	// they take cs.mu to drop the entry, so a paused or deleted loop is
	// never scheduled again here.
	cs.mu.Lock()
	defer cs.mu.Unlock()
	job := cs.store.Get(jobID)
	if job == nil || !job.IsLoopJob() || !job.Enabled {
		return fmt.Errorf("loop %s is not active", jobID)
	}
	every, _, err := ParseLoopIntervalSpec(job.LoopInterval)
	if err != nil {
		return err
	}
	if old, ok := cs.entries[jobID]; ok {
		cs.cron.Remove(old)
	}
	cs.entries[jobID] = cs.cron.Schedule(firstThenEvery{first: time.Now().Add(d), every: every}, cron.FuncJob(func() { cs.executeJob(jobID) }))
	return nil
}

func (cs *CronScheduler) executeJob(jobID string) {
	job := cs.store.Get(jobID)
	if job == nil || !job.Enabled {
		return
	}
	if job.IsTimerJob() {
		// Fired: drop the schedule entry (it never runs again).
		cs.mu.Lock()
		if entryID, ok := cs.entries[jobID]; ok {
			cs.cron.Remove(entryID)
			delete(cs.entries, jobID)
		}
		cs.mu.Unlock()
	}

	cs.mu.RLock()
	engine, ok := cs.engines[job.Project]
	cs.mu.RUnlock()

	if !ok {
		slog.Error("cron: project not found", "job", jobID, "project", job.Project)
		cs.store.MarkRun(jobID, fmt.Errorf("project %q not found", job.Project))
		return
	}

	slog.Info("cron: executing job", "id", jobID, "project", job.Project, "prompt", truncateStr(job.Prompt, 60))

	jobCtx := context.Background()
	cancel := func() {}
	done := make(chan error, 1)
	timeout := job.ExecutionTimeout()
	if timeout > 0 {
		jobCtx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	defer cancel()
	go func() {
		done <- engine.ExecuteCronJob(jobCtx, job)
	}()

	err := <-done
	if err == errCronEnqueued {
		// Chat was busy: the run was queued and a later drain will execute it
		// (and MarkRun the real result). Don't record it as completed now.
		slog.Info("cron: enqueued (chat busy), will run on drain", "id", jobID)
		if job.IsTimerJob() {
			cs.store.SetQueuedAt(jobID, time.Now())
		}
		return
	}
	if timeout > 0 && err == context.DeadlineExceeded {
		err = fmt.Errorf("job timed out after %v", timeout)
	}

	cs.store.MarkRun(jobID, err)

	if err != nil {
		slog.Error("cron: job failed", "id", jobID, "error", err)
	} else {
		slog.Info("cron: job completed", "id", jobID)
	}
}

func GenerateCronID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		slog.Warn("cron: failed to generate random id bytes", "error", err)
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

const minLoopInterval = 5 * time.Second

func ParseLoopIntervalSpec(spec string) (time.Duration, string, error) {
	spec = strings.ToLower(strings.TrimSpace(spec))
	if spec == "" {
		return 0, "", fmt.Errorf("loop interval is required")
	}
	if len(spec) < 2 {
		return 0, "", fmt.Errorf("invalid loop interval %q", spec)
	}
	unit := spec[len(spec)-1]
	valueText := spec[:len(spec)-1]
	value, err := strconv.Atoi(valueText)
	if err != nil || value <= 0 {
		return 0, "", fmt.Errorf("invalid loop interval %q", spec)
	}
	var d time.Duration
	switch unit {
	case 's':
		d = time.Duration(value) * time.Second
	case 'm':
		d = time.Duration(value) * time.Minute
	case 'h':
		d = time.Duration(value) * time.Hour
	case 'd':
		d = time.Duration(value) * 24 * time.Hour
	default:
		return 0, "", fmt.Errorf("invalid loop interval unit %q (use s, m, h, or d)", string(unit))
	}
	if d < minLoopInterval {
		return 0, "", fmt.Errorf("loop interval %q is too short (minimum %s)", spec, minLoopInterval)
	}
	return d, fmt.Sprintf("%d%c", value, unit), nil
}

func LoopIntervalToHuman(spec string) string {
	_, normalized, err := ParseLoopIntervalSpec(spec)
	if err != nil {
		return spec
	}
	unit := normalized[len(normalized)-1]
	value := normalized[:len(normalized)-1]
	switch unit {
	case 's':
		return "every " + value + "s"
	case 'm':
		return "every " + value + "m"
	case 'h':
		return "every " + value + "h"
	case 'd':
		return "every " + value + "d"
	default:
		return normalized
	}
}

var cronWeekdays = map[Language][7]string{
	LangEnglish:            {"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"},
	LangChinese:            {"周日", "周一", "周二", "周三", "周四", "周五", "周六"},
	LangTraditionalChinese: {"週日", "週一", "週二", "週三", "週四", "週五", "週六"},
	LangJapanese:           {"日曜", "月曜", "火曜", "水曜", "木曜", "金曜", "土曜"},
	LangSpanish:            {"domingo", "lunes", "martes", "miércoles", "jueves", "viernes", "sábado"},
}

var cronMonths = map[Language][13]string{
	LangEnglish:            {"", "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"},
	LangChinese:            {"", "1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	LangTraditionalChinese: {"", "1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	LangJapanese:           {"", "1月", "2月", "3月", "4月", "5月", "6月", "7月", "8月", "9月", "10月", "11月", "12月"},
	LangSpanish:            {"", "ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"},
}

func cronLangNames(lang Language) (weekdays [7]string, months [13]string) {
	if w, ok := cronWeekdays[lang]; ok {
		weekdays = w
	} else {
		weekdays = cronWeekdays[LangEnglish]
	}
	if m, ok := cronMonths[lang]; ok {
		months = m
	} else {
		months = cronMonths[LangEnglish]
	}
	return
}

// cronPhrases are the words CronExprToHuman puts around the numbers.
type cronPhrases struct {
	everyMinute, everyMinutes, hourly, everyHours string
	daily, weekly, monthly, yearly                string
	oneDay, dayRange, listSep                     string
}

var cronPhrasesByLang = map[Language]cronPhrases{
	LangEnglish: {
		everyMinute: "Every minute", everyMinutes: "Every %d minutes",
		hourly: "Every hour at :%02d", everyHours: "Every %d hours at :%02d",
		daily: "Daily at %s", weekly: "%s at %s",
		monthly: "Monthly on day %d at %s", yearly: "Every year on %s %d at %s",
		oneDay: "Every %s", dayRange: "%s to %s", listSep: ", ",
	},
	LangChinese: {
		everyMinute: "每分钟", everyMinutes: "每 %d 分钟",
		hourly: "每小时第 %d 分", everyHours: "每 %d 小时，第 %d 分",
		daily: "每天 %s", weekly: "%s %s",
		monthly: "每月 %d 日 %s", yearly: "每年 %s%d日 %s",
		oneDay: "每%s", dayRange: "每%s至%s", listSep: "、",
	},
	LangTraditionalChinese: {
		everyMinute: "每分鐘", everyMinutes: "每 %d 分鐘",
		hourly: "每小時第 %d 分", everyHours: "每 %d 小時，第 %d 分",
		daily: "每天 %s", weekly: "%s %s",
		monthly: "每月 %d 日 %s", yearly: "每年 %s%d日 %s",
		oneDay: "每%s", dayRange: "每%s至%s", listSep: "、",
	},
	LangJapanese: {
		everyMinute: "毎分", everyMinutes: "%d 分ごと",
		hourly: "毎時 %d 分", everyHours: "%d 時間ごと（%d 分）",
		daily: "毎日 %s", weekly: "%s %s",
		monthly: "毎月 %d 日 %s", yearly: "毎年 %s%d日 %s",
		oneDay: "毎週%s", dayRange: "毎週%s〜%s", listSep: "、",
	},
	LangSpanish: {
		everyMinute: "Cada minuto", everyMinutes: "Cada %d minutos",
		hourly: "Cada hora en el minuto %d", everyHours: "Cada %d horas en el minuto %d",
		daily: "Diario a las %s", weekly: "%s a las %s",
		monthly: "Mensual, día %d a las %s", yearly: "Cada año, el %[2]d de %[1]s a las %[3]s",
		oneDay: "Cada %s", dayRange: "De %s a %s", listSep: ", ",
	},
}

// CronExprToHuman describes a cron expression in words. Forms it cannot
// describe exactly (names, ranges of hours, steps with offsets, ...) come
// back as the expression itself.
func CronExprToHuman(expr string, lang Language) string {
	spec, tz := strings.TrimSpace(expr), ""
	if f := strings.Fields(spec); len(f) == 6 && (strings.HasPrefix(f[0], "CRON_TZ=") || strings.HasPrefix(f[0], "TZ=")) {
		tz = f[0][strings.IndexByte(f[0], '=')+1:]
		spec = strings.Join(f[1:], " ")
	}
	human := cronSpecToHuman(spec, lang)
	if human == "" {
		return expr
	}
	if tz != "" {
		human += " (" + tz + ")"
	}
	return human
}

func cronSpecToHuman(spec string, lang Language) string {
	f := strings.Fields(spec)
	if len(f) != 5 {
		return ""
	}
	minute, hour, dom, month, dow := f[0], f[1], f[2], f[3], f[4]
	t, ok := cronPhrasesByLang[lang]
	if !ok {
		t = cronPhrasesByLang[LangEnglish]
	}
	weekdays, months := cronLangNames(lang)
	everyDay := dom == "*" && month == "*" && dow == "*"

	if n, ok := cronStep(minute, 60); ok {
		if hour != "*" || !everyDay {
			return ""
		}
		if n == 1 {
			return t.everyMinute
		}
		return fmt.Sprintf(t.everyMinutes, n)
	}
	m, ok := cronNumber(minute, 0, 59)
	if !ok {
		return ""
	}
	if hour == "*" {
		if !everyDay {
			return ""
		}
		return fmt.Sprintf(t.hourly, m)
	}
	if n, ok := cronStep(hour, 24); ok {
		if !everyDay {
			return ""
		}
		if n == 1 {
			return fmt.Sprintf(t.hourly, m)
		}
		return fmt.Sprintf(t.everyHours, n, m)
	}
	hours, ok := cronNumberList(hour, 0, 23)
	if !ok {
		return ""
	}
	times := make([]string, len(hours))
	for i, h := range hours {
		times[i] = fmt.Sprintf("%02d:%02d", h, m)
	}
	at := strings.Join(times, t.listSep)

	switch {
	case everyDay:
		return fmt.Sprintf(t.daily, at)
	case dom == "*" && month == "*":
		days := cronDaysText(dow, weekdays, t)
		if days == "" {
			return ""
		}
		return fmt.Sprintf(t.weekly, days, at)
	case dow == "*" && month == "*":
		if d, ok := cronNumber(dom, 1, 31); ok {
			return fmt.Sprintf(t.monthly, d, at)
		}
	case dow == "*":
		d, okD := cronNumber(dom, 1, 31)
		mo, okM := cronNumber(month, 1, 12)
		if okD && okM {
			return fmt.Sprintf(t.yearly, months[mo], d, at)
		}
	}
	return ""
}

// cronDaysText names the weekdays of a day-of-week field: one day, a range
// ("1-5") or a list ("1,3,5"). Other forms give "".
func cronDaysText(dow string, names [7]string, t cronPhrases) string {
	name := func(n int) string { return names[n%7] } // 7 is Sunday too
	if lo, hi, found := strings.Cut(dow, "-"); found {
		a, okA := cronNumber(lo, 0, 7)
		b, okB := cronNumber(hi, 0, 7)
		if !okA || !okB || a >= b {
			return ""
		}
		return fmt.Sprintf(t.dayRange, name(a), name(b))
	}
	days, ok := cronNumberList(dow, 0, 7)
	if !ok {
		return ""
	}
	out := make([]string, len(days))
	for i, d := range days {
		out[i] = name(d)
	}
	return fmt.Sprintf(t.oneDay, strings.Join(out, t.listSep))
}

// cronNumber parses a plain number within [lo, hi].
func cronNumber(s string, lo, hi int) (int, bool) {
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= lo && n <= hi
}

// cronNumberList parses "n" or "n,m,..." with each number within [lo, hi].
func cronNumberList(s string, lo, hi int) ([]int, bool) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, ok := cronNumber(part, lo, hi)
		if !ok {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// cronStep parses "*/n" where n divides period (60 minutes, 24 hours): only
// then is it an even interval ("*/45" fires at :00 and :45).
func cronStep(s string, period int) (int, bool) {
	rest, ok := strings.CutPrefix(s, "*/")
	if !ok {
		return 0, false
	}
	n, ok := cronNumber(rest, 1, period)
	return n, ok && n < period && period%n == 0
}
