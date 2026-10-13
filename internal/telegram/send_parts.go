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
func (w *Worker) textParts(ctx context.Context, key string, chat, topic int64, text string, markup any) (Message, error) {
	parts := splitText(text, messageLimit)
	var sent Message
	for i, part := range parts {
		var err error
		sent, err = w.confirmedPart(ctx, key, i, func() (Message, error) {
			if err := w.rate(ctx, chat); err != nil {
				return Message{}, err
			}
			payload := map[string]any{"chat_id": chat, "text": part}
			if topic != 0 {
				payload["message_thread_id"] = topic
			}
			if i == len(parts)-1 && markup != nil {
				payload["reply_markup"] = markup
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
