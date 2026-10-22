package telegram

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"

	"bot-bvk/internal/domain"
)

// A new ticket used to reach the queue as a text card plus a separate
// "Дополнений от клиента" line, so a photo attached in the form stayed invisible
// until an engineer claimed the ticket. The queue card now carries the first
// client attachment itself: sendPhoto/sendVideo/sendDocument with the usual card
// as a caption. Documents are included because Telegram clients often upload a
// photo "as a file". Edits use editMessageCaption, and a card created before the
// photo arrived is upgraded in place with editMessageMedia.

// queueCaptionReserve leaves room for the "Передано в отдел: …" suffix that the
// source department appends when it retires its queue card.
const queueCaptionReserve = 64

var queueCardMediaMethods = map[string]string{
	"photo":    "sendPhoto",
	"video":    "sendVideo",
	"document": "sendDocument",
}

func queueCardMediaMethod(mediaType string) string {
	return queueCardMediaMethods[mediaType]
}

// queueCardAttachment returns the first client attachment that can carry the
// whole card. The order is the stored message order, so the picture the client
// attached in the form wins over a later addition.
func (w *Worker) queueCardAttachment(ctx context.Context, t domain.Ticket) (domain.Message, bool, error) {
	messages, err := w.store.Messages(ctx, t.ID)
	if err != nil {
		return domain.Message{}, false, err
	}
	for _, m := range messages {
		if m.Direction != "inbound" || m.FileID == "" {
			continue
		}
		if queueCardMediaMethod(m.MediaType) == "" {
			continue
		}
		return m, true, nil
	}
	return domain.Message{}, false, nil
}

func (w *Worker) sendQueueCardMedia(ctx context.Context, chat, topic int64, media domain.Message, caption string, buttons [][]Button) (Message, error) {
	method := queueCardMediaMethod(media.MediaType)
	if method == "" {
		return Message{}, domain.ErrInvalid
	}
	send := func() (Message, error) {
		if err := w.rate(ctx, chat, method); err != nil {
			return Message{}, err
		}
		payload := map[string]any{
			"chat_id":       chat,
			media.MediaType: media.FileID,
			"caption":       caption,
			"parse_mode":    "HTML",
			"reply_markup":  Markup{Buttons: buttons},
		}
		if topic != 0 {
			payload["message_thread_id"] = topic
		}
		var result Message
		err := w.client.Call(ctx, method, payload, &result)
		return result, err
	}
	if w.currentOperation == 0 {
		return send()
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s:%s:%s", chat, topic, media.FileID, media.MediaType, caption)))
	return w.confirmedPart(ctx, fmt.Sprintf("operation:%d:queue_media:%x", w.currentOperation, digest), 0, send)
}

func (w *Worker) editQueueCardCaption(ctx context.Context, chat, messageID int64, caption string, buttons [][]Button) error {
	if err := w.rate(ctx, chat, "editMessageCaption"); err != nil {
		return err
	}
	return messageEditError(w.client.Call(ctx, "editMessageCaption", map[string]any{
		"chat_id":      chat,
		"message_id":   messageID,
		"caption":      caption,
		"parse_mode":   "HTML",
		"reply_markup": Markup{Buttons: buttons},
	}, nil))
}

// editQueueCardMedia upgrades a text card that was sent before the client added
// an attachment. Bot API allows replacing a bot message with a media message.
func (w *Worker) editQueueCardMedia(ctx context.Context, chat, messageID int64, media domain.Message, caption string, buttons [][]Button) error {
	if queueCardMediaMethod(media.MediaType) == "" {
		return domain.ErrInvalid
	}
	if err := w.rate(ctx, chat, "editMessageMedia"); err != nil {
		return err
	}
	return messageEditError(w.client.Call(ctx, "editMessageMedia", map[string]any{
		"chat_id":    chat,
		"message_id": messageID,
		"media": map[string]any{
			"type":       media.MediaType,
			"media":      media.FileID,
			"caption":    caption,
			"parse_mode": "HTML",
		},
		"reply_markup": Markup{Buttons: buttons},
	}, nil))
}

// queueMediaFallback reports whether a rejected media card should degrade to the
// text card. 429 and transport failures must be retried, because the media may
// still be delivered; 400 means Telegram will never accept this file or caption.
func queueMediaFallback(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Code == 400 && !api.Ambiguous
}

func (w *Worker) saveQueueCard(ctx context.Context, t domain.Ticket, messageID int64, mediaType string) error {
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		now, err := tx.GetTicket(ctx, t.ID)
		if err != nil {
			return err
		}
		if now.TopicID != t.TopicID || now.Department != t.Department {
			return nil
		}
		now.QueueMessageID = messageID
		now.QueueMediaType = mediaType
		return tx.SaveTicket(ctx, &now)
	})
}

func (w *Worker) queueMediaFallbackLog(t domain.Ticket, err error) {
	log.Printf("queue card media unavailable for ticket %d: %v; sending a text card", t.ID, err)
}
