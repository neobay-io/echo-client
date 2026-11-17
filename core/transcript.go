package core

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// transcriptOwnershipMu serializes, across every engine in this process, the
// operations that bind an agent transcript to a chat (/attach) and the
// operations that delete one (/delete purge). Without it a purge could pass
// its reference check and then delete a transcript another chat just attached.
var transcriptOwnershipMu sync.Mutex

// agentSessionIDPattern accepts the identifiers agent CLIs generate (UUIDs and
// similar tokens) and rejects anything that could act as a path or glob.
var agentSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidAgentSessionID reports whether id is safe to use in transcript file
// lookups and deletions.
func ValidAgentSessionID(id string) bool {
	return agentSessionIDPattern.MatchString(id) && !strings.Contains(id, "..")
}

// Engines returns a snapshot of every registered engine.
func (cs *CronScheduler) Engines() []*Engine {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	engines := make([]*Engine, 0, len(cs.engines))
	for _, e := range cs.engines {
		engines = append(engines, e)
	}
	return engines
}

// peerEngines returns every engine in this process (including e) whose agent
// writes transcripts to the same place, i.e. runs the same agent type.
func (e *Engine) peerEngines() []*Engine {
	all := []*Engine{e}
	if e.cronScheduler != nil {
		all = e.cronScheduler.Engines()
	}
	peers := make([]*Engine, 0, len(all))
	seenSelf := false
	for _, other := range all {
		if other == nil || other.agent == nil || other.agent.Name() != e.agent.Name() {
			continue
		}
		if other == e {
			seenSelf = true
		}
		peers = append(peers, other)
	}
	if !seenSelf {
		peers = append(peers, e)
	}
	return peers
}

// transcriptReference describes something in this process that still needs
// the agent transcript agentSessionID, or returns "" if nothing does.
// exceptLocalID is e's local session being deleted. Caller holds
// transcriptOwnershipMu.
func (e *Engine) transcriptReference(agentSessionID, exceptLocalID string) string {
	for _, peer := range e.peerEngines() {
		except := ""
		if peer == e {
			except = exceptLocalID
		}
		if localID := peer.sessions.agentSessionReferenceExcept(agentSessionID, except); localID != "" {
			return fmt.Sprintf("project %s, session %s", peer.name, localID)
		}
		if key := peer.otherChatHolding(agentSessionID, ""); key != "" {
			return fmt.Sprintf("a running chat in project %s", peer.name)
		}
	}
	if e.cronScheduler != nil {
		for _, job := range e.cronScheduler.Store().List() {
			if job != nil && strings.TrimSpace(job.AgentSessionID) == agentSessionID {
				return fmt.Sprintf("cron job %s", job.ID)
			}
		}
	}
	return ""
}
