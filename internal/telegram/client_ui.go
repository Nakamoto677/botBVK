package telegram

import (
	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
)

const clientAcceptedText = "Заявка принята!\nВ ближайшее время в этот чат вам ответит специалист."
const clientFinishedText = "Рады, что ваша проблема решилась.\nОбращайтесь в любое время, мы всегда рады помочь!"
const clientCancelDraftLabel = "Отменить подачу заявки"

var clientDraftKeyboard = [][]string{{clientCancelDraftLabel}}
var clientActiveKeyboard = [][]string{{"Отменить заявку"}}

// Client menus are contextual; the staff keyboard builders are independent.
func clientReplyMarkup(keyboard [][]string) map[string]any {
	rows := [][]map[string]string{}
	for _, row := range keyboard {
		items := []map[string]string{}
		for _, label := range row {
			items = append(items, map[string]string{"text": label})
		}
		rows = append(rows, items)
	}
	return map[string]any{"keyboard": rows, "resize_keyboard": true, "is_persistent": true}
}
func clientTicketScenario(id int64) string { return fmt.Sprintf("client_ticket:%d", id) }
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
		return a.clientScreen(ctx, m, clientTicketScenario(t.ID), "Напишите сообщение — специалист ответит в этом чате.", clientActiveKeyboard)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if d, exists, err := a.getDraft(ctx, u.ID); err != nil {
		return err
	} else if exists && d.Step != "submitted" {
		return a.clientScreen(ctx, m, "draft", "Опишите проблему или отмените подачу заявки.", clientDraftKeyboard)
	}
	return a.clientHome(ctx, m, "Чем можем помочь?")
}
func (a *Adapter) cancelClientDraft(ctx context.Context, m *Message) error {
	if _, err := a.store.ActiveTicket(ctx, m.Chat.ID); err == nil {
		return a.say(ctx, m, "Заявка уже принята. Для её завершения нажмите «Отменить заявку».", nil)
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := a.deleteDraft(ctx, m.Chat.ID); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(m.Chat.ID, m), "draft")
	return a.clientHome(ctx, m, "Подача заявки отменена. Вы можете создать новую.")
}
func (w *Worker) clientNotification(ctx context.Context, t domain.Ticket, text string) error {
	// Sanitize notifications queued by older versions before the service comment
	// became private, including notifications processed after a reopen.
	if strings.HasPrefix(text, "Заявка закрыта. Результат: ") {
		text = tickets.ClientClosedNotification
	}
	accepted := text == "Заявка принята и поставлена в очередь."
	cancelled := text == "Заявка завершена по вашему запросу."
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
	}
	if !t.Status.Active() {
		keyboard = clientKeyboard
		if cancelled {
			text = clientFinishedText
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
	if err := w.rate(ctx, t.ClientID); err != nil {
		return err
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
		return "Этот шаг уже завершён или заявка изменилась. Откройте меню заново: /menu."
	case errors.Is(err, domain.ErrForbidden):
		return "Это действие недоступно для вашего аккаунта."
	case errors.Is(err, domain.ErrNotFound):
		return "Заявка или этот шаг больше не доступны. Откройте меню: /menu."
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
	if err := w.rate(ctx, id); err != nil {
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
	case "Создание заявки.", "Выберите направление.", draftDepartmentText, "Опишите проблему текстом. Вложения отправьте после создания заявки.", draftDescriptionText, clientAcceptedText, "Инженер взял заявку в работу.", "Подача заявки отменена. Вы можете создать новую.":
		return "client_kept"
	}
	if strings.HasPrefix(text, "Создать заявку?\n") || text == "Создать заявку?" {
		return "client_kept"
	}
	return scenario
}
