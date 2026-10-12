package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

type promptKey struct{ UserID, ChatID, TopicID int64 }
type prompt struct {
	TicketID         int64
	Action, Phase    string
	Department       domain.Department
	SourceDepartment domain.Department
	ExpiresAt        time.Time
	Generation       int64
}

func scope(actor int64, m *Message) promptKey { return promptKey{actor, m.Chat.ID, m.ThreadID} }
func (a *Adapter) promptText(ctx context.Context, m *Message, ticket int64, text string) error {
	return a.say(ctx, m, fmt.Sprintf("Заявка #%d: %s", ticket, text), [][]Button{{{Text: "Отмена", Data: fmt.Sprintf("promptcancel:%d", ticket)}}})
}
func (a *Adapter) beginStaffPrompt(ctx context.Context, c *Callback, id int64, action string) error {
	if c.Message == nil {
		return domain.ErrInvalid
	}
	t, err := a.store.Ticket(ctx, id)
	if err != nil {
		return err
	}
	u, err := a.store.User(ctx, c.From.ID)
	if err != nil {
		return err
	}
	if !t.Status.Active() || !staffAuthorized(u, t) {
		return domain.ErrForbidden
	}
	if (action == "close" || action == "reply") && u.Role != "admin" && u.Role != "dispatcher" && t.AssigneeID != u.ID {
		return domain.ErrForbidden
	}
	if t.RoutingPending {
		return domain.ErrConflict
	}
	p := prompt{TicketID: id, Action: action, SourceDepartment: t.Department, Generation: t.RoutingGeneration, ExpiresAt: time.Now().Add(15 * time.Minute)}
	if action == "reply" {
		if !provisioningStatus(t.Status) {
			return domain.ErrConflict
		}
		p.Phase = "reason"
		a.prompts[scope(c.From.ID, c.Message)] = p
		return a.promptText(ctx, c.Message, id, "отправьте ответ клиенту следующим сообщением. Текст и вложения будут отправлены клиенту.")
	}
	if action == "close" {
		p.Phase = "reason"
		a.prompts[scope(c.From.ID, c.Message)] = p
		return a.promptText(ctx, c.Message, id, "напишите итог решения следующим сообщением.")
	}
	p.Phase = "department"
	a.prompts[scope(c.From.ID, c.Message)] = p
	buttons := [][]Button{}
	for _, d := range []domain.Department{domain.OneC, domain.IT, domain.Installation, domain.Field} {
		if d == t.Department {
			continue
		}
		buttons = append(buttons, []Button{{Text: departmentName(d), Data: fmt.Sprintf("transferdept:%d:%s", id, d)}})
	}
	buttons = append(buttons, []Button{{Text: "Отмена", Data: fmt.Sprintf("promptcancel:%d", id)}})
	return a.say(ctx, c.Message, fmt.Sprintf("Заявка #%d: выберите отдел для передачи.", id), buttons)
}
func (a *Adapter) beginCancel(ctx context.Context, actor int64, m *Message, id int64) error {
	if m == nil || m.Chat.Type != "private" {
		return domain.ErrForbidden
	}
	u, err := a.store.User(ctx, actor)
	if err != nil {
		return err
	}
	t, err := a.store.Ticket(ctx, id)
	if err != nil {
		return err
	}
	if !historyAuthorized(u, t) || !t.Status.Active() {
		return domain.ErrForbidden
	}
	if u.Role != "client" && u.Role != "admin" && u.Role != "dispatcher" {
		return domain.ErrForbidden
	}
	a.prompts[scope(actor, m)] = prompt{TicketID: id, Action: "cancel", Phase: "confirm", SourceDepartment: t.Department, ExpiresAt: time.Now().Add(15 * time.Minute)}
	return a.say(ctx, m, fmt.Sprintf("Отменить заявку #%d?", id), [][]Button{{{Text: "Да, отменить", Data: fmt.Sprintf("cancelconfirm:%d", id)}, {Text: "Нет", Data: fmt.Sprintf("promptcancel:%d", id)}}})
}
func (a *Adapter) promptCallback(ctx context.Context, c *Callback, action string, id int64, parts []string) error {
	if c.Message == nil {
		return domain.ErrInvalid
	}
	key := scope(c.From.ID, c.Message)
	p, ok := a.prompts[key]
	if !ok || p.TicketID != id || time.Now().After(p.ExpiresAt) {
		return domain.ErrConflict
	}
	if action == "promptcancel" {
		delete(a.prompts, key)
		return a.say(ctx, c.Message, "Действие отменено. Заявка не изменилась.", nil)
	}
	if action == "transferdept" {
		if p.Action != "transfer" || p.Phase != "department" || len(parts) != 3 || !domain.Department(parts[2]).Valid() || domain.Department(parts[2]) == p.SourceDepartment {
			return domain.ErrInvalid
		}
		p.Department = domain.Department(parts[2])
		p.Phase = "reason"
		a.prompts[key] = p
		return a.promptText(ctx, c.Message, id, "напишите причину передачи в отдел "+departmentName(p.Department)+" следующим сообщением.")
	}
	if action == "cancelconfirm" {
		if p.Action != "cancel" || p.Phase != "confirm" {
			return domain.ErrInvalid
		}
		p.Phase = "reason"
		a.prompts[key] = p
		return a.promptText(ctx, c.Message, id, "напишите причину отмены следующим сообщением.")
	}
	return domain.ErrInvalid
}
func (a *Adapter) consumePrompt(ctx context.Context, m *Message, u domain.User) (bool, error) {
	key := scope(m.From.ID, m)
	p, ok := a.prompts[key]
	if !ok {
		return false, nil
	}
	text := strings.TrimSpace(messageText(m))
	if _, ok := menuAliases[text]; ok {
		return false, nil
	}
	if strings.HasPrefix(text, "/") && !(p.Action == "reply" && (text == "/reply" || strings.HasPrefix(text, "/reply "))) {
		return false, nil
	}
	if time.Now().After(p.ExpiresAt) {
		delete(a.prompts, key)
		return true, a.say(ctx, m, "Время ввода истекло. Нажмите кнопку нужного действия заново.", nil)
	}
	if p.Phase != "reason" {
		return true, a.say(ctx, m, "Сначала выберите действие кнопкой в предыдущем сообщении.", nil)
	}
	if text == "" && !(p.Action == "reply" && normalise(m, false).FileID != "") {
		return true, a.promptText(ctx, m, p.TicketID, "нужен текст. Можно написать его в подписи к вложению.")
	}
	t, err := a.store.Ticket(ctx, p.TicketID)
	if err != nil {
		return true, err
	}
	if t.Department != p.SourceDepartment || !t.Status.Active() || (p.Action != "cancel" && t.RoutingGeneration != p.Generation) {
		delete(a.prompts, key)
		return true, domain.ErrConflict
	}
	if p.Action != "cancel" && !staffAuthorized(u, t) {
		return true, domain.ErrForbidden
	}
	attachment := normalise(m, false)
	if attachment.FileID != "" && p.Action != "reply" {
		switch p.Action {
		case "close":
			if service, ok := a.service.(interface {
				CloseWithMessage(context.Context, int64, int64, string, domain.Message) error
			}); ok {
				err = service.CloseWithMessage(ctx, u.ID, p.TicketID, text, attachment)
			} else {
				return true, a.promptText(ctx, m, p.TicketID, "вложение нельзя сохранить вместе с этим действием. Отправьте файл отдельным сообщением, затем напишите итог текстом.")
			}
		case "transfer":
			if service, ok := a.service.(interface {
				TransferWithMessage(context.Context, int64, int64, domain.Department, string, domain.Message) error
			}); ok {
				err = service.TransferWithMessage(ctx, u.ID, p.TicketID, p.Department, text, attachment)
			} else {
				return true, a.promptText(ctx, m, p.TicketID, "вложение нельзя сохранить вместе с этим действием. Отправьте файл отдельным сообщением, затем напишите причину текстом.")
			}
		default:
			return true, a.promptText(ctx, m, p.TicketID, "пришлите причину текстом без вложения; файл можно отправить отдельным сообщением до завершения действия.")
		}
		if err != nil {
			return true, err
		}
		delete(a.prompts, key)
		return true, a.say(ctx, m, "Готово. Заявка обновлена, вложение сохранено для архива.", nil)
	}
	switch p.Action {
	case "reply":
		if u.Role != "admin" && u.Role != "dispatcher" && t.AssigneeID != u.ID {
			return true, domain.ErrForbidden
		}
		msg := normalise(m, text == "/reply" || strings.HasPrefix(text, "/reply "))
		err = a.service.AddMessage(ctx, u.ID, p.TicketID, msg, true)
	case "close":
		err = a.service.Close(ctx, u.ID, p.TicketID, text)
	case "transfer":
		err = a.service.Transfer(ctx, u.ID, p.TicketID, p.Department, text)
	case "cancel":
		if service, ok := a.service.(interface {
			Cancel(context.Context, int64, int64, string) error
		}); ok {
			err = service.Cancel(ctx, u.ID, p.TicketID, text)
		} else {
			err = domain.ErrInvalid
		}
	default:
		err = domain.ErrInvalid
	}
	if err != nil {
		return true, err
	}
	delete(a.prompts, key)
	if p.Action == "reply" {
		return true, a.say(ctx, m, "Ответ поставлен в очередь отправки клиенту.", nil)
	}
	return true, a.say(ctx, m, "Готово. Заявка обновлена.", nil)
}
