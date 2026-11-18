package core

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Live metadata for Echo's agent registration (T7.1): what the agent can
// do, which CLI version runs, whether it is logged in, and its default
// settings. Gathered when the worker registers (every connect).

var (
	echoAuthStatusTTL  = 10 * time.Minute
	echoVersionTimeout = 10 * time.Second
	echoAuthTimeout    = 20 * time.Second
)

// echoProbes caches what the CLI probes found, process-wide: projects that
// share a CLI share its version and login state, so a registration runs each
// probe once (one at a time per CLI) however many projects use it.
var echoProbes = struct {
	mu       sync.Mutex
	keyLocks map[string]*sync.Mutex
	versions map[string]echoVersionEntry // CLI path -> version
	auth     map[string]echoAuthEntry    // agent name -> login state
}{keyLocks: map[string]*sync.Mutex{}, versions: map[string]echoVersionEntry{}, auth: map[string]echoAuthEntry{}}

type echoVersionEntry struct {
	of      agentCLIIdentity
	version string
}

type echoAuthEntry struct {
	at    time.Time
	value map[string]any
}

// probeLock serializes probes of one key (a CLI path or an agent name).
func probeLock(key string) *sync.Mutex {
	echoProbes.mu.Lock()
	defer echoProbes.mu.Unlock()
	l := echoProbes.keyLocks[key]
	if l == nil {
		l = &sync.Mutex{}
		echoProbes.keyLocks[key] = l
	}
	return l
}

// invalidateEchoAuth drops an agent's cached login state (after /auth).
func invalidateEchoAuth(agent string) {
	echoProbes.mu.Lock()
	delete(echoProbes.auth, agent)
	echoProbes.mu.Unlock()
}

// EchoAgentMetadata returns this project's live registration metadata.
func (e *Engine) EchoAgentMetadata(ctx context.Context) map[string]any {
	md := map[string]any{"capabilities": CapabilitiesOf(e.agent)}
	if v := e.agentCLIVersion(ctx); v != "" {
		md["cli_version"] = v
	}
	if st := e.echoAuthStatus(ctx); st != nil {
		md["auth_status"] = st
	}
	if ms, ok := e.agent.(ModelSwitcher); ok {
		if m := ms.GetModel(); m != "" {
			md["default_model"] = m
		}
	}
	if rs, ok := e.agent.(ReasoningLevelSwitcher); ok {
		if l := rs.GetReasoningLevel(); l != "" {
			md["default_effort"] = l
		}
	}
	if ms, ok := e.agent.(ModeSwitcher); ok {
		if m := ms.GetMode(); m != "" {
			md["default_mode"] = m
		}
	}
	return md
}

// agentCLIVersion is the first line of `<cli> --version`, cached until the
// binary changes.
func (e *Engine) agentCLIVersion(ctx context.Context) string {
	id, ok := agentCLIIdentityOf(e.agent)
	if !ok {
		return ""
	}
	l := probeLock("version:" + id.path)
	l.Lock()
	defer l.Unlock()
	echoProbes.mu.Lock()
	cached, ok := echoProbes.versions[id.path]
	echoProbes.mu.Unlock()
	if ok && cached.of.equal(id) {
		return cached.version
	}

	ctx, cancel := context.WithTimeout(ctx, echoVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, id.path, "--version")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	v := firstLine(strings.TrimSpace(string(out)), 120)
	echoProbes.mu.Lock()
	echoProbes.versions[id.path] = echoVersionEntry{of: id, version: v}
	echoProbes.mu.Unlock()
	return v
}

// echoAuthStatus reports whether the agent CLI is logged in (cached; the
// check runs the CLI). Only the login state, method and expiry are sent.
func (e *Engine) echoAuthStatus(ctx context.Context) map[string]any {
	if e.agentAuthMgr == nil {
		return nil
	}
	agent := e.agent.Name()
	l := probeLock("auth:" + agent)
	l.Lock()
	defer l.Unlock()
	echoProbes.mu.Lock()
	cached, ok := echoProbes.auth[agent]
	echoProbes.mu.Unlock()
	if ok && time.Since(cached.at) < echoAuthStatusTTL {
		return cached.value
	}

	ctx, cancel := context.WithTimeout(ctx, echoAuthTimeout)
	defer cancel()
	st, err := e.agentAuthMgr.status(ctx, agent)
	var out map[string]any
	switch {
	case err != nil && err.Error() == "auth_unsupported":
		out = map[string]any{"supported": false}
	case err != nil:
		out = map[string]any{"supported": true, "error": "status check failed"}
	default:
		out = map[string]any{"supported": true, "logged_in": st.LoggedIn}
		if st.Detail != "" {
			out["method"] = st.Detail
		}
		if !st.Expires.IsZero() {
			out["expires_at"] = st.Expires.UTC().Format(time.RFC3339)
		}
		out["checked_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	echoProbes.mu.Lock()
	echoProbes.auth[agent] = echoAuthEntry{at: time.Now(), value: out}
	echoProbes.mu.Unlock()
	return out
}
