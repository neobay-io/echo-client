package core

import (
	"strings"
	"testing"
	"time"
)

func TestModelPickerSnapshotSurvivesCatalogReorder(t *testing.T) {
	a := &stubModelSwitchAgent{name: "codex", models: []ModelOption{{Name: "first"}, {Name: "second"}}}
	p := &stubButtonPlatform{n: "feishu"}
	e := NewEngine("test", a, nil, "", LangEnglish)
	msg := &Message{SessionKey: "chat"}
	e.cmdModel(p, msg, nil)
	callback := strings.Fields(strings.TrimPrefix(p.buttonDataSnapshot()[1], "cmd:/model "))
	a.models = []ModelOption{{Name: "second"}}
	e.cmdModel(p, msg, callback)
	if a.model != "second" {
		t.Fatalf("selected %q, want snapshot's second model", a.model)
	}
}

func TestModelPickerRejectsInvalidSelectionsWithoutClearingHistory(t *testing.T) {
	for _, reason := range []string{"replaced", "expired", "out-of-range", "other-chat", "context-changed", "missing"} {
		t.Run(reason, func(t *testing.T) {
			a := &stubModelSwitchAgent{name: "codex", model: "current", models: []ModelOption{{Name: "first"}}}
			p := &stubButtonPlatform{n: "test"}
			e := NewEngine("test", a, nil, "", LangEnglish)
			msg := &Message{SessionKey: "chat"}
			s := e.sessions.GetOrCreateActive(msg.SessionKey)
			s.AgentSessionID = "keep-transcript"
			s.AddHistory("user", "keep-history")
			e.cmdModel(p, msg, nil)
			args := strings.Fields(strings.TrimPrefix(p.buttonDataSnapshot()[0], "cmd:/model "))
			switch reason {
			case "replaced":
				e.cmdModel(p, msg, nil)
			case "expired":
				snapshot := e.modelPickers[msg.SessionKey]
				snapshot.Created = time.Now().Add(-time.Hour)
				e.modelPickers[msg.SessionKey] = snapshot
			case "out-of-range":
				args = []string{"999"}
			case "other-chat":
				msg.SessionKey = "other"
			case "context-changed":
				a.name = "another-agent"
			case "missing":
				delete(e.modelPickers, msg.SessionKey)
				args = []string{"1"}
			}
			e.cmdModel(p, msg, args)
			if a.model != "current" || s.AgentSessionID != "keep-transcript" || len(s.History) != 1 {
				t.Fatal("invalid selection changed model or history")
			}
		})
	}
}

func TestModelPickerTextOnlyPagination(t *testing.T) {
	a := &stubModelSwitchAgent{name: "codex", models: make([]ModelOption, 25)}
	for i := range a.models {
		a.models[i].Name = "model"
	}
	p := &stubPlatformEngine{n: "text"}
	e := NewEngine("test", a, nil, "", LangEnglish)
	msg := &Message{SessionKey: "chat"}
	e.cmdModel(p, msg, nil)
	if !strings.Contains(p.sent[len(p.sent)-1], "/model page N (1-2)") {
		t.Fatal("missing text navigation")
	}
	e.cmdModel(p, msg, []string{"page", "2"})
	if !strings.Contains(p.sent[len(p.sent)-1], "25. model") {
		t.Fatal("missing final page")
	}
}
