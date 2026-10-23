package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

func topicCardNotEditable(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Code == 400 && strings.Contains(strings.ToLower(api.Description), "message can't be edited")
}

// Older versions sent a topic card with ReplyKeyboardRemove. Telegram makes
// those messages permanently uneditable. Replace only that card, retaining the
// topic and original message/history. A key based on the old message survives
// retries of both create_topic and refresh_card, including failed DB writes.
func (w *Worker) replaceTopicCard(ctx context.Context, t domain.Ticket, text string, buttons [][]Button) (domain.Ticket, error) {
	team, ok := w.cfg.Departments[t.Department]
	if !ok || t.IsGroup() || t.TopicID == 0 || t.TopicChatID != team.ChatID || t.TopicID == team.QueueTopicID {
		return t, domain.ErrConflict
	}
	matches := func(current domain.Ticket) bool {
		return current.TopicChatID == t.TopicChatID && current.TopicID == t.TopicID && current.TopicMessageID == t.TopicMessageID && current.Department == t.Department && current.RoutingGeneration == t.RoutingGeneration && current.Status == t.Status && current.RoutingPending == t.RoutingPending && current.AssigneeID == t.AssigneeID
	}
	current, err := w.store.Ticket(ctx, t.ID)
	if err != nil {
		return t, err
	}
	if !matches(current) {
		return t, domain.ErrConflict
	}
	// Send progress uses the connection pool itself. Do not hold a transaction
	// while sending or recording it; recheck the binding under lock afterwards.
	key := fmt.Sprintf("topic-card-replacement:%d:%d:%d:%d", t.ID, t.TopicChatID, t.TopicID, t.TopicMessageID)
	sent, err := w.textParts(ctx, key, t.TopicChatID, t.TopicID, text, Markup{Buttons: buttons}, "HTML")
	if err != nil {
		return t, err
	}
	err = w.store.Transact(ctx, func(tx domain.Tx) error {
		current, err := tx.GetTicket(ctx, t.ID)
		if err != nil {
			return err
		}
		if !matches(current) {
			return domain.ErrConflict
		}
		current.TopicMessageID = sent.ID
		if err = tx.SaveTicket(ctx, &current); err != nil {
			return err
		}
		data, _ := json.Marshal(map[string]any{"source_message_id": 0, "telegram_chat_id": t.TopicChatID, "telegram_topic_id": t.TopicID, "telegram_message_id": sent.ID, "direction": "inbound", "initial_description": true, "replaced_message_id": t.TopicMessageID})
		if err = tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: t.ClientID, Kind: "message_delivered", Data: data, CreatedAt: time.Now().UTC()}); err != nil {
			return err
		}
		t = current
		return nil
	})
	return t, err
}
