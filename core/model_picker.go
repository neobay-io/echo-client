package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type modelPickerSnapshot struct {
	Token   string
	Context [32]byte
	Models  []ModelOption
	Created time.Time
}

func (e *Engine) modelPickerContext() [32]byte {
	state := map[string]any{"agent": e.agent.Name()}
	if provider, ok := e.agent.(ProviderSwitcher); ok {
		state["provider"] = provider.GetActiveProvider()
	}
	if dir, ok := e.agent.(WorkDirSwitcher); ok {
		state["dir"] = dir.GetWorkDir()
	}
	if model, ok := e.agent.(ModelSwitcher); ok {
		state["model"] = model.GetModel()
	}
	data, _ := json.Marshal(state)
	return sha256.Sum256(data)
}

// Caller holds modelPickerMu. Keep only the latest picker per chat and expire
// abandoned pickers so old cards cannot silently select from a new catalog.
func (e *Engine) modelPickerSnapshotFor(key string, contextID [32]byte) (modelPickerSnapshot, bool) {
	snapshot, ok := e.modelPickers[key]
	return snapshot, ok && time.Since(snapshot.Created) < 10*time.Minute && snapshot.Context == contextID
}

func (e *Engine) saveModelPickerSnapshot(key string, contextID [32]byte, models []ModelOption) modelPickerSnapshot {
	if e.modelPickers == nil {
		e.modelPickers = make(map[string]modelPickerSnapshot)
	}
	for key, snapshot := range e.modelPickers {
		if time.Since(snapshot.Created) >= 10*time.Minute {
			delete(e.modelPickers, key)
		}
	}
	if len(e.modelPickers) >= 256 {
		var oldestKey string
		var oldestTime time.Time
		for key, snapshot := range e.modelPickers {
			if oldestTime.IsZero() || snapshot.Created.Before(oldestTime) {
				oldestKey, oldestTime = key, snapshot.Created
			}
		}
		delete(e.modelPickers, oldestKey)
	}
	var token [8]byte
	_, _ = rand.Read(token[:])
	snapshot := modelPickerSnapshot{Token: hex.EncodeToString(token[:]), Context: contextID, Models: append([]ModelOption(nil), models...), Created: time.Now()}
	e.modelPickers[key] = snapshot
	return snapshot
}
