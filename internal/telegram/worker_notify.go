package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"bot-bvk/internal/domain"
)

// Operations that never touch a ticket: screens, access notifications and
// auxiliary cleanup. They are handled before the ticket is loaded, so they stay
// out of the ticket dispatcher.

func (w *Worker) accessRequestNotification(ctx context.Context, op domain.Operation) error {
	var request struct {
		RequestID int64 `json:"request_id"`
		UserID    int64 `json:"user_id"`
		CompanyID int64 `json:"company_id"`
	}
	if err := json.Unmarshal(op.Payload, &request); err != nil {
		return domain.ErrInvalid
	}
	company, err := companyName(ctx, w.store, request.CompanyID)
	if err != nil {
		return err
	}
	user, err := displayName(ctx, w.store, request.UserID, "Пользователь")
	if err != nil {
		return err
	}
	for _, id := range w.cfg.AdminIDs {
		buttons := [][]Button{{{Text: "Одобрить доступ", Data: fmt.Sprintf("approve:%d", request.RequestID)}, {Text: "Отклонить доступ", Data: fmt.Sprintf("reject:%d", request.RequestID)}}}
		body := fmt.Sprintf("Запрос доступа №%d\nПользователь: %s · ID %d\nКомпания: %s\nПроверьте принадлежность пользователя перед решением.", request.RequestID, user, request.UserID, company)
		if _, err := w.send(ctx, id, 0, body, buttons); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) accessNotification(ctx context.Context, payload operationPayload) error {
	user, err := w.store.User(ctx, payload.UserID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if user.Approved && user.Role == "client" {
		return w.clientAccessNotification(ctx, payload.UserID, payload.Text+"\nВыберите действие в меню.")
	}
	_, err = w.send(ctx, payload.UserID, 0, payload.Text, nil)
	return err
}

func (w *Worker) deleteAuxMessage(ctx context.Context, payload operationPayload) error {
	if payload.ChatID == 0 || payload.MessageID <= 0 {
		return domain.ErrInvalid
	}
	if err := w.rate(ctx, payload.ChatID, "deleteMessage"); err != nil {
		return err
	}
	err := w.client.Call(ctx, "deleteMessage", map[string]any{"chat_id": payload.ChatID, "message_id": payload.MessageID}, nil)
	if auxiliaryDeleteBlocked(err) {
		// Telegram can refuse deletion of an old service screen. Its buttons can
		// still be disabled, and the screen's cleanup is otherwise finished.
		if editErr := messageEditError(w.client.HideMessageKeyboard(ctx, payload.ChatID, payload.MessageID)); editErr != nil {
			slog.Warn("could not hide keyboard on undeletable auxiliary message", "chat", payload.ChatID, "message", payload.MessageID, "error", editErr)
		}
		return nil
	}
	return auxiliaryDeleteError(err)
}
