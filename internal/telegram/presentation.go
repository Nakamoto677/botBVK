package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"bot-bvk/internal/domain"
)

func displayName(ctx context.Context, store domain.Store, id int64, fallback string) (string, error) {
	if id == 0 {
		return fallback, nil
	}
	u, err := store.User(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return fallback, nil
	}
	if err != nil {
		return "", err
	}
	if name := strings.TrimSpace(u.Name); name != "" {
		return name, nil
	}
	return fallback, nil
}
func companyName(ctx context.Context, store domain.Store, id int64) (string, error) {
	companies, err := store.Companies(ctx)
	if err != nil {
		return "", err
	}
	for _, company := range companies {
		if company.ID == id && strings.TrimSpace(company.Name) != "" {
			return company.Name, nil
		}
	}
	return "Компания", nil
}
func departmentName(d domain.Department) string {
	switch d {
	case domain.OneC:
		return "1С"
	case domain.IT:
		return "IT"
	case domain.Installation:
		return "Монтаж"
	case domain.Field:
		return "Выезд"
	}
	return "Поддержка"
}
func statusName(s domain.Status) string {
	switch s {
	case domain.New:
		return "Новая"
	case domain.InProgress:
		return "В работе"
	case domain.Waiting:
		return "Ожидает клиента"
	case domain.Closed:
		return "Закрыта"
	case domain.Cancelled:
		return "Отменена"
	}
	return "Заявка"
}
func ticketText(t domain.Ticket, client, company, assignee string) string {
	description := []rune(t.Description)
	if len(description) > 2400 {
		description = append(description[:2400], []rune("…")...)
	}
	if t.AssigneeID == 0 {
		assignee = "Не назначен"
	}
	return fmt.Sprintf("Заявка #%d · %s\nКлиент: %s\n%s\nОтдел: %s · %s\nИсполнитель: %s", t.ID, company, client, string(description), departmentName(t.Department), statusName(t.Status), assignee)
}
func clientTicketButtons(t domain.Ticket) [][]Button {
	id := fmt.Sprint(t.ID)
	buttons := [][]Button{{{Text: "История", Data: "history:" + id}}}
	if t.Status.Active() {
		buttons = append(buttons, []Button{{Text: "Отменить заявку", Data: "cancel:" + id}})
	} else if t.Status == domain.Closed {
		buttons = append(buttons, []Button{{Text: "Открыть повторно", Data: "reopen:" + id}})
	}
	return buttons
}

var menuAliases = map[string]string{"Создать заявку": "/new", "Помощь": "/help", "Отменить заявку": "/cancel_current", "Отменить подачу заявки": "/cancel_draft", "📝 Новая заявка": "/new", "📋 Мои заявки": "/tickets", "💬 Текущая заявка": "/current", "❌ Отменить заявку": "/cancel_current", "ℹ️ Помощь": "/help", "📊 Отчёт": "/report"}
var staffKeyboard = [][]string{{"📋 Мои заявки", "📊 Отчёт"}, {"ℹ️ Помощь"}}
var engineerKeyboard = [][]string{{"📋 Мои заявки"}, {"ℹ️ Помощь"}}
var clientKeyboard = [][]string{{"Создать заявку", "Помощь"}}

func (a *Adapter) menu(ctx context.Context, m *Message, u domain.User) error {
	if u.Role == "engineer" {
		return a.client.SendKeyboard(ctx, m.Chat.ID, "Работайте с кнопками заявок в группе отдела. Выберите список заявок или помощь.", engineerKeyboard)
	}
	if u.Role != "client" {
		return a.client.SendKeyboard(ctx, m.Chat.ID, "Работайте с кнопками заявок в группе отдела. Выберите список заявок, отчёт или помощь.", staffKeyboard)
	}
	return a.clientMenu(ctx, m, u)
}
func (a *Adapter) help(ctx context.Context, m *Message, u domain.User) error {
	if u.Role != "client" {
		return a.say(ctx, m, "В группе отдела: возьмите заявку кнопкой. Обычные сообщения и вложения инженеров текущего отдела в актуальном топике автоматически уходят клиенту. Служебный ввод для закрытия и передачи не пересылается. В очереди нажмите Закрыть задачу, в топике — Закрыть тикет; Передать предложит выбрать отдел и причину. /history ID — переписка, /tickets — список, /report — отчёт для администратора. Для отмены ввода нажмите Отменить.", nil)
	}
	return a.sayAux(ctx, m, u.ID, "client_home", "Чтобы обратиться в поддержку, нажмите «Создать заявку», выберите отдел и опишите проблему. Специалист ответит прямо здесь.\n\nИстория: /tickets. Повторное открытие: /reopen ID.", nil)
}
func (a *Adapter) showTicket(ctx context.Context, m *Message, u domain.User, t domain.Ticket) error {
	if !historyAuthorized(u, t) || !a.historyDestination(m, u, t) {
		return domain.ErrForbidden
	}
	name, err := displayName(ctx, a.store, t.ClientID, "Клиент")
	if err != nil {
		return err
	}
	company, err := companyName(ctx, a.store, t.CompanyID)
	if err != nil {
		return err
	}
	assignee, err := displayName(ctx, a.store, t.AssigneeID, "Специалист")
	if err != nil {
		return err
	}
	buttons := clientTicketButtons(t)
	if u.Role == "client" && t.Status.Active() {
		buttons = [][]Button{{{Text: "Отменить заявку", Data: fmt.Sprintf("cancel:%d", t.ID)}}}
	}
	if u.Role == "engineer" {
		buttons = [][]Button{{{Text: "История", Data: fmt.Sprintf("history:%d", t.ID)}}}
		team := a.cfg.Departments[t.Department]
		topicOrMessage := t.TopicID
		label := "Открыть топик"
		if topicOrMessage == 0 {
			topicOrMessage = t.QueueMessageID
			label = "Открыть очередь"
		}
		if team.ChatID < 0 && topicOrMessage != 0 {
			chat := strings.TrimPrefix(fmt.Sprint(team.ChatID), "-100")
			buttons = append(buttons, []Button{{Text: label, URL: fmt.Sprintf("https://t.me/c/%s/%d", chat, topicOrMessage)}})
		}
	}
	return a.say(ctx, m, ticketText(t, name, company, assignee), buttons)
}
func (a *Adapter) updateName(ctx context.Context, u domain.User, p Person) error {
	name := strings.TrimSpace(p.FirstName + " " + p.LastName)
	if !u.Approved || name == "" || name == u.Name {
		return nil
	}
	return a.store.Transact(ctx, func(tx domain.Tx) error {
		current, err := tx.GetUser(ctx, u.ID)
		if err != nil {
			return err
		}
		current.Name = name
		return tx.SaveUser(ctx, current)
	})
}
