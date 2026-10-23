package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"

	"bot-bvk/internal/domain"
)

const engineerCloseLabel = "Завершить заявку"
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
	return engineerCardTextLimit(t, statusName(t.Status), client, company, assignee, engineerDescriptionLimit)
}

// engineerCardTextLimit renders the whole card with a caller-chosen status line
// and description budget. The status is explicit because a retired queue card
// describes the transfer, not the live ticket: it must read «Передана» even
// though the ticket already belongs to the receiving department. A media message
// can only carry a 1024-unit caption, so the queue photo card passes a smaller
// budget while the regular text card keeps 2400.
func engineerCardTextLimit(t domain.Ticket, status, client, company, assignee string, descriptionLimit int) string {
	description := strings.TrimSpace(t.Description)
	if description == "" {
		description = "Описание не указано"
	}
	return engineerCardPrefix(t, status, client, company, assignee) + html.EscapeString(engineerTextBound(description, descriptionLimit)) + engineerCardSuffix(t)
}

// engineerCardPrefix ends right after the "Описание" heading, and
// engineerCardSuffix carries the transient provisioning note. Keeping the pieces
// separate lets a caption budget leave room for the queue-only extras without
// cutting HTML tags or rebuilding the card in two places.
func engineerCardPrefix(t domain.Ticket, status, client, company, assignee string) string {
	if t.AssigneeID == 0 {
		assignee = "Не назначен"
	}
	heading := fmt.Sprintf("Заявка #%d · %s", t.ID, status)
	if t.Urgency == domain.Urgent {
		heading = fmt.Sprintf("🔴 СРОЧНАЯ ЗАЯВКА #%d · %s", t.ID, status)
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
	if !t.Status.Active() || t.Status == domain.New || t.RoutingPending {
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
		return append(buttons, []Button{{Text: engineerCloseLabel, Data: "close:" + id}, {Text: engineerTransferLabel, Data: "transfer:" + id}})
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
	return append(buttons, []Button{{Text: engineerCloseLabel, Data: "close:" + id}, {Text: engineerTransferLabel, Data: "transfer:" + id}})
}

// Engineer cards are HTML, so they are sent through the shared recorded-parts
// path with an explicit parse mode rather than a private send helper. That keeps
// the retry fence in force: a crash between the send and the ticket update used
// to duplicate the card, because this call bypassed bvk_send_parts.
func (w *Worker) sendEngineerCard(ctx context.Context, chat, topic int64, text string, buttons [][]Button) (Message, error) {
	return w.send(ctx, chat, topic, text, buttons, "HTML")
}

// Reply buttons send text, so only the current client conversation topic may
// turn that text into an action. An old keyboard in Queue is removed on tap.
func (a *Adapter) engineerKeyboardAction(ctx context.Context, m *Message, u domain.User) (bool, error) {
	action := ""
	switch strings.TrimSpace(m.Text) {
	case engineerCloseLabel:
		action = "close"
	case engineerTransferLabel:
		action = "transfer"
	default:
		return false, nil
	}
	if !u.Approved || (u.Role != "engineer" && u.Role != "admin" && u.Role != "dispatcher") {
		return true, domain.ErrForbidden
	}
	for _, team := range a.cfg.Departments {
		if m.Chat.ID == team.ChatID && m.ThreadID == team.QueueTopicID {
			return true, a.removeQueueKeyboard(ctx, m)
		}
	}
	t, err := a.store.TopicTicket(ctx, m.Chat.ID, m.ThreadID)
	if errors.Is(err, domain.ErrNotFound) {
		return true, a.say(ctx, m, "Откройте актуальный топик заявки и нажмите кнопку там.", nil)
	}
	if err != nil {
		return true, err
	}
	if t.TopicID == 0 || m.Chat.ID != t.TopicChatID || m.ThreadID != t.TopicID || !t.Status.Active() || t.RoutingPending {
		return true, domain.ErrConflict
	}
	if err = a.beginStaffPrompt(ctx, &Callback{From: m.From, Message: m}, t.ID, action); err != nil {
		return true, err
	}
	if action == "transfer" {
		return true, a.recordAux(ctx, m, m.From.ID, actionScenario(t.ID), m.ID)
	}
	return true, nil
}

func (a *Adapter) removeQueueKeyboard(ctx context.Context, m *Message) error {
	text := "В «Очереди» управляйте заявкой кнопками на её карточке."
	remove := map[string]any{"remove_keyboard": true}
	if a.activeUpdate != 0 {
		return a.queueScreen(ctx, m, m.From.ID, "", text, remove)
	}
	payload := map[string]any{"chat_id": m.Chat.ID, "message_thread_id": m.ThreadID, "text": text, "reply_markup": remove}
	return a.client.Call(ctx, "sendMessage", payload, nil)
}
