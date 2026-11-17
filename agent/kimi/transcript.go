package kimi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// Kimi Code keeps each session in
// $KIMI_CODE_HOME/sessions/<workspace>/<session id>/ with state.json (cwd,
// title, archived) and the transcript in agents/main/wire.jsonl.

func kimiSessionsDir(extraEnv ...string) string {
	home := kimiCodeHome(extraEnv...)
	if home == "" {
		return ""
	}
	return filepath.Join(home, "sessions")
}

func listKimiSessions(workDir string, extraEnv ...string) ([]core.AgentSessionInfo, error) {
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		absWorkDir = workDir
	}
	base := kimiSessionsDir(extraEnv...)
	if base == "" {
		return nil, nil
	}
	projects, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("kimi: read sessions dir: %w", err)
	}

	var sessions []core.AgentSessionInfo
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(base, project.Name())
		entries, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if info := parseKimiSessionDir(filepath.Join(projectDir, entry.Name()), absWorkDir); info != nil {
				sessions = append(sessions, *info)
			}
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt) })
	return sessions, nil
}

func parseKimiSessionDir(sessionDir, filterWorkDir string) *core.AgentSessionInfo {
	data, err := os.ReadFile(filepath.Join(sessionDir, "state.json"))
	if err != nil {
		return nil
	}
	var state struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Archived  bool   `json:"archived"`
		CWD       string `json:"cwd"`
		WorkDir   string `json:"workDir"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	if json.Unmarshal(data, &state) != nil || state.Archived {
		return nil
	}
	cwd := state.CWD
	if cwd == "" {
		cwd = state.WorkDir
	}
	if filterWorkDir != "" {
		if cwd == "" {
			return nil
		}
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
		if !sameDir(cwd, filterWorkDir) {
			return nil
		}
	}

	id := state.ID
	if id == "" {
		id = filepath.Base(sessionDir)
	}
	modified := time.UnixMilli(state.UpdatedAt)
	if state.UpdatedAt == 0 {
		st, err := os.Stat(sessionDir)
		if err != nil {
			return nil
		}
		modified = st.ModTime()
	}

	count, summary := countWireTranscript(filepath.Join(sessionDir, "agents", "main", "wire.jsonl"))
	if summary == "" {
		summary = state.Title
	}
	if utf8.RuneCountInString(summary) > 60 {
		summary = string([]rune(summary)[:60]) + "..."
	}
	return &core.AgentSessionInfo{
		ID:           id,
		Summary:      summary,
		Title:        state.Title,
		MessageCount: count,
		ModifiedAt:   modified,
		WorkDir:      cwd,
	}
}

// sameDir compares two absolute paths, also after resolving symlinks (macOS
// reports /tmp as /private/tmp in some places and not in others).
func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

func findKimiSessionDir(sessionID string, extraEnv ...string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) || sessionID == "." || sessionID == ".." {
		return ""
	}
	base := kimiSessionsDir(extraEnv...)
	if base == "" {
		return ""
	}
	projects, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		candidate := filepath.Join(base, project.Name(), sessionID)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	return ""
}

func readSessionHistory(sessionID string, limit int, extraEnv ...string) ([]core.HistoryEntry, error) {
	sessionDir := findKimiSessionDir(sessionID, extraEnv...)
	if sessionDir == "" {
		return nil, fmt.Errorf("kimi: session not found: %s", sessionID)
	}
	f, err := os.Open(filepath.Join(sessionDir, "agents", "main", "wire.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("kimi: read session history: %w", err)
	}
	defer func() { _ = f.Close() }()
	entries, err := readWireHistory(f)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// wireRecord covers the wire.jsonl records history needs. Current Kimi Code
// puts origin inside message and content as an array of parts; older 2.x
// builds used a top-level origin and string content.
type wireRecord struct {
	Type    string          `json:"type"`
	Time    json.RawMessage `json:"time"`
	Origin  json.RawMessage `json:"origin"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Origin  json.RawMessage `json:"origin"`
	} `json:"message"`
	Event struct {
		Type   string `json:"type"`
		TurnID string `json:"turnId"`
		Part   struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"part"`
	} `json:"event"`
}

func newWireScanner(f *os.File) *bufio.Scanner {
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 16*1024*1024)
	return scanner
}

// countWireTranscript counts the user's turns and returns the first one as a
// summary, without materializing assistant output.
func countWireTranscript(path string) (count int, summary string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ""
	}
	defer func() { _ = f.Close() }()
	scanner := newWireScanner(f)
	for scanner.Scan() {
		var rec wireRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		if rec.Type != "context.append_message" || rec.Message.Role != "user" || !isUserOrigin(rec.Origin, rec.Message.Origin) {
			continue
		}
		text := extractText(rec.Message.Content)
		if text == "" {
			continue
		}
		count++
		if summary == "" {
			summary = firstLine(text)
		}
	}
	return count, summary
}

func readWireHistory(f *os.File) ([]core.HistoryEntry, error) {
	var entries []core.HistoryEntry
	var turnID string
	var assistant strings.Builder
	var assistantAt time.Time
	flush := func() {
		if text := strings.TrimSpace(assistant.String()); text != "" {
			entries = append(entries, core.HistoryEntry{Role: "assistant", Content: text, Timestamp: assistantAt})
		}
		turnID, assistantAt = "", time.Time{}
		assistant.Reset()
	}

	scanner := newWireScanner(f)
	for scanner.Scan() {
		var rec wireRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		switch rec.Type {
		case "context.append_message":
			role := rec.Message.Role
			if role != "user" && role != "assistant" {
				continue
			}
			if role == "user" && !isUserOrigin(rec.Origin, rec.Message.Origin) {
				continue
			}
			text := extractText(rec.Message.Content)
			if text == "" {
				continue
			}
			flush()
			entries = append(entries, core.HistoryEntry{Role: role, Content: text, Timestamp: parseWireTime(rec.Time)})
		case "context.append_loop_event":
			if rec.Event.Type != "content.part" || rec.Event.Part.Type != "text" || rec.Event.Part.Text == "" {
				continue
			}
			if assistant.Len() > 0 && rec.Event.TurnID != turnID {
				flush()
			}
			if assistant.Len() == 0 {
				turnID, assistantAt = rec.Event.TurnID, parseWireTime(rec.Time)
			}
			assistant.WriteString(rec.Event.Part.Text)
		}
	}
	flush()
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("kimi: scan wire.jsonl: %w", err)
	}
	return entries, nil
}

// isUserOrigin skips user-role messages Kimi injects itself (reminders,
// mode notices). Records without an origin predate the field and count as
// the user's.
func isUserOrigin(topLevel, nested json.RawMessage) bool {
	origin := nested
	if len(origin) == 0 || string(origin) == "null" {
		origin = topLevel
	}
	if len(origin) == 0 || string(origin) == "null" {
		return true
	}
	var value struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(origin, &value) != nil {
		return false
	}
	switch value.Kind {
	case "user", "user-slash", "user_slash":
		return true
	}
	return false
}

// extractText returns a message's text, without the options hint echo-client
// prepends to user prompts.
func extractText(raw json.RawMessage) string {
	return strings.TrimSpace(core.StripInteractionOptions(extractRawText(raw)))
}

func extractRawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, part := range parts {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(texts, "\n")
}

func parseWireTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var millis int64
	if json.Unmarshal(raw, &millis) == nil {
		return time.UnixMilli(millis)
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
