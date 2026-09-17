package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// The pinned SDK predates forward_origin. Decode that metadata before it is
// discarded, then project it onto the legacy fields consumed by our adapter.
type messageOrigin struct {
	Date           int            `json:"date"`
	SenderUser     *tgbotapi.User `json:"sender_user"`
	SenderUserName string         `json:"sender_user_name"`
	SenderChat     *tgbotapi.Chat `json:"sender_chat"`
	Chat           *tgbotapi.Chat `json:"chat"`
}

func decodeUpdates(data json.RawMessage) ([]tgbotapi.Update, error) {
	var updates []tgbotapi.Update
	if err := json.Unmarshal(data, &updates); err != nil {
		return nil, err
	}
	var metadata []struct {
		Message *struct {
			Origin *messageOrigin `json:"forward_origin"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	for i, item := range metadata {
		if item.Message == nil || item.Message.Origin == nil || updates[i].Message == nil {
			continue
		}
		origin, msg := item.Message.Origin, updates[i].Message
		msg.ForwardDate = origin.Date
		if msg.ForwardDate == 0 {
			// Presence alone must fail closed even if the date is missing.
			msg.ForwardDate = -1
		}
		msg.ForwardFrom = origin.SenderUser
		msg.ForwardSenderName = origin.SenderUserName
		msg.ForwardFromChat = origin.SenderChat
		if origin.Chat != nil {
			msg.ForwardFromChat = origin.Chat
		}
	}
	return updates, nil
}

func pollUpdates(ctx context.Context, bot *tgbotapi.BotAPI, cfg tgbotapi.UpdateConfig) <-chan tgbotapi.Update {
	ch := make(chan tgbotapi.Update, bot.Buffer)
	go func() {
		defer close(ch)
		for ctx.Err() == nil {
			response, err := bot.Request(cfg)
			var updates []tgbotapi.Update
			if err == nil {
				updates, err = decodeUpdates(response.Result)
			}
			if err != nil {
				// SDK errors can contain credentials or response bodies.
				slog.Warn("telegram: failed to get updates; retrying")
				timer := time.NewTimer(3 * time.Second)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				continue
			}
			for _, update := range updates {
				if update.UpdateID < cfg.Offset {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case ch <- update:
					cfg.Offset = update.UpdateID + 1
				}
			}
		}
	}()
	return ch
}
