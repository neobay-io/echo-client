package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
)

// agy has no command that lists conversations. It keeps them in SQLite under
// ~/.gemini/antigravity-cli: conversation_summaries.db has one row per
// conversation (title, preview, step count, workspace URIs, times). echo-client
// reads it with the sqlite3 CLI rather than linking a SQLite driver; without
// sqlite3 only the directory's last conversation (cache/last_conversations.json)
// is listed.

const sessionQueryTimeout = 5 * time.Second

var (
	agyHomeDir    = os.UserHomeDir
	sqlite3Lookup = func() (string, error) { return exec.LookPath("sqlite3") }
)

func agyStateDir() string {
	home, err := agyHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli")
}

type agySummaryRow struct {
	ID            string `json:"conversation_id"`
	Title         string `json:"title"`
	Preview       string `json:"preview"`
	StepCount     int    `json:"step_count"`
	LastModified  string `json:"last_modified_time"`
	WorkspaceURIs string `json:"workspace_uris"`
}

func listAgySessions(ctx context.Context, workDir string) ([]core.AgentSessionInfo, error) {
	stateDir := agyStateDir()
	if stateDir == "" {
		return nil, nil
	}
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		absWorkDir = workDir
	}
	dbPath := filepath.Join(stateDir, "conversation_summaries.db")
	if _, err := os.Stat(dbPath); err == nil {
		if sqlite, err := sqlite3Lookup(); err == nil {
			rows, err := querySummaries(ctx, sqlite, dbPath)
			if err == nil {
				return summariesForWorkDir(rows, absWorkDir), nil
			}
			slog.Debug("antigravity: read conversation summaries failed", "error", err)
		}
	}
	return lastConversation(stateDir, absWorkDir), nil
}

// querySummaries reads the summaries table read-only. immutable=1 is needed
// because the database uses WAL and a read-only open would otherwise have to
// create the -shm file; agy checkpoints when it exits, so at most a
// conversation that is running right now is missing or stale.
func querySummaries(ctx context.Context, sqlite, dbPath string) ([]agySummaryRow, error) {
	ctx, cancel := context.WithTimeout(ctx, sessionQueryTimeout)
	defer cancel()
	uri := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&immutable=1"}).String()
	const query = `SELECT conversation_id, title, preview, step_count, last_modified_time, workspace_uris
FROM conversation_summaries
WHERE nesting_depth = 0 AND parent_conversation_id = ''
ORDER BY last_modified_time DESC LIMIT 500`
	out, err := exec.CommandContext(ctx, sqlite, "-readonly", "-json", uri, query).Output()
	if err != nil {
		return nil, fmt.Errorf("sqlite3: %w", err)
	}
	return parseSummaryRows(out)
}

func parseSummaryRows(out []byte) ([]agySummaryRow, error) {
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil // sqlite3 -json prints nothing for zero rows
	}
	var rows []agySummaryRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decode summaries: %w", err)
	}
	return rows, nil
}

func summariesForWorkDir(rows []agySummaryRow, absWorkDir string) []core.AgentSessionInfo {
	var sessions []core.AgentSessionInfo
	for _, row := range rows {
		if row.ID == "" || !inWorkspace(row.WorkspaceURIs, absWorkDir) {
			continue
		}
		summary := strings.TrimSpace(row.Title)
		if summary == "" {
			summary = firstLine(core.StripInteractionOptions(row.Preview))
		}
		if summary == "" {
			summary = row.ID
		}
		if utf8.RuneCountInString(summary) > 60 {
			summary = string([]rune(summary)[:60]) + "..."
		}
		sessions = append(sessions, core.AgentSessionInfo{
			ID:           row.ID,
			Summary:      summary,
			Title:        strings.TrimSpace(row.Title),
			MessageCount: row.StepCount,
			ModifiedAt:   parseSQLiteTime(row.LastModified),
			WorkDir:      absWorkDir,
		})
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt) })
	return sessions
}

// inWorkspace reports whether a JSON array of file:// URIs includes workDir.
func inWorkspace(urisJSON, absWorkDir string) bool {
	var uris []string
	if json.Unmarshal([]byte(urisJSON), &uris) != nil {
		return false
	}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "file" {
			continue
		}
		if sameDir(u.Path, absWorkDir) {
			return true
		}
	}
	return false
}

func lastConversation(stateDir, absWorkDir string) []core.AgentSessionInfo {
	path := filepath.Join(stateDir, "cache", "last_conversations.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var byDir map[string]string
	if json.Unmarshal(data, &byDir) != nil {
		return nil
	}
	modified := time.Time{}
	if st, err := os.Stat(path); err == nil {
		modified = st.ModTime()
	}
	for dir, id := range byDir {
		if id != "" && sameDir(dir, absWorkDir) {
			return []core.AgentSessionInfo{{ID: id, Summary: id, ModifiedAt: modified, WorkDir: absWorkDir}}
		}
	}
	return nil
}

func parseSQLiteTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
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

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
