package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"bot-bvk/internal/domain"
)

// operationPayload is the shared shape of an outbox operation payload. Fields are
// decoded once in Handle, which keeps every handler free of local struct copies.
type operationPayload struct {
	Text              string            `json:"text"`
	UserID            int64             `json:"user_id"`
	TicketID          int64             `json:"ticket_id"`
	ChatID            int64             `json:"chat_id"`
	TopicID           int64             `json:"topic_id"`
	OldChatID         int64             `json:"old_chat_id"`
	OldTopicID        int64             `json:"old_topic_id"`
	Reason            string            `json:"reason"`
	Pause             bool              `json:"pause"`
	Retire            bool              `json:"retire"`
	MessageID         int64             `json:"message_id"`
	FromDepartment    domain.Department `json:"from_department"`
	ToDepartment      domain.Department `json:"to_department"`
	RoutingGeneration int64             `json:"routing_generation"`
	CompletionKey     string            `json:"completion_key"`
}

// Topic lifecycle lives here so that creating, closing, reopening and deleting a
// topic follow one readable path each instead of four inline branches. Every
// handler receives the ticket that Handle already loaded.
//
// A ticket binding is immutable for the duration of an operation: the handlers
// only reassign their local copy after a successful write, so a concurrent
// transfer cannot be overwritten by stale data.

func (w *Worker) createTopic(ctx context.Context, op domain.Operation, payload operationPayload, t domain.Ticket, team Team) error {
	var err error
	if payload.RoutingGeneration != 0 && payload.RoutingGeneration != t.RoutingGeneration {
		return nil
	}
	// Old queued operations must not create a topic before an engineer claims the ticket.
	if t.Status == domain.New {
		return w.refresh(ctx, t)
	}
	if !provisioningStatus(t.Status) {
		return nil
	}
	// A binding matching this department makes a retried operation safe after a crash.
	if t.TopicID != 0 && t.TopicChatID == team.ChatID {
		t, err = w.ensureTopicCard(ctx, t)
		if err != nil {
			return err
		}
		if payload.OldChatID != 0 && payload.OldTopicID != 0 {
			if err = w.client.CloseTopic(ctx, payload.OldChatID, payload.OldTopicID); err != nil {
				return err
			}
			if err = w.retire(ctx, t.ID, payload.OldChatID, payload.OldTopicID); err != nil {
				return err
			}
		}
		if err = w.replayDeferred(ctx, t); err != nil {
			return err
		}
		t, err = w.finishProvision(ctx, t)
		if err != nil {
			return err
		}
		return w.refresh(ctx, t)
	}
	if !t.Status.Active() {
		return nil
	}
	if err = w.rate(ctx, team.ChatID); err != nil {
		return err
	}
	name, err := displayName(ctx, w.store, t.ClientID, "Клиент")
	if err != nil {
		return err
	}
	company, err := companyName(ctx, w.store, t.CompanyID)
	if err != nil {
		return err
	}
	title := []rune(fmt.Sprintf("#%d · %s · %s", t.ID, name, company))
	if len(title) > 120 {
		title = title[:120]
	}
	topic, err := w.client.Topic(ctx, team.ChatID, string(title))
	if err != nil {
		return err
	}
	bound := false
	err = w.store.Transact(ctx, func(tx domain.Tx) error {
		current, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if current.Department != t.Department || !provisioningStatus(current.Status) || !current.RoutingPending || current.AssigneeID != t.AssigneeID || current.RoutingGeneration != t.RoutingGeneration || current.TopicID != t.TopicID || current.TopicChatID != t.TopicChatID {
			return domain.ErrConflict
		}
		current.TopicChatID = team.ChatID
		current.TopicID = topic
		current.TopicMessageID = 0
		if e = tx.SaveTicket(ctx, &current); e != nil {
			return e
		}
		t = current
		bound = true
		return nil
	})
	if err != nil {
		_ = w.client.CloseTopic(ctx, team.ChatID, topic)
		return err
	}
	if !bound {
		return domain.ErrConflict
	}
	t, err = w.ensureTopicCard(ctx, t)
	if err != nil {
		return err
	}
	if payload.Reason != "" {
		if _, err = w.send(ctx, t.TopicChatID, t.TopicID, "Причина передачи: "+payload.Reason, nil); err != nil {
			return err
		}
	}
	if payload.OldChatID != 0 && payload.OldTopicID != 0 {
		if err = w.client.CloseTopic(ctx, payload.OldChatID, payload.OldTopicID); err != nil {
			return err
		}
		if err = w.retire(ctx, t.ID, payload.OldChatID, payload.OldTopicID); err != nil {
			return err
		}
	}
	if err = w.replayDeferred(ctx, t); err != nil {
		return err
	}
	t, err = w.finishProvision(ctx, t)
	if err != nil {
		return err
	}
	return w.refresh(ctx, t)
}

func (w *Worker) closeTopic(ctx context.Context, op domain.Operation, payload operationPayload, t domain.Ticket) error {
	var err error
	if payload.ChatID == 0 {
		payload.ChatID = t.TopicChatID
	}
	if payload.TopicID == 0 {
		payload.TopicID = t.TopicID
	}
	if payload.TopicID == 0 {
		return nil
	}
	if payload.ChatID == t.TopicChatID && payload.TopicID == t.TopicID && t.Status.Active() && t.Status != domain.New {
		return nil
	}
	notice := payload.Text
	if payload.Retire && payload.ToDepartment.Valid() {
		notice = "Передано в отдел " + departmentName(payload.ToDepartment)
	}
	if notice != "" {
		if err = w.topicClosingNotice(ctx, op, payload.ChatID, payload.TopicID, notice); err != nil {
			return err
		}
	}
	if err = w.client.CloseTopic(ctx, payload.ChatID, payload.TopicID); err != nil {
		return err
	}
	if payload.Retire {
		return w.retire(ctx, t.ID, payload.ChatID, payload.TopicID)
	}
	return nil
}

func (w *Worker) reopenTopic(ctx context.Context, payload operationPayload, t domain.Ticket) error {
	var err error
	if payload.RoutingGeneration != 0 && payload.RoutingGeneration != t.RoutingGeneration {
		return nil
	}
	if t.TopicID == 0 || !provisioningStatus(t.Status) {
		return nil
	}
	if err = w.client.ReopenTopic(ctx, t.TopicChatID, t.TopicID); err != nil {
		return err
	}
	t, err = w.ensureTopicCard(ctx, t)
	if err != nil {
		return err
	}
	if err = w.replayDeferred(ctx, t); err != nil {
		return err
	}
	t, err = w.finishProvision(ctx, t)
	if err != nil {
		return err
	}
	return w.refresh(ctx, t)
}

func (w *Worker) deleteTopic(ctx context.Context, payload operationPayload, t domain.Ticket) error {
	if payload.ChatID == 0 || payload.TopicID == 0 || payload.CompletionKey == "" {
		return domain.ErrInvalid
	}
	events, e := w.store.Events(ctx, t.ID)
	if e != nil {
		return e
	}
	intent := false
	for _, event := range events {
		if event.Kind != "completion_cleanup_requested" {
			continue
		}
		var saved struct {
			ChatID     int64  `json:"chat_id"`
			TopicID    int64  `json:"topic_id"`
			Generation int64  `json:"routing_generation"`
			Key        string `json:"completion_key"`
		}
		if json.Unmarshal(event.Data, &saved) == nil && saved.ChatID == payload.ChatID && saved.TopicID == payload.TopicID && saved.Generation == payload.RoutingGeneration && saved.Key == payload.CompletionKey {
			intent = true
		}
	}
	if !intent {
		return domain.ErrForbidden
	}
	data, _ := json.Marshal(map[string]any{"chat_id": payload.ChatID, "topic_id": payload.TopicID, "routing_generation": payload.RoutingGeneration, "key": payload.CompletionKey, "completion_key": payload.CompletionKey})
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		now, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if now.TopicChatID == payload.ChatID && now.TopicID == payload.TopicID && now.Status.Active() {
			return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "cleanup_deferred", Data: data, CreatedAt: time.Now().UTC()})
		}
		if w.cfg.BeforeDelete == nil {
			return domain.ErrForbidden
		}
		if e = w.cfg.BeforeDelete(ctx, tx, now, payload.ChatID, payload.TopicID); e != nil {
			if errors.Is(e, domain.ErrConflict) {
				return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "cleanup_deferred", Data: data, CreatedAt: time.Now().UTC()})
			}
			return e
		}
		// This call holds the inbox/archive fence. Bound network waiting so
		// Telegram trouble cannot stall acceptance for the client's 65s timeout.
		deleteCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		e = topicDeleteError(w.client.Call(deleteCtx, "deleteForumTopic", map[string]any{"chat_id": payload.ChatID, "message_thread_id": payload.TopicID}, nil))
		cancel()
		if e != nil {
			return e
		}
		if now.TopicChatID == payload.ChatID && now.TopicID == payload.TopicID && !now.Status.Active() {
			now.TopicChatID = 0
			now.TopicID = 0
			now.TopicMessageID = 0
			if e = tx.SaveTicket(ctx, &now); e != nil {
				return e
			}
		}
		return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "topic_deleted", Data: data, CreatedAt: time.Now().UTC()})
	})
}
