package qoder

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestSend_PassesAttachmentsToQoderCLI(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-1","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(context.Background(), workDir, nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	err = qs.Send("", []core.ImageAttachment{{
		MimeType: "image/png",
		Data:     []byte("pngdata"),
		FileName: "screen.png",
	}}, []core.FileAttachment{{
		MimeType: "video/mp4",
		Data:     []byte("mp4data"),
		FileName: "clip.mp4",
	}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	var attachments []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--attachment" && i+1 < len(args) {
			attachments = append(attachments, args[i+1])
		}
	}
	if len(attachments) != 2 {
		t.Fatalf("attachments = %#v, want 2 attachment args", attachments)
	}
	for _, attachment := range attachments {
		if !strings.HasPrefix(attachment, filepath.Join(workDir, ".cc-connect")+string(os.PathSeparator)) {
			t.Fatalf("attachment path = %q, want saved path in work dir", attachment)
		}
	}
	if len(args) == 0 || args[len(args)-1] != "Please analyze the attached file(s)." {
		t.Fatalf("args = %#v, want fallback prompt as final positional argument", args)
	}
	if len(args) < 3 || args[0] != "--print" || args[1] != "--output-format" || args[2] != "stream-json" {
		t.Fatalf("args = %#v, want documented print/output-format prefix", args)
	}
}

func TestSend_UsesAddDirForExtraWorkspaces(t *testing.T) {
	workDir := t.TempDir()
	primaryDir := filepath.Join(workDir, "repo")
	if err := os.MkdirAll(primaryDir, 0o755); err != nil {
		t.Fatalf("mkdir primary: %v", err)
	}
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	argsFile := filepath.Join(workDir, "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-2","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(context.Background(), workDir, []string{primaryDir}, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	if err := qs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	var addDirs []string
	var workspaces []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--add-dir" && i+1 < len(args) {
			addDirs = append(addDirs, args[i+1])
		}
		if (args[i] == "-w" || args[i] == "--workspace") && i+1 < len(args) {
			workspaces = append(workspaces, args[i+1])
		}
	}
	if len(addDirs) != 1 || addDirs[0] != primaryDir {
		t.Fatalf("add-dir args = %#v, want [%q]", addDirs, primaryDir)
	}
	if len(workspaces) != 1 || workspaces[0] != workDir {
		t.Fatalf("workspace args = %#v, want [%q]", workspaces, workDir)
	}
}

func TestSend_DeduplicatesNormalizedWorkspacePaths(t *testing.T) {
	workDir := filepath.Join(t.TempDir(), "managed")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir managed: %v", err)
	}
	binDir := filepath.Join(filepath.Dir(workDir), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	argsFile := filepath.Join(filepath.Dir(workDir), "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-3","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(
		context.Background(),
		workDir,
		[]string{workDir + string(os.PathSeparator), workDir},
		"",
		"",
		"",
		nil,
	)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	if err := qs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	var addDirs []string
	var workspaces []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--add-dir" && i+1 < len(args) {
			addDirs = append(addDirs, args[i+1])
		}
		if (args[i] == "-w" || args[i] == "--workspace") && i+1 < len(args) {
			workspaces = append(workspaces, args[i+1])
		}
	}
	if len(addDirs) != 0 {
		t.Fatalf("add-dir args = %#v, want none", addDirs)
	}
	if len(workspaces) != 1 || workspaces[0] != workDir {
		t.Fatalf("workspace args = %#v, want [%q]", workspaces, workDir)
	}
}

func TestSend_PassesModelToQoderCLI(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-model","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(context.Background(), workDir, nil, "ultimate", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	if err := qs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	modelIdx := -1
	for i, arg := range args {
		if arg == "--model" {
			modelIdx = i
			break
		}
	}
	if modelIdx < 0 || modelIdx+1 >= len(args) || args[modelIdx+1] != "ultimate" {
		t.Fatalf("args = %#v, want --model ultimate", args)
	}
}

func TestSend_UsesDocumentedResumeAndPermissionModeSyntax(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	argsFile := filepath.Join(workDir, "args.txt")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$QODER_ARGS_FILE"
echo '{"type":"result","session_id":"qoder-session-plan","done":true,"message":{"content":[{"type":"text","text":"ok"}]}}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}

	t.Setenv("QODER_ARGS_FILE", argsFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(context.Background(), workDir, nil, "", "plan", "resume-1", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	if err := qs.Send("hello", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	timeout := time.After(5 * time.Second)
	for {
		select {
		case evt := <-qs.Events():
			if evt.Type == core.EventError {
				t.Fatalf("unexpected error event: %v", evt.Error)
			}
			if evt.Type == core.EventResult && evt.Done {
				goto done
			}
		case <-timeout:
			t.Fatal("timed out waiting for result")
		}
	}

done:
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(argsBytes)), "\n")
	joined := strings.Join(args, "\n")
	if !strings.Contains(joined, "--resume\nresume-1") {
		t.Fatalf("args = %#v, want --resume resume-1", args)
	}
	if !strings.Contains(joined, "--permission-mode\nplan") {
		t.Fatalf("args = %#v, want --permission-mode plan", args)
	}
	if args[len(args)-1] != "hello" {
		t.Fatalf("args = %#v, want prompt as final positional argument", args)
	}
}

func TestHandleResult_UsesTopLevelResultFallback(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	qs.handleEvent(&streamEvent{
		Type:      "result",
		SessionID: "qoder-session-result",
		Result:    "review output from result field",
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "review output from result field" {
			t.Fatalf("event content = %q, want top-level result text", evt.Content)
		}
		if evt.SessionID != "qoder-session-result" {
			t.Fatalf("event sessionID = %q, want %q", evt.SessionID, "qoder-session-result")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestHandleResult_PrefersMessageContentOverTopLevelResult(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	content, err := json.Marshal([]contentItem{{Type: "text", Text: "message content wins"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	qs.handleEvent(&streamEvent{
		Type:      "result",
		SessionID: "qoder-session-result",
		Result:    "top-level result",
		Message:   &streamMessage{Content: content},
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "message content wins" {
			t.Fatalf("event content = %q, want message content", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestHandleResult_ConcatenatesMultipleTextItems(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	content, err := json.Marshal([]contentItem{
		{Type: "text", Text: "first block"},
		{Type: "function", Name: "Read"},
		{Type: "text", Text: "second block"},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	qs.handleEvent(&streamEvent{
		Type:      "result",
		SessionID: "qoder-session-result",
		Result:    "top-level result",
		Message:   &streamMessage{Content: content},
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "first block\n\nsecond block" {
			t.Fatalf("event content = %q, want concatenated text blocks", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestHandleResult_FallsBackWhenMessageHasNoTextItems(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	content, err := json.Marshal([]contentItem{{Type: "function", Name: "Read"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	qs.handleEvent(&streamEvent{
		Type:      "result",
		SessionID: "qoder-session-result",
		Result:    "fallback result text",
		Message:   &streamMessage{Content: content},
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "fallback result text" {
			t.Fatalf("event content = %q, want fallback top-level result", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestHandleResult_FallsBackToAssistantTextWhenResultIsEmpty(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	content, err := json.Marshal([]contentItem{{Type: "text", Text: "assistant text without finished status"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	qs.handleEvent(&streamEvent{
		Type:      "assistant",
		SessionID: "qoder-session-result",
		Message:   &streamMessage{Role: "assistant", Content: content},
	})
	qs.handleEvent(&streamEvent{
		Type:      "result",
		SessionID: "qoder-session-result",
		Done:      true,
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "assistant text without finished status" {
			t.Fatalf("event content = %q, want assistant text fallback", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestHandleResult_FallbackSkipsInProgressAssistantText(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	partialContent, err := json.Marshal([]contentItem{{Type: "text", Text: "The answer"}})
	if err != nil {
		t.Fatalf("Marshal partial: %v", err)
	}
	finishedContent, err := json.Marshal([]contentItem{{Type: "text", Text: "The answer is 42"}})
	if err != nil {
		t.Fatalf("Marshal finished: %v", err)
	}

	qs.handleEvent(&streamEvent{
		Type:    "assistant",
		Message: &streamMessage{Role: "assistant", Status: "in_progress", Content: partialContent},
	})
	qs.handleEvent(&streamEvent{
		Type:    "assistant",
		Message: &streamMessage{Role: "assistant", Status: "finished", Content: finishedContent},
	})
	qs.handleEvent(&streamEvent{
		Type:      "result",
		SessionID: "qoder-session-result",
		Done:      true,
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventText {
			t.Fatalf("first event type = %v, want text", evt.Type)
		}
		if evt.Content != "The answer is 42" {
			t.Fatalf("text event content = %q, want finished text", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for text event")
	}

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "The answer is 42" {
			t.Fatalf("event content = %q, want only finished assistant text fallback", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestHandleResult_PrefersResultOverAssistantTextFallback(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	content, err := json.Marshal([]contentItem{{Type: "text", Text: "assistant fallback text"}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	qs.handleEvent(&streamEvent{
		Type:    "assistant",
		Message: &streamMessage{Role: "assistant", Content: content},
	})
	qs.handleEvent(&streamEvent{
		Type:      "result",
		SessionID: "qoder-session-result",
		Result:    "top-level result text",
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "top-level result text" {
			t.Fatalf("event content = %q, want top-level result", evt.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestSend_ResetsAssistantTextFallbackBetweenTurns(t *testing.T) {
	workDir := t.TempDir()
	binDir := filepath.Join(workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	script := `#!/bin/sh
echo '{"type":"result","session_id":"qoder-session-reset","done":true}'
`
	scriptPath := filepath.Join(binDir, "qodercli")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qodercli: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	qs, err := newQoderSession(context.Background(), workDir, nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	qs.recordAssistantText("stale assistant text")
	if err := qs.Send("next turn", nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventResult {
			t.Fatalf("event type = %v, want result", evt.Type)
		}
		if evt.Content != "" {
			t.Fatalf("event content = %q, want no stale assistant fallback", evt.Content)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result event")
	}
}

func TestHandleAssistant_EmitsToolUseForToolUseItemsWithoutFinishedStatus(t *testing.T) {
	qs, err := newQoderSession(context.Background(), t.TempDir(), nil, "", "", "", nil)
	if err != nil {
		t.Fatalf("newQoderSession: %v", err)
	}
	defer func() { _ = qs.Close() }()

	content, err := json.Marshal([]map[string]any{
		{
			"type":  "tool_use",
			"name":  "Read",
			"input": map[string]any{"file_path": "/tmp/demo/README.md"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	qs.handleEvent(&streamEvent{
		Type:    "assistant",
		Message: &streamMessage{Content: content},
	})

	select {
	case evt := <-qs.Events():
		if evt.Type != core.EventToolUse {
			t.Fatalf("event type = %v, want tool_use", evt.Type)
		}
		if evt.ToolName != "Read" {
			t.Fatalf("tool name = %q, want %q", evt.ToolName, "Read")
		}
		if evt.ToolInput != "/tmp/demo/README.md" {
			t.Fatalf("tool input = %q, want file path preview", evt.ToolInput)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tool_use event")
	}
}

func TestExtractToolPreview_SupportsLegacyStringifiedJSON(t *testing.T) {
	got := extractToolPreview(json.RawMessage(`"{\"command\":\"git status\"}"`))
	if got != "git status" {
		t.Fatalf("extractToolPreview = %q, want %q", got, "git status")
	}
}
