//go:build live

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// A message steered into a running Claude turn is answered: in the turn's
// result when Claude takes it at a step, else as output of a turn Claude
// starts after the result. Either way the event stream carries exactly one
// result, and the original answer is not replaced.
func TestSteerProbeClaude(t *testing.T) {
	agent, err := core.CreateAgent("claudecode", map[string]any{"work_dir": t.TempDir(), "mode": "bypassPermissions"})
	if err != nil {
		t.Skip(err)
	}
	s, err := agent.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	background := make(chan string, 4)
	s.(core.BackgroundReporter).OnBackgroundOutput(func(out core.BackgroundOutput) { background <- out.Text })
	if err := s.Send("Write the numbers from 1 to 40 as words, one per line. Do not use tools.", nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second)
	if err := s.(core.Steerer).Steer("Also say the word BANANA.", nil, nil); err != nil {
		t.Fatal(err)
	}
	var results []string
	var extra []string
	deadline := time.After(90 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			switch ev.Type {
			case core.EventResult:
				results = append(results, ev.Content)
			case core.EventBackground:
				extra = append(extra, ev.Content)
			}
		case text := <-background:
			extra = append(extra, text)
		case <-deadline:
			t.Logf("results %d, background %d", len(results), len(extra))
			if len(results) != 1 {
				t.Fatalf("results: %q, want one", results)
			}
			if !strings.Contains(strings.ToLower(results[0]), "forty") {
				t.Errorf("the turn's answer lost the numbers: %q", results[0])
			}
			if all := results[0] + strings.Join(extra, " "); !strings.Contains(all, "BANANA") {
				t.Errorf("the steered message was not answered: %q / %q", results, extra)
			}
			return
		}
	}
}
