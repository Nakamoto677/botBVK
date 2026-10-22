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
	if catalog, ok := store.(interface {
		Company(context.Context, int64) (domain.Company, error)
	}); ok {
		c, err := catalog.Company(ctx, id)
		if errors.Is(err, domain.ErrNotFound) {
			return "Компания", nil
		}
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(c.Name) != "" {
			return c.Name, nil
		}
		return "Компания", nil
	}
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
	return d.Name()
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
func clientTicketButtons(t domain.Ticket) [][]Button {
	id := fmt.Sprint(t.ID)
	buttons := [][]Button{{{Text: "История заявки", Data: "history:" + id}}}
	if t.Status.Active() {
		buttons = append(buttons, []Button{{Text: "Завершить заявку", Data: "cancel:" + id}})
	}
	return buttons
}

var menuAliases = map[string]string{"Создать заявку": "/new", "Мои заявки": "/tickets", "Помощь": "/help", "Продолжить заявку": "/form", "Удалить черновик": "/cancel_draft", "Завершить заявку": "/cancel_current", "Отменить заявку": "/cancel_current", "Отменить подачу заявки": "/cancel_draft", "📝 Новая заявка": "/new", "📋 Мои заявки": "/tickets", "💬 Текущая заявка": "/current", "❌ Отменить заявку": "/cancel_current", "ℹ️ Помощь": "/help", "📊 Отчёт": "/report"}
var staffKeyboard = [][]string{{"📋 Мои заявки", "📊 Отчёт"}, {"ℹ️ Помощь"}}
var engineerKeyboard = [][]string{{"📋 Мои заявки"}, {"ℹ️ Помощь"}}
var clientKeyboard = [][]string{{"Создать заявку"}, {"Мои заявки", "Помощь"}}

func (a *Adapter) menu(ctx context.Context, m *Message, u domain.User) error {
	if u.Role == "engineer" {
		return a.client.SendKeyboard(ctx, m.Chat.ID, "Работайте с кнопками заявок в группе отдела. Выберите список заявок или помощь.", engineerKeyboard)
	}
	if u.Role != "client" {
		return a.client.SendKeyboard(ctx, m.Chat.ID, "Работайте с кнопками заявок в группе отдела. Выберите список заявок, отчёт или помощь.", staffKeyboard)
	}
	return a.clientMenu(ctx, m, u)
}

// Help answers are retained. They are not a transient step of a flow: a person taps
// Помощь to read something, and a screen registered under the disposable home
// scenario was wiped by the next cleanup, which is why the button looked dead.
const clientHelpText = "Как получить помощь\n\n1. Нажмите «Создать заявку».\n2. Выберите отдел и срочность, опишите проблему.\n3. Ответ специалиста придёт в этот чат.\n\nПрошлые обращения — «Мои заявки». Незавершённую форму откройте командой /form.\n\nДополнительные команды: /ticket ID — карточка, /history ID — переписка."
const staffHelpText = "Работа с заявкой\n\nВ очереди нажмите «Взять в работу». Пишите клиенту в актуальном топике: обычные сообщения и вложения доставляются автоматически.\n\n«Передать» предложит другой отдел и комментарий. «Завершить заявку» запросит итог для клиента. Служебный ввод не пересылается.\n\n/tickets — список, /history ID — переписка, /report — отчёт администратора."

func (a *Adapter) help(ctx context.Context, m *Message, u domain.User) error {
	if u.Role != "client" {
		return a.say(ctx, m, staffHelpText, nil)
	}
	// The answer keeps the client menu: its own text tells the person to press
	// «Создать заявку», and without the keyboard they would have no way on.
	return a.clientScreen(ctx, m, "client_home", clientHelpText, clientKeyboard)
}
func (a *Adapter) showTicket(ctx context.Context, m *Message, u domain.User, t domain.Ticket) error {
	if !historyAuthorized(u, t) || !a.historyDestination(m, u, t) {
		return domain.ErrForbidden
	}
	return a.historyPage(ctx, m, u, t, 0, 0)
}
func (a *Adapter) updateName(ctx context.Context, u domain.User, p Person) error {
	name := strings.TrimSpace(p.FirstName + " " + p.LastName)
	if (name == "" || name == u.Name) && p.Username == u.Username {
		return nil
	}
	return a.store.Transact(ctx, func(tx domain.Tx) error {
		current, err := tx.GetUser(ctx, u.ID)
		if err != nil {
			return err
		}
		if name != "" {
			current.Name = name
		}
		current.Username = p.Username
		return tx.SaveUser(ctx, current)
	})
}

// A username is optional and can change or be removed by the Telegram user.
func clientContact(ctx context.Context, store domain.Store, id int64) (string, error) {
	u, err := store.User(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return "Клиент · Telegram: не указан", nil
	}
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(u.Name)
	if name == "" {
		name = "Клиент"
	}
	name = engineerTextBound(strings.Join(strings.Fields(name), " "), 60)
	username := strings.TrimSpace(u.Username)
	if username == "" {
		return name + " · Telegram: не указан", nil
	}
	return name + " · @" + username, nil
}
