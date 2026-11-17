package antigravity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestSummariesFilterByWorkspace(t *testing.T) {
	workDir := t.TempDir()
	rows, err := parseSummaryRows([]byte(fmt.Sprintf(`[
	{"conversation_id":"c1","title":"","preview":%q,"step_count":4,"last_modified_time":"2026-11-17 07:42:32.631994+00:00","workspace_uris":"[\"file://%s\"]"},
	{"conversation_id":"c2","title":"Named","preview":"x","step_count":2,"last_modified_time":"2026-11-17 08:00:00+00:00","workspace_uris":"[\"file://%s\"]"},
	{"conversation_id":"c3","title":"elsewhere","preview":"x","step_count":2,"last_modified_time":"2026-11-17 09:00:00+00:00","workspace_uris":"[\"file:///other\"]"}
	]`, core.AppendInteractionOptions("fix the build\nmore")[:40], workDir, workDir)))
	if err != nil {
		t.Fatal(err)
	}
	sessions := summariesForWorkDir(rows, workDir)
	if len(sessions) != 2 || sessions[0].ID != "c2" || sessions[1].ID != "c1" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if sessions[1].Summary != "fix the build" || sessions[1].MessageCount != 4 || sessions[1].ModifiedAt.IsZero() {
		t.Fatalf("c1 = %+v", sessions[1])
	}
	if empty, err := parseSummaryRows(nil); err != nil || empty != nil {
		t.Fatalf("zero rows: %v %v", empty, err)
	}
}

func TestListSessionsReadsSummariesDatabase(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 not installed")
	}
	home := t.TempDir()
	workDir := t.TempDir()
	agyHomeDir = func() (string, error) { return home, nil }
	defer func() { agyHomeDir = os.UserHomeDir }()
	stateDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(stateDir, "conversation_summaries.db")
	schema := fmt.Sprintf(`PRAGMA journal_mode=WAL;
CREATE TABLE conversation_summaries (conversation_id text, title text NOT NULL DEFAULT "", preview text NOT NULL DEFAULT "", step_count integer NOT NULL DEFAULT 0, last_modified_time datetime NOT NULL, workspace_uris text NOT NULL, nesting_depth integer NOT NULL DEFAULT 0, parent_conversation_id text NOT NULL DEFAULT "", PRIMARY KEY (conversation_id));
INSERT INTO conversation_summaries VALUES ('top','', 'hello', 2, '2026-11-17 07:42:32.6+00:00', '["file://%[1]s"]', 0, '');
INSERT INTO conversation_summaries VALUES ('child','', 'sub', 1, '2026-11-17 07:43:00+00:00', '["file://%[1]s"]', 1, 'top');`, workDir)
	if out, err := exec.Command(sqlite, db, schema).CombinedOutput(); err != nil {
		t.Fatalf("create db: %v %s", err, out)
	}
	sessions, err := listAgySessions(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "top" || sessions[0].Summary != "hello" {
		t.Fatalf("sessions = %+v", sessions)
	}
}

func TestListSessionsFallsBackToLastConversation(t *testing.T) {
	home := t.TempDir()
	workDir := t.TempDir()
	agyHomeDir = func() (string, error) { return home, nil }
	sqlite3Lookup = func() (string, error) { return "", exec.ErrNotFound }
	defer func() {
		agyHomeDir = os.UserHomeDir
		sqlite3Lookup = func() (string, error) { return exec.LookPath("sqlite3") }
	}()
	cache := filepath.Join(home, ".gemini", "antigravity-cli", "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "antigravity-cli", "conversation_summaries.db"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{%q:"conv-last","/other":"conv-other"}`, workDir)
	if err := os.WriteFile(filepath.Join(cache, "last_conversations.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions, err := listAgySessions(context.Background(), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "conv-last" {
		t.Fatalf("sessions = %+v", sessions)
	}
}
