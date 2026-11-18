package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chenhg5/cc-connect/core"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// P8: commands with "-" were dropped from the menu (agent-upgrade).
func TestBotCommandsRenameHyphenatedCommands(t *testing.T) {
	cmds, aliases := telegramBotCommands([]core.BotCommandInfo{
		{Command: "agent-upgrade", Description: "Upgrade agent CLIs"},
		{Command: "status", Description: "Show status"},
		{Command: "loop", Description: ""},
		{Command: "bad cmd!", Description: "x"},
	})
	var names []string
	for _, c := range cmds {
		names = append(names, c.Command+"="+c.Description)
	}
	want := []string{"agent_upgrade=Upgrade agent CLIs", "status=Show status", "loop=loop"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("menu = %q, want %q", names, want)
	}
	if len(aliases) != 1 || aliases["agent_upgrade"] != "agent-upgrade" {
		t.Fatalf("aliases = %v", aliases)
	}
	long := strings.Repeat("长", 200) // 600 bytes
	cmds, _ = telegramBotCommands([]core.BotCommandInfo{{Command: "x", Description: long}})
	if d := cmds[0].Description; len(d) > 256 || !strings.HasSuffix(d, "...") || !strings.HasPrefix(long, strings.TrimSuffix(d, "...")) {
		t.Fatalf("long description cut badly: %d bytes", len(d))
	}
}

// Review 3: a renamed command must never take over another command's name.
func TestBotCommandsCollisionsDoNotShadow(t *testing.T) {
	// Whatever the order, the command already named deploy_prod keeps the
	// name and deploy-prod is left out.
	for _, order := range [][]string{{"deploy-prod", "deploy_prod"}, {"deploy_prod", "deploy-prod"}} {
		var in []core.BotCommandInfo
		for _, c := range order {
			in = append(in, core.BotCommandInfo{Command: c, Description: c})
		}
		cmds, aliases := telegramBotCommands(in)
		if len(cmds) != 1 || cmds[0].Command != "deploy_prod" || cmds[0].Description != "deploy_prod" || len(aliases) != 0 {
			t.Fatalf("order %v: menu = %v, aliases = %v", order, cmds, aliases)
		}
	}
	// Two renamed commands with the same Telegram name: neither is offered.
	cmds, aliases := telegramBotCommands([]core.BotCommandInfo{
		{Command: "Deploy-Prod", Description: "a"},
		{Command: "deploy-prod", Description: "b"},
		{Command: "status", Description: "s"},
	})
	if len(cmds) != 1 || cmds[0].Command != "status" || len(aliases) != 0 {
		t.Fatalf("menu = %v, aliases = %v", cmds, aliases)
	}
	// The same command listed twice is not a collision.
	cmds, aliases = telegramBotCommands([]core.BotCommandInfo{
		{Command: "agent-upgrade", Description: "first"},
		{Command: "agent-upgrade", Description: "second"},
	})
	if len(cmds) != 1 || cmds[0].Description != "first" || aliases["agent_upgrade"] != "agent-upgrade" {
		t.Fatalf("menu = %v, aliases = %v", cmds, aliases)
	}
}

func TestRestoreCommandMapsMenuNamesBack(t *testing.T) {
	p := &Platform{commandAliases: map[string]string{"agent_upgrade": "agent-upgrade"}}
	cases := map[string]string{
		"/agent_upgrade":                  "/agent-upgrade",
		"/agent_upgrade run all":          "/agent-upgrade run all",
		"/AGENT_UPGRADE status":           "/agent-upgrade status",
		"/agent_upgrade\nnotify on":       "/agent-upgrade\nnotify on",
		"/status":                         "/status",
		"tell me about /agent_upgrade":    "tell me about /agent_upgrade",
		"/agent_upgrades are not a thing": "/agent_upgrades are not a thing",
	}
	for in, want := range cases {
		if got := p.restoreCommand(in); got != want {
			t.Errorf("restoreCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeBotAPI records sendDocument and sendMessage calls.
type fakeBotAPI struct {
	mu           sync.Mutex
	calls        []string
	messages     []string
	failMessages bool
	document     struct {
		chatID, caption, replyTo, allowWithoutReply, filename, content string
	}
}

func newFakeBot(t *testing.T) (*tgbotapi.BotAPI, *fakeBotAPI) {
	t.Helper()
	fake := &fakeBotAPI{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fake.mu.Lock()
		fake.calls = append(fake.calls, method)
		fake.mu.Unlock()
		switch method {
		case "getMe":
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Bot","username":"bot"}}`)
		case "sendDocument":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse upload: %v", err)
			}
			file, header, err := r.FormFile("document")
			if err != nil {
				t.Errorf("no document part: %v", err)
				http.Error(w, "bad", 400)
				return
			}
			data, _ := io.ReadAll(file)
			fake.mu.Lock()
			fake.document.chatID = r.FormValue("chat_id")
			fake.document.caption = r.FormValue("caption")
			fake.document.replyTo = r.FormValue("reply_to_message_id")
			fake.document.allowWithoutReply = r.FormValue("allow_sending_without_reply")
			fake.document.filename = header.Filename
			fake.document.content = string(data)
			fake.mu.Unlock()
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":99,"date":0,"chat":{"id":42,"type":"private"}}}`)
		default: // sendMessage
			fake.mu.Lock()
			fake.messages = append(fake.messages, r.FormValue("text"))
			fail := fake.failMessages
			fake.mu.Unlock()
			if fail {
				_, _ = fmt.Fprint(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"message_id":98,"date":0,"chat":{"id":42,"type":"private"}}}`)
		}
	}))
	t.Cleanup(server.Close)
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return bot, fake
}

// P8: Telegram had no FileSender, so agents could not send files there.
func TestSendFileUploadsDocument(t *testing.T) {
	bot, fake := newFakeBot(t)
	p := &Platform{bot: bot}
	var _ core.FileSender = p

	path := filepath.Join(t.TempDir(), "report.md")
	if err := os.WriteFile(path, []byte("# report"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := replyContext{chatID: 42, messageID: 7, private: true}
	if err := p.SendFile(context.Background(), rc, path, "Here is the report"); err != nil {
		t.Fatal(err)
	}
	d := fake.document
	if d.chatID != "42" || d.caption != "Here is the report" || d.replyTo != "7" || d.allowWithoutReply != "true" ||
		d.filename != "report.md" || d.content != "# report" {
		t.Fatalf("upload = %+v", d)
	}
	if id, ok := p.latestSentMessage(42); !ok || id != 99 {
		t.Fatalf("latest sent = %d %v", id, ok)
	}

	// A caption longer than Telegram allows goes out as its own message first.
	fake.calls = nil
	if err := p.SendFile(context.Background(), rc, path, strings.Repeat("x", 1100)); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.calls, ",") != "sendMessage,sendDocument" || fake.document.caption != "" {
		t.Fatalf("calls = %v, caption = %q", fake.calls, fake.document.caption)
	}

	// Review 3: a caption over Telegram's message limit is split, and the
	// file still goes out.
	fake.calls, fake.messages = nil, nil
	long := strings.Repeat("y", 5000)
	if err := p.SendFile(context.Background(), rc, path, long); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.calls, ",") != "sendMessage,sendMessage,sendDocument" ||
		strings.Join(fake.messages, "") != long || len(fake.messages[0]) != telegramMessageLimit {
		t.Fatalf("calls = %v, message lengths = %d", fake.calls, len(fake.messages))
	}

	// If the caption cannot be sent, the file still is, and the error says so.
	fake.calls = nil
	fake.failMessages = true
	err := p.SendFile(context.Background(), rc, path, long)
	if err == nil || !strings.Contains(err.Error(), "file sent, but its caption was not") {
		t.Fatalf("err = %v", err)
	}
	if fake.calls[len(fake.calls)-1] != "sendDocument" || fake.document.content != "# report" {
		t.Fatalf("calls = %v", fake.calls)
	}
}

func TestSplitTelegramText(t *testing.T) {
	// Emoji outside the BMP count as two UTF-16 units.
	chunks := splitTelegramText(strings.Repeat("😀", 5), 4)
	if strings.Join(chunks, "|") != "😀😀|😀😀|😀" {
		t.Fatalf("chunks = %q", chunks)
	}
	// A line break in the second half of a piece is preferred.
	chunks = splitTelegramText("aaaa\nbbbbbb", 8)
	if strings.Join(chunks, "|") != "aaaa\n|bbbbbb" {
		t.Fatalf("chunks = %q", chunks)
	}
	if got := splitTelegramText("short", 8); len(got) != 1 || got[0] != "short" {
		t.Fatalf("chunks = %q", got)
	}
}

func TestSendFileRejectsBadPaths(t *testing.T) {
	bot, fake := newFakeBot(t)
	p := &Platform{bot: bot}
	rc := replyContext{chatID: 42}
	dir := t.TempDir()
	big := filepath.Join(dir, "big.bin")
	if f, err := os.Create(big); err != nil {
		t.Fatal(err)
	} else {
		_ = f.Truncate(telegramUploadLimit + 1) // sparse
		_ = f.Close()
	}
	for path, want := range map[string]string{
		"relative.txt":                    "absolute",
		dir:                               "directory",
		filepath.Join(dir, "missing.txt"): "no such file",
		big:                               "at most 50 MB",
	} {
		err := p.SendFile(context.Background(), rc, path, "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("SendFile(%s) = %v, want error containing %q", filepath.Base(path), err, want)
		}
	}
	for _, call := range fake.calls {
		if call == "sendDocument" {
			t.Fatal("a rejected file was uploaded")
		}
	}
}
