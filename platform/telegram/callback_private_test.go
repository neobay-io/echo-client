package telegram

import (
	"testing"

	"github.com/chenhg5/cc-connect/core"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// P2 review: a button pressed in a private chat must reach the engine as a
// private-chat message (/auth refuses anything else).
func TestCallbackKeepsPrivateChatFlag(t *testing.T) {
	bot, _ := newFakeBot(t)
	var got []*core.Message
	p := &Platform{bot: bot, handler: func(_ core.Platform, msg *core.Message) { got = append(got, msg) }}
	press := func(chatType string) {
		p.handleCallbackQuery(&tgbotapi.CallbackQuery{
			ID:      "cb",
			From:    &tgbotapi.User{ID: 7, UserName: "admin"},
			Data:    "cmd:/auth cancel",
			Message: &tgbotapi.Message{MessageID: 3, Chat: &tgbotapi.Chat{ID: 42, Type: chatType}, Text: "login"},
		})
	}
	press("private")
	press("group")
	if len(got) != 2 {
		t.Fatalf("messages = %d", len(got))
	}
	if !p.IsPrivateChat(got[0].ReplyCtx) || p.IsPrivateChat(got[1].ReplyCtx) || got[0].Content != "/auth cancel" {
		t.Fatalf("private flags = %v, %v; content %q", p.IsPrivateChat(got[0].ReplyCtx), p.IsPrivateChat(got[1].ReplyCtx), got[0].Content)
	}
}
