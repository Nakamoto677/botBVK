package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

const engineerCloseLabel = "Закрыть тикет"
const engineerTransferLabel = "Передать"

// Telegram text limits count rendered characters. Bound UTF-16 length before
// escaping, so even long emoji-only descriptions leave room for the card.
func engineerTextBound(text string, limit int) string {
	used := 0
	for offset, r := range text {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if used+n > limit {
			return text[:offset] + "…"
		}
		used += n
	}
	return text
}
func engineerField(text string, limit int) string {
	return html.EscapeString(engineerTextBound(strings.Join(strings.Fields(text), " "), limit))
}

const engineerDescriptionLimit = 2400

func engineerCardText(t domain.Ticket, client, company, assignee string) string {
	return engineerCardTextLimit(t, client, company, assignee, engineerDescriptionLimit)
}

// engineerCardTextLimit renders the whole card with a caller-chosen description
// budget. A media message can only carry a 1024-unit caption, so the queue photo
// card passes a smaller budget while the regular text card keeps 2400.
func engineerCardTextLimit(t domain.Ticket, client, company, assignee string, descriptionLimit int) string {
	description := strings.TrimSpace(t.Description)
	if description == "" {
		description = "Описание не указано"
	}
	return engineerCardPrefix(t, client, company, assignee) + html.EscapeString(engineerTextBound(description, descriptionLimit)) + engineerCardSuffix(t)
}

// engineerCardPrefix ends right after the "Описание" heading, and
// engineerCardSuffix carries the transient provisioning note. Keeping the pieces
// separate lets a caption budget leave room for the queue-only extras without
// cutting HTML tags or rebuilding the card in two places.
func engineerCardPrefix(t domain.Ticket, client, company, assignee string) string {
	if t.AssigneeID == 0 {
		assignee = "Не назначен"
	}
	heading := fmt.Sprintf("Заявка #%d · %s", t.ID, statusName(t.Status))
	if t.Urgency == domain.Urgent {
		heading = fmt.Sprintf("🔴 СРОЧНАЯ ЗАЯВКА #%d · %s", t.ID, statusName(t.Status))
	}
	urgency := urgencyLabel(t.Urgency)
	if t.Urgency == domain.Urgent {
		urgency = "срочно, указана клиентом"
	}
	if t.IsGroup() {
		requester := strings.TrimSpace(t.RequesterName)
		if requester == "" {
			requester = "Сотрудник компании"
		}
		if username := strings.TrimSpace(t.RequesterUsername); username != "" {
			requester += " · @" + strings.TrimPrefix(username, "@")
		}
		return fmt.Sprintf("<b>%s</b>\n<b>Начало:</b> %s (Иркутск)\n\n<b>Компания:</b> %s\n<b>Чат компании:</b> %s\n<b>Обратился:</b> %s\n<b>Отдел:</b> %s\n<b>Исполнитель:</b> %s\n\n<b>Срочность:</b> %s\n\n<b>Описание</b>\n", heading, t.CreatedAt.In(irkutsk).Format("02.01.2006 15:04"), engineerField(company, 120), engineerField(client, 100), engineerField(requester, 100), departmentName(t.Department), engineerField(assignee, 100), urgency)
	}
	return fmt.Sprintf("<b>%s</b>\n<b>Начало:</b> %s (Иркутск)\n\n<b>Компания:</b> %s\n<b>Клиент:</b> %s\n<b>Отдел:</b> %s\n<b>Исполнитель:</b> %s\n\n<b>Срочность:</b> %s\n\n<b>Описание</b>\n", heading, t.CreatedAt.In(irkutsk).Format("02.01.2006 15:04"), engineerField(company, 120), engineerField(client, 100), departmentName(t.Department), engineerField(assignee, 100), urgency)
}

func engineerCardSuffix(t domain.Ticket) string {
	if t.RoutingPending {
		return "\n\n<i>Создаём топик. Кнопки появятся после подготовки.</i>"
	}
	return ""
}
func engineerTopicButtons(t domain.Ticket) [][]Button {
	if !t.Status.Active() || t.Status == domain.New {
		return [][]Button{}
	}
	id := fmt.Sprint(t.ID)
	return [][]Button{{{Text: engineerCloseLabel, Data: "close:" + id}, {Text: engineerTransferLabel, Data: "transfer:" + id}}}
}
func engineerQueueButtons(t domain.Ticket, companyLink string) [][]Button {
	if !t.Status.Active() || t.RoutingPending {
		return [][]Button{}
	}
	id := fmt.Sprint(t.ID)
	if t.IsGroup() {
		buttons := [][]Button{}
		if link := strings.TrimSpace(companyLink); link != "" {
			buttons = append(buttons, []Button{{Text: "В чат компании", URL: link}})
		}
		if t.Status == domain.New {
			return append(buttons, []Button{{Text: "Взять в работу", Data: "claim:" + id}}, []Button{{Text: engineerTransferLabel, Data: "transfer:" + id}})
		}
		return append(buttons, []Button{{Text: "Закрыть задачу", Data: "close:" + id}, {Text: engineerTransferLabel, Data: "transfer:" + id}})
	}
	if t.Status == domain.New {
		return [][]Button{{{Text: "Взять в работу", Data: "claim:" + id}}, {{Text: engineerTransferLabel, Data: "transfer:" + id}}}
	}
	buttons := [][]Button{}
	if strings.HasPrefix(fmt.Sprint(t.TopicChatID), "-100") && t.TopicID > 0 {
		messageID := t.TopicMessageID
		if messageID == 0 {
			messageID = t.TopicID
		}
		link := fmt.Sprintf("https://t.me/c/%s/%d?thread=%d", strings.TrimPrefix(fmt.Sprint(t.TopicChatID), "-100"), messageID, t.TopicID)
		buttons = append(buttons, []Button{{Text: "В чат", URL: link}})
	}
	return append(buttons, []Button{{Text: "Закрыть задачу", Data: "close:" + id}, {Text: engineerTransferLabel, Data: "transfer:" + id}})
}

// Engineer cards are HTML, so they are sent through the shared recorded-parts
// path with an explicit parse mode rather than a private send helper. That keeps
// the retry fence in force: a crash between the send and the ticket update used
// to duplicate the card, because this call bypassed bvk_send_parts.
func (w *Worker) sendEngineerCard(ctx context.Context, chat, topic int64, text string, buttons [][]Button) (Message, error) {
	return w.send(ctx, chat, topic, text, buttons, "HTML")
}

// Reply keyboards send ordinary text. The handler resolves that text only in
// the source's current ticket topic, before public message relay or prompts.
func (a *Adapter) engineerKeyboardAction(ctx context.Context, m *Message) (bool, error) {
	action := ""
	switch strings.TrimSpace(m.Text) {
	case engineerCloseLabel:
		action = "close"
	case engineerTransferLabel:
		action = "transfer"
	default:
		return false, nil
	}
	t, err := a.store.TopicTicket(ctx, m.Chat.ID, m.ThreadID)
	if errors.Is(err, domain.ErrNotFound) {
		return true, a.say(ctx, m, "Откройте актуальный топик заявки и нажмите кнопку там. В очереди выберите действие на карточке заявки.", nil)
	}
	if err != nil {
		return true, err
	}
	if t.TopicID == 0 || m.Chat.ID != t.TopicChatID || m.ThreadID != t.TopicID || !t.Status.Active() || m.ThreadID == a.cfg.Departments[t.Department].QueueTopicID {
		return true, domain.ErrConflict
	}
	if err = a.beginStaffPrompt(ctx, &Callback{From: m.From, Message: m}, t.ID, action); err != nil {
		return true, err
	}
	// Only this validated control message is disposable, never free-text input.
	if action == "transfer" {
		return true, a.recordAux(ctx, m, m.From.ID, actionScenario(t.ID), m.ID)
	}
	return true, nil
}
func (w *Worker) ensureEngineerKeyboard(ctx context.Context, t domain.Ticket) error {
	events, err := w.store.Events(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Kind != "engineer_keyboard_ready" {
			continue
		}
		var data struct {
			ChatID  int64 `json:"chat_id"`
			TopicID int64 `json:"topic_id"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.ChatID == t.TopicChatID && data.TopicID == t.TopicID {
			return nil
		}
	}
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		current, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if current.TopicChatID != t.TopicChatID || current.TopicID != t.TopicID || current.RoutingGeneration != t.RoutingGeneration || !provisioningStatus(current.Status) {
			return domain.ErrConflict
		}
		if e = w.rate(ctx, t.TopicChatID, "sendMessage"); e != nil {
			return e
		}
		keyboard := map[string]any{"keyboard": [][]map[string]string{{{"text": engineerCloseLabel}, {"text": engineerTransferLabel}}}, "resize_keyboard": true, "is_persistent": true, "input_field_placeholder": "Сообщение клиенту"}
		if e = w.client.Call(ctx, "sendMessage", map[string]any{"chat_id": t.TopicChatID, "message_thread_id": t.TopicID, "text": "Сообщения в этом топике отправляются клиенту. Для управления заявкой используйте кнопки ниже.", "reply_markup": keyboard}, nil); e != nil {
			return e
		}
		data, _ := json.Marshal(map[string]int64{"chat_id": t.TopicChatID, "topic_id": t.TopicID})
		return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "engineer_keyboard_ready", Data: data, CreatedAt: time.Now().UTC()})
	})
}
