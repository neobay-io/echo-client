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
		{Command: "agent_upgrade", Description: "duplicate after renaming"},
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
	mu       sync.Mutex
	calls    []string
	document struct {
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
