package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

// Card refresh, message delivery, archival and notifications live here so that
// Handle stays a readable dispatcher. Each handler gets the ticket that Handle
// already loaded and never reassigns it.

func (w *Worker) retireQueueCard(ctx context.Context, payload operationPayload, t domain.Ticket) error {
	chat := payload.ChatID
	if chat == 0 {
		chat = w.cfg.Departments[payload.FromDepartment].ChatID
	}
	if chat == 0 || payload.MessageID == 0 {
		return nil
	}
	note := "\nПередано в отдел: " + departmentName(payload.ToDepartment)
	if payload.QueueMediaType != "" {
		media, hasMedia, e := w.queueCardAttachment(ctx, t)
		if e != nil {
			return e
		}
		mediaID := int64(0)
		if hasMedia {
			mediaID = media.ID
		}
		caption, e := w.cardCaption(ctx, t, mediaID)
		if e != nil {
			return e
		}
		return w.editQueueCardCaption(ctx, chat, payload.MessageID, caption+note, [][]Button{})
	}
	text, err := w.card(ctx, t)
	if err != nil {
		return err
	}
	if err = w.rate(ctx, chat, "editMessageText"); err != nil {
		return err
	}
	return messageEditError(w.client.Call(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": payload.MessageID, "text": text + note, "parse_mode": "HTML", "reply_markup": Markup{Buttons: [][]Button{}}}, nil))
}

func (w *Worker) teamNotification(ctx context.Context, payload operationPayload, team Team) error {
	_, err := w.send(ctx, team.ChatID, team.QueueTopicID, payload.Text, nil)
	return err
}

func (w *Worker) urgentAssignmentReminder(ctx context.Context, t domain.Ticket, payload operationPayload, team Team) error {
	if t.Urgency != domain.Urgent || t.Status != domain.New || t.AssigneeID != 0 {
		return nil
	}
	baseline := t.CreatedAt
	events, err := w.store.Events(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if (ev.Kind == "transferred" || ev.Kind == "reopened") && ev.CreatedAt.After(baseline) {
			baseline = ev.CreatedAt
		}
	}
	if baseline.UTC().Format(time.RFC3339Nano) != payload.Baseline {
		return nil
	}
	if t.QueueMessageID == 0 {
		return domain.ErrConflict
	}
	buttons := [][]Button{}
	if strings.HasPrefix(fmt.Sprint(team.ChatID), "-100") {
		url := fmt.Sprintf("https://t.me/c/%s/%d", strings.TrimPrefix(fmt.Sprint(team.ChatID), "-100"), t.QueueMessageID)
		buttons = [][]Button{{{Text: "Открыть заявку", URL: url}}}
	}
	_, err = w.send(ctx, team.ChatID, team.QueueTopicID, payload.Text, buttons)
	return err
}

func (w *Worker) archiveMessage(ctx context.Context, op domain.Operation, t domain.Ticket) error {
	var msg domain.Message
	if err := json.Unmarshal(op.Payload, &msg); err != nil {
		return domain.ErrInvalid
	}
	return w.archive(ctx, t, msg)
}

func (w *Worker) relayMessage(ctx context.Context, op domain.Operation, t domain.Ticket) error {
	if t.IsGroup() {
		// Company-chat messages are already visible to both sides. A stale
		// relay operation must never repost them.
		return nil
	}
	var msg domain.Message
	if err := json.Unmarshal(op.Payload, &msg); err != nil {
		return domain.ErrInvalid
	}
	if op.Kind == "client_message" {
		events, err := w.store.Events(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Kind != "message_delivered" {
				continue
			}
			var d struct {
				SourceID int64      `json:"source_message_id"`
				EditedAt *time.Time `json:"edited_at"`
			}
			if json.Unmarshal(event.Data, &d) == nil && d.SourceID == msg.ID && d.EditedAt != nil && (msg.EditedAt == nil || d.EditedAt.After(*msg.EditedAt)) {
				return nil
			}
		}
	}
	if msg.FileID != "" {
		if err := w.archive(ctx, t, msg); err != nil {
			return err
		}
	}
	chat, topic := t.ClientID, int64(0)
	if op.Kind == "team_message" {
		var err error
		chat, topic, err = w.messageRoute(ctx, t, msg)
		if err != nil {
			return err
		}
		if topic == 0 {
			return fmt.Errorf("topic pending")
		}
	}
	sent, e := w.deliver(ctx, chat, topic, msg, op.Kind == "team_message")
	if e != nil {
		return e
	}
	// How long a message took from being written to reaching the other side. This is
	// the user-visible number behind "the bot is slow to pass messages on".
	if !msg.CreatedAt.IsZero() && w.client != nil && w.client.Metrics != nil {
		w.client.Metrics.ObserveMilliseconds("bvk_ticket_relay", op.Kind, time.Since(msg.CreatedAt))
	}
	data, _ := json.Marshal(map[string]any{"source_message_id": msg.ID, "telegram_chat_id": chat, "telegram_message_id": sent.ID, "telegram_topic_id": topic, "direction": msg.Direction, "edited_at": msg.EditedAt})
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		if op.Kind == "client_message" {
			current, e := tx.GetTicket(ctx, t.ID)
			if e != nil {
				return e
			}
			if current.FirstResponseAt == nil {
				now := time.Now().UTC()
				current.FirstResponseAt = &now
				if e = tx.SaveTicket(ctx, &current); e != nil {
					return e
				}
				if e = tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: msg.AuthorID, Kind: "first_response", CreatedAt: now, Data: json.RawMessage(`{}`)}); e != nil {
					return e
				}
			}
		}
		return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: msg.AuthorID, Kind: "message_delivered", Data: data, CreatedAt: time.Now().UTC()})
	})
}
