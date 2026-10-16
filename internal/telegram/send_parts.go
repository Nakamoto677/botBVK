package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"errors"
	"fmt"
)

func (w *Worker) confirmedPart(ctx context.Context, key string, part int, send func() (Message, error)) (Message, error) {
	store, ok := w.store.(domain.SendProgressStore)
	if ok {
		id, err := store.SendPart(ctx, key, part)
		if err == nil {
			return Message{ID: id}, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return Message{}, err
		}
	}
	sent, err := send()
	if err != nil {
		return sent, err
	}
	if ok {
		err = store.SaveSendPart(ctx, key, part, sent.ID)
	}
	return sent, err
}
// textParts sends a message in Telegram-sized chunks and records every confirmed
// part, so a retry after a crash resumes instead of duplicating. An optional
// parse mode is applied to every chunk; engineer cards need HTML, plain text must
// stay unparsed because user content may contain unescaped markup characters.
func (w *Worker) textParts(ctx context.Context, key string, chat, topic int64, text string, markup any, parseMode ...string) (Message, error) {
	parts := splitText(text, messageLimit)
	var sent Message
	for i, part := range parts {
		var err error
		sent, err = w.confirmedPart(ctx, key, i, func() (Message, error) {
			if err := w.rate(ctx, chat, "sendMessage"); err != nil {
				return Message{}, err
			}
			payload := map[string]any{"chat_id": chat, "text": part}
			if topic != 0 {
				payload["message_thread_id"] = topic
			}
			if i == len(parts)-1 && markup != nil {
				payload["reply_markup"] = markup
			}
			if len(parseMode) > 0 && parseMode[0] != "" {
				payload["parse_mode"] = parseMode[0]
			}
			var result Message
			err := w.client.Call(ctx, "sendMessage", payload, &result)
			return result, err
		})
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}
func (w *Worker) operationSendKey(kind string) string {
	return fmt.Sprintf("operation:%d:%s", w.currentOperation, kind)
}
