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
func (a *Adapter) clientTopic(m *Message, t domain.Ticket) bool {
	team := a.cfg.Departments[t.Department]
	return !t.IsGroup() && t.TopicID != 0 && m.Chat.ID == t.TopicChatID && m.ThreadID == t.TopicID && m.ThreadID != team.QueueTopicID
}
func (a *Adapter) promptText(ctx context.Context, m *Message, actor, ticket int64, action, text string) error {
	label := "Отменить завершение"
	buttons := [][]Button{}
	if action == "transfer" {
		label = "Отменить передачу"
	} else if action == "close" {
		t, err := a.store.Ticket(ctx, ticket)
		if err != nil {
			return err
		}
		if a.clientTopic(m, t) {
			buttons = append(buttons, []Button{{Text: "Без комментария", Data: fmt.Sprintf("closewithout:%d", ticket)}})
		}
	}
	buttons = append(buttons, []Button{{Text: label, Data: fmt.Sprintf("promptcancel:%d", ticket)}})
	return a.sayAux(ctx, m, actor, actionScenario(ticket), fmt.Sprintf("Заявка №%d: %s", ticket, text), buttons)
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
		return a.promptText(ctx, c.Message, c.From.ID, id, "close", "напишите итог решения следующим сообщением. Клиент увидит его в уведомлении. На ввод есть 15 минут.")
	}
	p.Phase = "department"
	if err = a.replacePrompt(ctx, key, p); err != nil {
		return err
	}
	buttons := [][]Button{}
	for _, d := range a.configuredDepartments() {
		if d != t.Department {
			buttons = append(buttons, []Button{{Text: departmentName(d), Data: fmt.Sprintf("transferdept:%d:%s", id, d)}})
		}
	}
	buttons = append(buttons, []Button{{Text: "Отменить передачу", Data: fmt.Sprintf("promptcancel:%d", id)}})
	return a.sayAux(ctx, c.Message, c.From.ID, actionScenario(id), fmt.Sprintf("Передача заявки №%d\nВыберите новый отдел.", id), buttons)
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
	question := fmt.Sprintf("Завершить заявку №%d?\nПереписка сохранится в истории. Если помощь ещё нужна, оставьте заявку открытой.", id)
	return a.sayAux(ctx, m, actor, actionScenario(id), question, [][]Button{{{Text: "Завершить заявку", Data: fmt.Sprintf("cancelconfirm:%d:%s", id, p.FlowID)}, {Text: "Оставить открытой", Data: fmt.Sprintf("promptcancel:%d:%s", id, p.FlowID)}}})
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
		return a.declineNotice(ctx, c, p)
	}
	if action == "closewithout" {
		if p.Action != "close" || p.Phase != "reason" {
			return domain.ErrInvalid
		}
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		if !a.clientTopic(c.Message, t) {
			return domain.ErrConflict
		}
		u, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		if !staffAuthorized(u, t) || !t.Status.Active() || t.Department != p.SourceDepartment || t.RoutingGeneration != p.Generation || t.RoutingPending {
			return domain.ErrConflict
		}
		if u.Role != "admin" && u.Role != "dispatcher" && t.AssigneeID != u.ID {
			return domain.ErrForbidden
		}
		if e = a.service.Close(ctx, c.From.ID, id, "Без комментария"); e != nil {
			return e
		}
		a.finishPrompt(ctx, key, p)
		return nil
	}
	if action == "transferwithout" {
		if p.Action != "transfer" || p.Phase != "reason" {
			return domain.ErrInvalid
		}
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		u, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		if !staffAuthorized(u, t) || !t.Status.Active() || t.Department != p.SourceDepartment || t.RoutingGeneration != p.Generation {
			return domain.ErrConflict
		}
		if e = a.service.Transfer(ctx, c.From.ID, id, p.Department, "-"); e != nil {
			return e
		}
		a.finishPrompt(ctx, key, p)
		return nil
	}
	if action == "transferdept" {
		if p.Action != "transfer" || p.Phase != "department" || len(parts) != 3 || !a.departmentEnabled(domain.Department(parts[2])) || domain.Department(parts[2]) == p.SourceDepartment {
			return domain.ErrInvalid
		}
		p.Department = domain.Department(parts[2])
		p.Phase = "reason"
		if err = a.savePrompt(ctx, key, p); err != nil {
			return err
		}
		a.cleanupBestEffort(ctx, key, actionScenario(id))
		return a.sayAux(ctx, c.Message, c.From.ID, actionScenario(id), fmt.Sprintf("Передача заявки №%d → %s\nНапишите причину передачи. Её увидят инженеры нового отдела. На ввод есть 15 минут.", id, departmentName(p.Department)), [][]Button{{{Text: "Без комментария", Data: fmt.Sprintf("transferwithout:%d", id)}}, {{Text: "Отменить передачу", Data: fmt.Sprintf("promptcancel:%d", id)}}})
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
			Cancel(context.Context, int64, int64) error
		})
		if !ok {
			return domain.ErrInvalid
		}
		if err = service.Cancel(ctx, c.From.ID, id); err != nil {
			return err
		}
		a.finishPrompt(ctx, key, p)
		return nil
	}
	return domain.ErrInvalid
}

// Declining a confirmation only deletes the question, so the user used to face
// an empty chat with no way to tell whether the action had run. Every decline
// ends with a service receipt describing the state the ticket is really in; it
// is registered under the action scenario and disappears with the next cleanup.
func (a *Adapter) declineNotice(ctx context.Context, c *Callback, p prompt) error {
	text := fmt.Sprintf("Действие отменено — заявка #%d не изменилась.", p.TicketID)
	t, err := a.store.Ticket(ctx, p.TicketID)
	switch {
	case err != nil:
		// The prompt is already finished; a store hiccup must not turn the
		// acknowledgement into "action unavailable".
	case !t.Status.Active():
		text = fmt.Sprintf("Заявка #%d уже завершена — отменять нечего.", p.TicketID)
	case p.Action == "cancel":
		text = fmt.Sprintf(clientCancelDeclinedText, p.TicketID)
	case p.Action == "close":
		text = fmt.Sprintf("Закрытие отменено — заявка #%d остаётся открытой.", p.TicketID)
	case p.Action == "transfer":
		text = fmt.Sprintf("Передача отменена — заявка #%d осталась в текущем отделе.", p.TicketID)
	}
	return a.sayAux(ctx, c.Message, c.From.ID, actionScenario(p.TicketID), text, nil)
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
	if p.Action == "company_code" {
		return true, a.handleCompanyCode(ctx, m, text)
	}
	if p.Action == "company_confirm" {
		return true, a.sayAux(ctx, m, m.From.ID, companyAccessScenario, "Подтвердите переход кнопкой в предыдущем сообщении или начните заново через /company.", nil)
	}
	if p.Phase != "reason" {
		return true, a.sayAux(ctx, m, m.From.ID, actionScenario(p.TicketID), "Сначала выберите отдел кнопкой в предыдущем вопросе.", nil)
	}
	if text == "" {
		return true, a.promptText(ctx, m, m.From.ID, p.TicketID, p.Action, "нужен текст; можно написать его в подписи к вложению.")
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
				return true, a.promptText(ctx, m, m.From.ID, p.TicketID, p.Action, "сначала отправьте вложение отдельным сообщением, затем напишите итог текстом.")
			}
		case "transfer":
			if service, ok := a.service.(interface {
				TransferWithMessage(context.Context, int64, int64, domain.Department, string, domain.Message) error
			}); ok {
				err = service.TransferWithMessage(ctx, u.ID, p.TicketID, p.Department, text, attachment)
			} else {
				return true, a.promptText(ctx, m, m.From.ID, p.TicketID, p.Action, "сначала отправьте вложение отдельным сообщением, затем напишите причину текстом.")
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
