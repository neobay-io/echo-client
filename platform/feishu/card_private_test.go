package feishu

import (
	"testing"

	"github.com/chenhg5/cc-connect/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
)

// P2 review: a card button pressed in a private chat must reach the engine as
// a private-chat message (/auth refuses anything else), and one from a chat
// never seen as p2p must not.
func TestCardActionKeepsPrivateChatFlag(t *testing.T) {
	var got []*core.Message
	p := &Platform{handler: func(_ core.Platform, msg *core.Message) { got = append(got, msg) }}
	press := func(chatID string) {
		_, _ = p.onCardAction(&callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: "ou_admin"},
			Action:   &callback.CallBackAction{Value: map[string]any{"data": "cmd:/auth cancel"}},
			Context:  &callback.Context{OpenChatID: chatID, OpenMessageID: "om_1"},
		}})
	}
	press("oc_private")
	p.privateChats.Store("oc_private", true)
	press("oc_private")
	press("oc_group")
	if len(got) != 3 {
		t.Fatalf("messages = %d", len(got))
	}
	for i, want := range []bool{false, true, false} {
		if private := p.IsPrivateChat(got[i].ReplyCtx); private != want || got[i].Content != "/auth cancel" {
			t.Errorf("press %d: private = %v, content %q; want %v", i, private, got[i].Content, want)
		}
	}
}
