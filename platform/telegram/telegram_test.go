package telegram

import (
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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
