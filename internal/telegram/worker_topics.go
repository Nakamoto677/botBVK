package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bot-bvk/internal/domain"
)

// operationPayload is the shared shape of an outbox operation payload. Fields are
// decoded once in Handle, which keeps every handler free of local struct copies.
type operationPayload struct {
	Text              string            `json:"text"`
	Baseline          string            `json:"baseline"`
	UserID            int64             `json:"user_id"`
	TicketID          int64             `json:"ticket_id"`
	ChatID            int64             `json:"chat_id"`
	TopicID           int64             `json:"topic_id"`
	OldChatID         int64             `json:"old_chat_id"`
	OldTopicID        int64             `json:"old_topic_id"`
	Reason            string            `json:"reason"`
	Retire            bool              `json:"retire"`
	MessageID         int64             `json:"message_id"`
	QueueMediaType    string            `json:"queue_media_type"`
	FromDepartment    domain.Department `json:"from_department"`
	ToDepartment      domain.Department `json:"to_department"`
	RoutingGeneration int64             `json:"routing_generation"`
}

// Topic lifecycle lives here so that creating, closing and reopening a
// Telegram topic follow one readable path each. Every
// handler receives the ticket that Handle already loaded.
//
// A ticket binding is immutable for the duration of an operation: the handlers
// only reassign their local copy after a successful write, so a concurrent
// transfer cannot be overwritten by stale data.

func (w *Worker) createTopic(ctx context.Context, op domain.Operation, payload operationPayload, t domain.Ticket, team Team) error {
	var err error
	if t.IsGroup() {
		// A company group is the conversation surface. A stale queued operation
		// must never create a department topic for it.
		return w.refresh(ctx, t)
	}
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
	// A ticket binding survives retries. A client binding survives tickets.
	if t.TopicID != 0 && t.TopicChatID == team.ChatID {
		return w.prepareTopic(ctx, t, payload)
	}
	if !t.Status.Active() {
		return nil
	}
	topics, ok := w.store.(domain.ClientTopicStore)
	if !ok {
		return domain.ErrInvalid
	}
	binding, err := topics.ClientTopic(ctx, t.ClientID, t.Department)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	previousTopicID := int64(0)
	if err == nil {
		previousTopicID = binding.TopicID
		if binding.ChatID == team.ChatID {
			if err = w.client.ReopenTopic(ctx, binding.ChatID, binding.TopicID); err == nil {
				if err = w.bindTicketTopic(ctx, t, binding, 0, false); err != nil {
					return err
				}
				t, err = w.store.Ticket(ctx, t.ID)
				if err != nil {
					return err
				}
				return w.prepareTopic(ctx, t, payload)
			}
			if !topicMissing(err) {
				return err
			}
		}
	}
	number, err := topics.ClientNumber(ctx, t.ClientID)
	if err != nil {
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
	title := []rune(fmt.Sprintf("Клиент №%d · %s · %s", number, name, company))
	if len(title) > 120 {
		title = title[:120]
	}
	if err = w.rate(ctx, team.ChatID, "createForumTopic"); err != nil {
		return err
	}
	topic, err := w.client.Topic(ctx, team.ChatID, string(title))
	if err != nil {
		return err
	}
	binding = domain.ClientTopic{ClientID: t.ClientID, Department: t.Department, ChatID: team.ChatID, TopicID: topic}
	if err = w.bindTicketTopic(ctx, t, binding, previousTopicID, previousTopicID == 0); err != nil {
		// Keep an orphan available for diagnosis; closing it could hide messages
		// accepted during an ambiguous Telegram response.
		return err
	}
	t, err = w.store.Ticket(ctx, t.ID)
	if err != nil {
		return err
	}
	if previousTopicID != 0 {
		_, err = w.textParts(ctx, fmt.Sprintf("replacement:%d:%s:%d", t.ClientID, t.Department, t.TopicID), t.TopicChatID, t.TopicID, "Прежний топик недоступен. Переписка прошлых заявок сохранена в архиве бота (/history ID).", nil)
		if err != nil {
			return err
		}
	}
	return w.prepareTopic(ctx, t, payload)
}

func topicMissing(err error) bool {
	var api *APIError
	if !errors.As(err, &api) || api.Code != 400 {
		return false
	}
	text := strings.ToLower(api.Description)
	return strings.Contains(text, "topic_id_invalid") || strings.Contains(text, "topic not found") || strings.Contains(text, "message thread not found") || strings.Contains(text, "message_thread_id_invalid")
}

func (w *Worker) bindTicketTopic(ctx context.Context, t domain.Ticket, binding domain.ClientTopic, replace int64, insert bool) error {
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		current, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if current.Department != t.Department || !provisioningStatus(current.Status) || !current.RoutingPending || current.AssigneeID != t.AssigneeID || current.RoutingGeneration != t.RoutingGeneration || current.TopicID != t.TopicID || current.TopicChatID != t.TopicChatID {
			return domain.ErrConflict
		}
		if current.TopicID != 0 {
			return domain.ErrConflict
		}
		if replace != 0 || insert {
			bindingTx, ok := tx.(domain.ClientTopicTx)
			if !ok {
				return domain.ErrInvalid
			}
			if replace != 0 {
				e = bindingTx.ReplaceClientTopic(ctx, binding, replace)
			} else {
				e = bindingTx.BindClientTopic(ctx, binding)
			}
			if e != nil {
				return e
			}
		}
		current.TopicChatID = binding.ChatID
		current.TopicID = binding.TopicID
		current.TopicMessageID = 0
		if e = tx.SaveTicket(ctx, &current); e != nil {
			return e
		}
		return nil
	})
}

func (w *Worker) prepareTopic(ctx context.Context, t domain.Ticket, payload operationPayload) error {
	var err error
	t, err = w.ensureTopicCard(ctx, t)
	if err != nil {
		return err
	}
	if payload.Reason != "" {
		if _, err = w.send(ctx, t.TopicChatID, t.TopicID, "Причина передачи: "+payload.Reason, nil); err != nil {
			return err
		}
	}
	// Transfer queued its own close operation for the previous department.
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
	if t.IsGroup() {
		return nil
	}
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
	if topics, ok := w.store.(domain.ClientTopicStore); ok {
		department := t.Department
		if payload.Retire && payload.FromDepartment.Valid() {
			department = payload.FromDepartment
		}
		active, e := topics.ActiveClientTopicTicket(ctx, t.ClientID, department, t.ID)
		if e == nil && active.TopicChatID == payload.ChatID && active.TopicID == payload.TopicID {
			return nil
		}
		if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return e
		}
	}
	notice := payload.Text
	if payload.Retire && payload.ToDepartment.Valid() {
		notice = fmt.Sprintf("Заявка #%d передана в отдел %s", t.ID, departmentName(payload.ToDepartment))
	} else if t.ClosedAt != nil {
		actor, e := displayName(ctx, w.store, t.ClosedBy, "Участник")
		if e != nil {
			return e
		}
		notice += fmt.Sprintf("\nЗавершил: %s · %s (Иркутск)", actor, t.ClosedAt.In(irkutsk).Format("02.01.2006 15:04"))
	}
	if notice != "" {
		if err = w.topicClosingNotice(ctx, op, payload.ChatID, payload.TopicID, notice); err != nil {
			// A confirmed missing topic must not leave the close operation stuck:
			// there is no conversation left to post the notice into. The next
			// claim creates a replacement topic with an archive note.
			if !topicMissing(err) {
				return err
			}
		}
	}
	if err = w.client.CloseTopic(ctx, payload.ChatID, payload.TopicID); err != nil && !topicMissing(err) {
		return err
	}
	if payload.Retire {
		return w.retire(ctx, t.ID, payload.ChatID, payload.TopicID)
	}
	return nil
}

func (w *Worker) reopenTopic(ctx context.Context, payload operationPayload, t domain.Ticket) error {
	if t.IsGroup() {
		return nil
	}
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
