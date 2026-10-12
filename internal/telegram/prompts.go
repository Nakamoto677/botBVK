package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

type promptKey struct{ UserID, ChatID, TopicID int64 }
type prompt struct {
	TicketID                     int64
	Action, Phase                string
	FlowID                       string
	Department, SourceDepartment domain.Department
	ExpiresAt                    time.Time
	Generation                   int64
}

func scope(actor int64, m *Message) promptKey { return promptKey{actor, m.Chat.ID, m.ThreadID} }
func (a *Adapter) promptText(ctx context.Context, m *Message, actor, ticket int64, text string) error {
	return a.sayAux(ctx, m, actor, actionScenario(ticket), fmt.Sprintf("Заявка #%d: %s", ticket, text), [][]Button{{{Text: "Отмена", Data: fmt.Sprintf("promptcancel:%d", ticket)}}})
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
	if action == "close" && u.Role != "admin" && u.Role != "dispatcher" && t.AssigneeID != u.ID {
		return domain.ErrForbidden
	}
	if t.RoutingPending {
		return domain.ErrConflict
	}
	if action != "close" && action != "transfer" {
		return domain.ErrInvalid
	}
	p := prompt{TicketID: id, Action: action, SourceDepartment: t.Department, Generation: t.RoutingGeneration, ExpiresAt: time.Now().Add(15 * time.Minute)}
	key := scope(c.From.ID, c.Message)
	if action == "close" {
		p.Phase = "reason"
		if err = a.replacePrompt(ctx, key, p); err != nil {
			return err
		}
		return a.promptText(ctx, c.Message, c.From.ID, id, "напишите итог решения следующим сообщением.")
	}
	p.Phase = "department"
	if err = a.replacePrompt(ctx, key, p); err != nil {
		return err
	}
	buttons := [][]Button{}
	for _, d := range []domain.Department{domain.OneC, domain.IT, domain.Installation, domain.Field} {
		if d != t.Department {
			buttons = append(buttons, []Button{{Text: departmentName(d), Data: fmt.Sprintf("transferdept:%d:%s", id, d)}})
		}
	}
	buttons = append(buttons, []Button{{Text: "Отмена", Data: fmt.Sprintf("promptcancel:%d", id)}})
	return a.sayAux(ctx, c.Message, c.From.ID, actionScenario(id), fmt.Sprintf("Заявка #%d: выберите отдел для передачи.", id), buttons)
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
	if !historyAuthorized(u, t) || (u.Role != "client" && u.Role != "admin" && u.Role != "dispatcher") {
		return domain.ErrForbidden
	}
	if !t.Status.Active() {
		return a.showTicket(ctx, m, u, t)
	}
	p := prompt{TicketID: id, Action: "cancel", Phase: "confirm", SourceDepartment: t.Department, Generation: t.RoutingGeneration, FlowID: strconv.FormatInt(time.Now().UnixNano(), 36), ExpiresAt: time.Now().Add(15 * time.Minute)}
	if err = a.replacePrompt(ctx, scope(actor, m), p); err != nil {
		return err
	}
	return a.sayAux(ctx, m, actor, actionScenario(id), "Вы уверены, что хотите завершить заявку?", [][]Button{{{Text: "Да, завершить", Data: fmt.Sprintf("cancelconfirm:%d:%s", id, p.FlowID)}, {Text: "Нет, продолжить", Data: fmt.Sprintf("promptcancel:%d:%s", id, p.FlowID)}}})
}
func (a *Adapter) promptCallback(ctx context.Context, c *Callback, action string, id int64, parts []string) error {
	if c.Message == nil {
		return domain.ErrInvalid
	}
	key := scope(c.From.ID, c.Message)
	p, exists, err := a.loadPrompt(ctx, key)
	if err != nil {
		return err
	}
	if action == "cancelconfirm" {
		u, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		if !historyAuthorized(u, t) || (u.Role != "client" && u.Role != "admin" && u.Role != "dispatcher") {
			return domain.ErrForbidden
		}
		if !t.Status.Active() {
			if exists && p.TicketID == id {
				a.finishPrompt(ctx, key, p)
			} else {
				a.cleanupBestEffort(ctx, key, actionScenario(id))
			}
			return a.showTicket(ctx, c.Message, u, t)
		}
	}
	if !exists || p.TicketID != id {
		return domain.ErrConflict
	}
	if p.FlowID != "" && (len(parts) != 3 || parts[2] != p.FlowID) {
		return domain.ErrConflict
	}
	if time.Now().After(p.ExpiresAt) && action != "promptcancel" {
		a.finishPrompt(ctx, key, p)
		return domain.ErrConflict
	}
	if action == "promptcancel" {
		a.finishPrompt(ctx, key, p)
		return nil
	}
	if action == "transferdept" {
		if p.Action != "transfer" || p.Phase != "department" || len(parts) != 3 || !domain.Department(parts[2]).Valid() || domain.Department(parts[2]) == p.SourceDepartment {
			return domain.ErrInvalid
		}
		p.Department = domain.Department(parts[2])
		p.Phase = "reason"
		if err = a.savePrompt(ctx, key, p); err != nil {
			return err
		}
		a.cleanupBestEffort(ctx, key, actionScenario(id))
		return a.promptText(ctx, c.Message, c.From.ID, id, "напишите причину передачи в отдел "+departmentName(p.Department)+" следующим сообщением.")
	}
	if action == "cancelconfirm" {
		if p.Action != "cancel" || p.Phase != "confirm" {
			return domain.ErrInvalid
		}
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		if t.RoutingGeneration != p.Generation {
			a.finishPrompt(ctx, key, p)
			return domain.ErrConflict
		}
		service, ok := a.service.(interface {
			Cancel(context.Context, int64, int64, string) error
		})
		if !ok {
			return domain.ErrInvalid
		}
		if err = service.Cancel(ctx, c.From.ID, id, ""); err != nil {
			return err
		}
		a.finishPrompt(ctx, key, p)
		return nil
	}
	return domain.ErrInvalid
}
func (a *Adapter) consumePrompt(ctx context.Context, m *Message, u domain.User) (bool, error) {
	key := scope(m.From.ID, m)
	p, exists, err := a.loadPrompt(ctx, key)
	if err != nil {
		return true, err
	}
	if !exists {
		return false, nil
	}
	text := strings.TrimSpace(messageText(m))
	if _, menu := menuAliases[text]; menu || strings.HasPrefix(text, "/") {
		return false, nil
	}
	if p.Action == "cancel" {
		return false, nil
	}
	if time.Now().After(p.ExpiresAt) {
		a.finishPrompt(ctx, key, p)
		return true, a.say(ctx, m, "Время служебного ввода истекло. Нажмите кнопку нужного действия заново.", nil)
	}
	if p.Phase != "reason" {
		return true, a.sayAux(ctx, m, m.From.ID, actionScenario(p.TicketID), "Сначала выберите отдел кнопкой в предыдущем вопросе.", nil)
	}
	if text == "" {
		return true, a.promptText(ctx, m, m.From.ID, p.TicketID, "нужен текст; можно написать его в подписи к вложению.")
	}
	t, err := a.store.Ticket(ctx, p.TicketID)
	if err != nil {
		return true, err
	}
	if t.Department != p.SourceDepartment || !t.Status.Active() || t.RoutingGeneration != p.Generation {
		a.finishPrompt(ctx, key, p)
		return true, domain.ErrConflict
	}
	if !staffAuthorized(u, t) {
		return true, domain.ErrForbidden
	}
	attachment := normalise(m, false)
	if attachment.FileID != "" {
		switch p.Action {
		case "close":
			if service, ok := a.service.(interface {
				CloseWithMessage(context.Context, int64, int64, string, domain.Message) error
			}); ok {
				err = service.CloseWithMessage(ctx, u.ID, p.TicketID, text, attachment)
			} else {
				return true, a.promptText(ctx, m, m.From.ID, p.TicketID, "сначала отправьте вложение отдельным сообщением, затем напишите итог текстом.")
			}
		case "transfer":
			if service, ok := a.service.(interface {
				TransferWithMessage(context.Context, int64, int64, domain.Department, string, domain.Message) error
			}); ok {
				err = service.TransferWithMessage(ctx, u.ID, p.TicketID, p.Department, text, attachment)
			} else {
				return true, a.promptText(ctx, m, m.From.ID, p.TicketID, "сначала отправьте вложение отдельным сообщением, затем напишите причину текстом.")
			}
		default:
			err = domain.ErrInvalid
		}
	} else {
		switch p.Action {
		case "close":
			err = a.service.Close(ctx, u.ID, p.TicketID, text)
		case "transfer":
			err = a.service.Transfer(ctx, u.ID, p.TicketID, p.Department, text)
		default:
			err = domain.ErrInvalid
		}
	}
	if err != nil {
		return true, err
	}
	a.finishPrompt(ctx, key, p)
	return true, nil
}
