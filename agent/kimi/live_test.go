//go:build live

package kimi

// Live checks against the installed Kimi Code CLI and account:
//
//	go test -tags live -run Live -v ./agent/kimi/
//
// They spend a few model turns.

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func liveTurn(t *testing.T, s core.AgentSession, prompt string, images []core.ImageAttachment) (string, core.Event) {
	t.Helper()
	if err := s.Send(prompt, images, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var text strings.Builder
	timeout := time.After(4 * time.Minute)
	for {
		select {
		case evt := <-s.Events():
			switch evt.Type {
			case core.EventText:
				text.WriteString(evt.Content)
			case core.EventError:
				t.Fatalf("turn error: %v", evt.Error)
			case core.EventResult:
				return text.String(), evt
			}
		case <-timeout:
			t.Fatal("turn timed out")
		}
	}
}

func TestLiveKimiTurnsResumeModelSwitchEffortAndImage(t *testing.T) {
	workDir := t.TempDir()
	agent, err := New(map[string]any{"work_dir": workDir, "mode": "bypassPermissions"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := agent.(*Agent)
	t.Logf("kimi binary: %s", a.cmd)

	models := a.AvailableModels(context.Background())
	if len(models) == 0 {
		t.Fatal("no models discovered")
	}
	t.Logf("models: %+v", models)
	t.Logf("efforts: %+v", a.AvailableReasoningLevels())

	// Turn 1: bypass mode writes files.
	s1, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	reply, res := liveTurn(t, s1, "Remember the codeword ORCHID-42. Also create a file named probe.txt containing the word ORCHID. Then reply OK.", nil)
	_ = s1.Close()
	sid := res.SessionID
	t.Logf("turn 1: session=%s reply=%q", sid, reply)
	if sid == "" {
		t.Fatal("no session id")
	}
	if data, err := os.ReadFile(filepath.Join(workDir, "probe.txt")); err != nil || !strings.Contains(string(data), "ORCHID") {
		t.Fatalf("probe.txt not written: %q %v", data, err)
	}

	// Turn 2: resumed in a new process with another model and effort.
	other := models[0].Name
	for _, m := range models {
		if m.Name == "kimi-code/k3" {
			other = m.Name
		}
	}
	a.SetSessionOptions(core.AgentSessionOptions{Model: other, ReasoningLevel: "low"})
	s2, err := a.StartSession(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ = liveTurn(t, s2, "What codeword did I ask you to remember? Reply with the codeword only.", nil)
	t.Logf("turn 2 (%s, low): %q", other, reply)
	if !strings.Contains(reply, "ORCHID-42") {
		t.Fatalf("context lost after model switch: %q", reply)
	}
	wire, _ := os.ReadFile(filepath.Join(findKimiSessionDir(sid), "agents", "main", "wire.jsonl"))
	if !bytes.Contains(wire, []byte(`"modelAlias":"`+other+`","thinkingEffort":"low"`)) &&
		!bytes.Contains(wire, []byte(`"thinkingEffort":"low"`)) {
		t.Fatalf("effort low not used by the LLM request")
	}

	// Turn 3: an image attachment.
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for x := 0; x < 64; x++ {
		for y := 0; y < 64; y++ {
			img.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	_ = png.Encode(&buf, img)
	a.SetSessionOptions(core.AgentSessionOptions{})
	reply, _ = liveTurn(t, s2, "What is the dominant color of the attached image? Reply with one word.",
		[]core.ImageAttachment{{MimeType: "image/png", Data: buf.Bytes(), FileName: "red.png"}})
	_ = s2.Close()
	t.Logf("turn 3 (image): %q", reply)
	if !strings.Contains(strings.ToLower(reply), "red") {
		t.Fatalf("image not understood: %q", reply)
	}

	// Listing and history see the session, without the options hint.
	sessions, err := a.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sessions {
		if s.ID == sid {
			found = true
			t.Logf("listed: %+v", s)
			if strings.HasPrefix(s.Summary, "If you need") {
				t.Fatalf("summary shows the options hint: %q", s.Summary)
			}
		}
	}
	if !found {
		t.Fatalf("session %s not listed: %+v", sid, sessions)
	}
	history, err := a.GetSessionHistory(context.Background(), sid, 0)
	if err != nil || len(history) < 4 {
		t.Fatalf("history = %+v, %v", history, err)
	}
}
