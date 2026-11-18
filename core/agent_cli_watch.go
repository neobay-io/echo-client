package core

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// ModelCacheInvalidator is implemented by agents that cache their model
// list. The engine calls it when the agent's CLI changes on disk, since a new
// version can bring new models.
type ModelCacheInvalidator interface {
	InvalidateModelCache()
}

// agentCLIIdentity identifies the agent CLI binary on disk. An upgrade
// changes it, whether it replaces the file or repoints a symlink.
type agentCLIIdentity struct {
	path  string
	size  int64
	mtime time.Time
}

func (a agentCLIIdentity) equal(b agentCLIIdentity) bool {
	return a.path == b.path && a.size == b.size && a.mtime.Equal(b.mtime)
}

// agentCLIIdentityOf is replaced in tests.
var agentCLIIdentityOf = func(agentType string) (agentCLIIdentity, bool) {
	path, err := ResolveAgentCLI(agentType)
	if err != nil {
		return agentCLIIdentity{}, false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return agentCLIIdentity{}, false
	}
	st, err := os.Stat(real)
	if err != nil {
		return agentCLIIdentity{}, false
	}
	return agentCLIIdentity{path: real, size: st.Size(), mtime: st.ModTime()}, true
}

// noteAgentCLIChange checks, before a chat turn, whether the agent's CLI
// changed on disk since it was last seen: upgraded by this or another
// echo-client service, by hand, or by the CLI's own updater. Agent processes
// that stay up between turns (Claude Code) would keep running the old
// version, so every chat's process is retired: its next turn starts the new
// version and resumes the same agent session. A turn in progress is not
// interrupted. Cached model lists are dropped too.
func (e *Engine) noteAgentCLIChange() {
	if e.agent == nil {
		return
	}
	id, ok := agentCLIIdentityOf(e.agent.Name())
	if !ok {
		return
	}
	e.agentCLIMu.Lock()
	prev, seen := e.agentCLISeen, e.agentCLISeenOK
	e.agentCLISeen, e.agentCLISeenOK = id, true
	e.agentCLIMu.Unlock()
	if !seen || prev.equal(id) {
		return
	}
	slog.Info("agent CLI changed on disk; chats restart their agent process on the next turn",
		"project", e.name, "agent", e.agent.Name(), "path", id.path)
	e.retireAllInteractiveStates()
	if inv, ok := e.agent.(ModelCacheInvalidator); ok {
		inv.InvalidateModelCache()
	}
}
