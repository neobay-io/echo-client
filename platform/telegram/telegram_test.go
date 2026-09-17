package telegram

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPollUpdatesPreservesForwardOriginAndAdvancesOffset(t *testing.T) {
	nextOffset := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = fmt.Fprint(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Bot","username":"bot"}}`)
			return
		}
		if r.FormValue("offset") == "8" {
			select {
			case nextOffset <- struct{}{}:
			default:
			}
			_, _ = fmt.Fprint(w, `{"ok":true,"result":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"result":[{"update_id":7,"message":{"message_id":2,"text":"/auth code secret","forward_origin":{"type":"hidden_user","date":1700000000,"sender_user_name":"Hidden"}}}]}`)
	}))
	defer server.Close()
	bot, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := pollUpdates(ctx, bot, tgbotapi.NewUpdate(0))
	select {
	case update := <-updates:
		msg := &core.Message{}
		applyForwardMetadata(msg, update.Message)
		if !msg.Forwarded || msg.ForwardSource != "Hidden" {
			t.Fatal("polling dropped modern forwarding metadata")
		}
	case <-time.After(time.Second):
		t.Fatal("polling did not deliver update")
	}
	select {
	case <-nextOffset:
	case <-time.After(time.Second):
		t.Fatal("polling did not advance offset")
	}
	cancel()
	select {
	case _, ok := <-updates:
		if ok {
			t.Fatal("duplicate update delivered")
		}
	case <-time.After(time.Second):
		t.Fatal("polling did not stop on cancellation")
	}
}

func TestDecodeUpdatesModernForwardOrigin(t *testing.T) {
	for _, tt := range []struct {
		name, origin, source, chat string
	}{
		{"user", `{"type":"user","date":1700000000,"sender_user":{"id":9,"first_name":"Alice","username":"alice"}}`, "alice", ""},
		{"hidden_user", `{"type":"hidden_user","date":1700000000,"sender_user_name":"Hidden"}`, "Hidden", ""},
		{"chat", `{"type":"chat","date":1700000000,"sender_chat":{"id":-9,"type":"supergroup","title":"Group"}}`, "", "Group"},
		{"channel", `{"type":"channel","date":1700000000,"chat":{"id":-9,"type":"channel","title":"Channel"},"message_id":4}`, "", "Channel"},
		{"unknown", `{"type":"future"}`, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := fmt.Sprintf(`[{"update_id":1,"message":{"message_id":2,"date":1700000001,"chat":{"id":3,"type":"private"},"from":{"id":3,"first_name":"Admin"},"text":"/auth code secret","forward_origin":%s}}]`, tt.origin)
			updates, err := decodeUpdates([]byte(data))
			if err != nil || len(updates) != 1 {
				t.Fatalf("decode: %v", err)
			}
			msg := &core.Message{}
			applyForwardMetadata(msg, updates[0].Message)
			if !msg.Forwarded || msg.ForwardSource != tt.source || msg.ForwardChat != tt.chat {
				t.Fatalf("forward metadata lost: %+v", msg)
			}
		})
	}
	for _, data := range []string{
		`[{"update_id":1,"message":{"message_id":2,"text":"ordinary"}}]`,
		`[{"update_id":1,"message":{"message_id":2,"forward_origin":null}}]`,
		`[{"update_id":1,"callback_query":{"id":"callback"}}]`,
	} {
		updates, err := decodeUpdates([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		msg := &core.Message{}
		applyForwardMetadata(msg, updates[0].Message)
		if msg.Forwarded {
			t.Fatal("ordinary update marked forwarded")
		}
	}
	if _, err := decodeUpdates([]byte(`[{"message":{"forward_origin":"bad"}}]`)); err == nil {
		t.Fatal("invalid origin must not be accepted")
	}
}

func TestDirectedMessageLogsDoNotContainCredentials(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	p := &Platform{bot: &tgbotapi.BotAPI{Self: tgbotapi.User{UserName: "bot"}}}
	for _, text := range []string{"@bot /auth code secret", "/auth code secret", "@other /auth code secret"} {
		p.isDirectedAtBot(&tgbotapi.Message{Text: text, Chat: &tgbotapi.Chat{ID: -1}})
	}
	p.isDirectedAtBot(&tgbotapi.Message{
		Text: "/auth@secret code secret", Chat: &tgbotapi.Chat{ID: -1},
		Entities: []tgbotapi.MessageEntity{{Type: "bot_command", Length: 12}},
	})
	p.isDirectedAtBot(&tgbotapi.Message{
		Text: "@secret /auth code secret", Chat: &tgbotapi.Chat{ID: -1},
		Entities: []tgbotapi.MessageEntity{{Type: "mention", Length: 7}},
	})
	if strings.Contains(logs.String(), "secret") {
		t.Fatal("credential leaked to Telegram debug logs")
	}
}

func TestAuthPrivateChatMetadata(t *testing.T) {
	p := &Platform{}
	if !p.IsPrivateChat(replyContext{private: true}) {
		t.Fatal("explicit private chat rejected")
	}
	for _, ctx := range []any{replyContext{chatID: -123}, replyContext{chatID: 123}, nil, "private"} {
		if p.IsPrivateChat(ctx) {
			t.Fatalf("unverified context accepted: %#v", ctx)
		}
	}
}

func TestExtractQuotedMessage_Text(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 42,
		Text:      "Quoted text",
		From: &tgbotapi.User{
			ID:        123,
			UserName:  "alice",
			FirstName: "Alice",
		},
	}

	quoted := extractQuotedMessage(msg)
	if quoted == nil {
		t.Fatal("expected quoted message")
	} else if quoted.messageID != "42" || quoted.userID != "123" || quoted.userName != "alice" || quoted.content != "Quoted text" {
		t.Fatalf("unexpected quoted message: %#v", quoted)
	}
}

func TestExtractQuotedMessage_UsesCaptionFallback(t *testing.T) {
	msg := &tgbotapi.Message{
		MessageID: 7,
		Caption:   "Image caption",
		From: &tgbotapi.User{
			ID:        456,
			FirstName: "Bob",
			LastName:  "Smith",
		},
	}

	quoted := extractQuotedMessage(msg)
	if quoted == nil {
		t.Fatal("expected quoted message")
	} else {
		if quoted.userName != "Bob Smith" {
			t.Fatalf("userName = %q, want Bob Smith", quoted.userName)
		}
		if quoted.content != "Image caption" {
			t.Fatalf("content = %q, want caption", quoted.content)
		}
	}
}

func TestExtractQuotedMessage_EmptyContent(t *testing.T) {
	msg := &tgbotapi.Message{MessageID: 9}
	if quoted := extractQuotedMessage(msg); quoted != nil {
		t.Fatalf("expected nil quoted message, got %#v", quoted)
	}
}

func TestApplyForwardMetadata(t *testing.T) {
	msg := &tgbotapi.Message{
		ForwardDate: 1710000000,
		ForwardFrom: &tgbotapi.User{
			ID:        321,
			FirstName: "Carol",
			LastName:  "Jones",
		},
		ForwardFromChat: &tgbotapi.Chat{
			Title: "Team Chat",
		},
	}

	dst := &core.Message{}
	applyForwardMetadata(dst, msg)

	if !dst.Forwarded {
		t.Fatal("expected forwarded flag")
	}
	if dst.ForwardSource != "Carol Jones" {
		t.Fatalf("ForwardSource = %q, want Carol Jones", dst.ForwardSource)
	}
	if dst.ForwardChat != "Team Chat" {
		t.Fatalf("ForwardChat = %q, want Team Chat", dst.ForwardChat)
	}
	if got, want := dst.ForwardDate, time.Unix(1710000000, 0); !got.Equal(want) {
		t.Fatalf("ForwardDate = %v, want %v", got, want)
	}
}
