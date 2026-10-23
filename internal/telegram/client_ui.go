package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"bot-bvk/internal/tickets"
)

const clientAcceptedText = "Заявка принята. Специалист ответит в этом чате."
const clientFinishedText = "Заявка завершена по вашему запросу. Если помощь снова понадобится, создайте новую заявку."
const clientCancelDeclinedText = "Заявка #%d остаётся открытой.\nПродолжайте переписку — ваша заявка и её переписка актуальны в этом чате."
const clientCancelDraftLabel = "Отменить подачу заявки"

var clientDraftKeyboard = [][]string{{clientCancelDraftLabel}}
var clientActiveKeyboard = [][]string{{"Мои заявки"}, {"Компания"}, {"Завершить заявку"}}
var clientDraftMenuKeyboard = [][]string{{"Продолжить заявку"}, {"Удалить черновик"}, {"Компания"}}

// Client menus are contextual; the staff keyboard builders are independent.
// The markup shape itself is shared with SendKeyboard through replyKeyboard.
func clientReplyMarkup(keyboard [][]string) map[string]any { return replyKeyboard(keyboard) }
func clientTicketScenario(id int64) string                 { return fmt.Sprintf("client_ticket:%d", id) }
func (a *Adapter) clientScreen(ctx context.Context, m *Message, scenario, text string, keyboard [][]string) error {
	if a.activeUpdate != 0 {
		return a.queueScreen(ctx, m, m.Chat.ID, scenario, text, clientReplyMarkup(keyboard))
	}
	var sent Message
	if err := a.client.Call(ctx, "sendMessage", map[string]any{"chat_id": m.Chat.ID, "text": text, "reply_markup": clientReplyMarkup(keyboard)}, &sent); err != nil {
		return err
	}
	return a.recordAux(ctx, m, m.Chat.ID, clientAuxScenario(m, scenario, text), sent.ID)
}
func (a *Adapter) clientHome(ctx context.Context, m *Message, text string) error {
	a.cleanupBestEffort(ctx, scope(m.Chat.ID, m), "client_home")
	return a.clientScreen(ctx, m, "client_home", text, clientKeyboard)
}
func (a *Adapter) clientMenu(ctx context.Context, m *Message, u domain.User) error {
	t, err := a.store.ActiveTicket(ctx, u.ID)
	if err == nil {
		instruction := "Напишите дополнение здесь — оно попадёт специалисту."
		if t.Status == domain.Waiting {
			instruction = "Специалист ждёт вашего ответа. Напишите, удалось ли проверить решение."
		}
		return a.clientScreen(ctx, m, clientTicketScenario(t.ID), fmt.Sprintf("Заявка №%d · %s\n%s", t.ID, statusName(t.Status), instruction), clientActiveKeyboard)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if d, exists, err := a.getDraft(ctx, u.ID); err != nil {
		return err
	} else if exists && d.Step != "submitted" {
		return a.clientScreen(ctx, m, "client_home", "У вас есть незавершённая заявка. Продолжить заполнение или удалить черновик?", clientDraftMenuKeyboard)
	}
	return a.clientHome(ctx, m, "Поддержка BVK\nВыберите действие.")
}
func (a *Adapter) cancelClientDraft(ctx context.Context, m *Message) error {
	if _, err := a.store.ActiveTicket(ctx, m.Chat.ID); err == nil {
		return a.say(ctx, m, "Заявка уже принята. Для её завершения нажмите «Завершить заявку».", nil)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := a.deleteDraft(ctx, m.Chat.ID); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(m.Chat.ID, m), "draft")
	return a.clientHome(ctx, m, "Черновик удалён. Вы можете создать новую заявку.")
}
func (w *Worker) clientNotification(ctx context.Context, t domain.Ticket, text string) error {
	if t.IsGroup() {
		// Group intake is intentionally silent. This guard also protects old
		// queued operations from ever sending a private menu into a company chat.
		return nil
	}
	accepted := text == "Заявка принята и поставлена в очередь" || text == "Заявка принята и поставлена в очередь."
	cancelled := text == "Заявка завершена по вашему запросу" || text == "Заявка завершена по вашему запросу."
	// An older queued notification must never restore an active menu after completion.
	if !t.Status.Active() && accepted {
		return nil
	}
	keyboard := clientActiveKeyboard
	scenario := clientTicketScenario(t.ID)
	if accepted {
		if store, ok := w.store.(domain.WorkflowStore); ok {
			for _, scenario := range []string{"draft", "client_home"} {
				if err := store.QueueAuxiliaryCleanup(ctx, t.ClientID, t.ClientID, 0, scenario); err != nil {
					return err
				}
			}
		}
		text = clientAcceptedText
		if t.Urgency.Valid() {
			level := urgencyLabel(t.Urgency)
			text = fmt.Sprintf("Заявка №%d принята\nОтдел: %s · Срочность: %s\nСпециалист ответит в этом чате.", t.ID, t.Department.Name(), level)
			calendar := tickets.DefaultCalendar()
			if w.cfg.EndHour > w.cfg.StartHour {
				calendar.StartHour, calendar.EndHour = w.cfg.StartHour, w.cfg.EndHour
			}
			if !calendar.IsWorking(t.CreatedAt) {
				text += fmt.Sprintf("\n\nСейчас нерабочее время. Поддержка начнёт обработку в будни с %02d:00 до %02d:00 (Иркутск).", calendar.StartHour, calendar.EndHour)
			}
		}
	}
	if !t.Status.Active() {
		keyboard = clientKeyboard
		if cancelled {
			text = fmt.Sprintf("Заявка №%d завершена по вашему запросу. Если помощь снова понадобится, создайте новую заявку.", t.ID)
		}
		if store, ok := w.store.(domain.WorkflowStore); ok {
			for _, old := range []string{scenario, "draft", "client_home", actionScenario(t.ID)} {
				if err := store.QueueAuxiliaryCleanup(ctx, t.ClientID, t.ClientID, 0, old); err != nil {
					return err
				}
			}
		}
		scenario = "client_home"
	}
	var sent Message
	var sendErr error
	sent, sendErr = w.textParts(ctx, w.operationSendKey("client_notice"), t.ClientID, 0, text, clientReplyMarkup(keyboard))
	if err := sendErr; err != nil {
		return err
	}
	if store, ok := w.store.(domain.WorkflowStore); ok && sent.ID > 0 {
		if err := store.SaveAuxiliary(ctx, domain.AuxiliaryMessage{UserID: t.ClientID, ChatID: t.ClientID, Scenario: clientAuxScenario(&Message{Chat: Chat{Type: "private"}}, scenario, text), MessageID: sent.ID}); err != nil {
			log.Printf("client notification UI registration failed: %v", err)
		}
	}
	return nil
}

func clientErrorText(err error) string {
	switch {
	case errors.Is(err, domain.ErrConflict):
		return "Данные изменились. Откройте «Мои заявки» или продолжите черновик через /form."
	case errors.Is(err, domain.ErrForbidden):
		return "Это действие недоступно для вашего аккаунта."
	case errors.Is(err, domain.ErrNotFound):
		return "Заявка или этот шаг больше недоступны. Откройте «Мои заявки»."
	case errors.Is(err, domain.ErrInvalid):
		return "Не удалось выполнить действие. Проверьте ввод и попробуйте ещё раз."
	default:
		return "Не удалось выполнить действие. Попробуйте ещё раз."
	}
}

// Approval notifications also replace stale menus and participate in UI cleanup.
func (w *Worker) clientAccessNotification(ctx context.Context, id int64, text string) error {
	keyboard := clientKeyboard
	scenario := "client_home"
	if ticket, err := w.store.ActiveTicket(ctx, id); err == nil {
		keyboard = clientActiveKeyboard
		scenario = clientTicketScenario(ticket.ID)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := w.rate(ctx, id, "sendMessage"); err != nil {
		return err
	}
	var sent Message
	if err := w.client.Call(ctx, "sendMessage", map[string]any{"chat_id": id, "text": text, "reply_markup": clientReplyMarkup(keyboard)}, &sent); err != nil {
		return err
	}
	if store, ok := w.store.(domain.WorkflowStore); ok && sent.ID > 0 {
		return store.SaveAuxiliary(ctx, domain.AuxiliaryMessage{UserID: id, ChatID: id, Scenario: scenario, MessageID: sent.ID})
	}
	return nil
}

// Keep the requested milestones even when their flow's temporary UI is cleaned.
func clientAuxScenario(m *Message, scenario, text string) string {
	if m.Chat.Type != "private" {
		return scenario
	}
	switch strings.TrimSpace(text) {
	case "Создание заявки.", "Создание заявки...", "Выберите направление.", "Опишите проблему текстом. Вложения отправьте после создания заявки.", draftDescriptionText, clientAcceptedText, "Инженер взял заявку в работу.", "Подача заявки отменена. Вы можете создать новую.", "Подача заявки отменена. Вы можете создать новую!", clientHelpText, staffHelpText:
		return "client_kept"
	}
	if (strings.HasPrefix(text, "Ваше обращение передали в отдел ") && strings.HasSuffix(text, "\nИнженеры увидели вашу заявку, скоро возьмут в работу!")) ||
		(strings.HasPrefix(text, "Заявка #") && strings.Contains(text, " принята.\n\nСрочность:")) ||
		(strings.HasPrefix(text, "Заявка №") && strings.Contains(text, " принята\nОтдел:")) ||
		(strings.HasPrefix(text, "Заявка №") && strings.Contains(text, " передана в отдел ")) ||
		(strings.HasPrefix(text, "Заявка №") && strings.Contains(text, " · В работе\n")) ||
		(strings.HasPrefix(text, "Инженер ") && strings.HasSuffix(text, "\nВаша переписка продолжится прямо в этом чате:")) {
		return "client_kept"
	}
	if strings.HasPrefix(text, "Создать заявку?\n") || text == "Создать заявку?" {
		return "client_kept"
	}
	return scenario
}
