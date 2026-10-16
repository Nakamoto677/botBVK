package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

func businessError(err error) bool {
	return errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound)
}
func (a *Adapter) processUpdate(ctx context.Context, entry domain.StoredUpdate) error {
	var u Update
	err := json.Unmarshal(entry.Payload, &u)
	malformed := err != nil
	if !malformed {
		a.activeUpdate = entry.ID
		a.activeActor = 0
		a.answeredCallback = false
		if u.Callback != nil {
			a.activeActor = u.Callback.From.ID
		} else if u.Message != nil {
			a.activeActor = u.Message.From.ID
		} else if u.EditedMessage != nil {
			a.activeActor = u.EditedMessage.From.ID
		}
		a.screenSequence = 0
		err = a.Handle(ctx, u)
		a.activeUpdate = 0
		a.activeActor = 0
	}
	var contention *pgconn.PgError
	if errors.As(err, &contention) && (contention.Code == "40001" || contention.Code == "40P01") {
		if retry, ok := a.store.(domain.RetryInbox); ok {
			// PostgreSQL rolled back this transaction. Keep the update pending;
			// contention must not be reported as a completed business conflict.
			return retry.RetryUpdate(ctx, entry.ID, "transaction contention", 100*time.Millisecond, false)
		}
		return err
	}
	if err == nil || businessError(err) {
		if err != nil {
			a.respondError(ctx, u, err)
		}
		return a.store.FinishUpdate(ctx, entry.ID)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Never convert a database or connection failure into a poison message.
	var db *pgconn.PgError
	var connection *pgconn.ConnectError
	var network *net.OpError
	if errors.As(err, &db) || errors.As(err, &connection) || errors.As(err, &network) || errors.Is(err, io.EOF) || pgconn.SafeToRetry(err) || pgconn.Timeout(err) {
		return err
	}
	if e := a.store.Ping(ctx); e != nil {
		return e
	}
	retry, ok := a.store.(domain.RetryInbox)
	if !ok {
		return err
	}
	var api *APIError
	permanent := malformed
	delay := time.Duration(1<<min(entry.Attempts+1, 8)) * time.Second
	if errors.As(err, &api) {
		permanent = api.Code >= 400 && api.Code < 500 && api.Code != 429
		if api.RetryAfter > 0 {
			delay = time.Duration(api.RetryAfter) * time.Second
		}
	}
	quarantine := permanent || entry.Attempts+1 >= 5
	if e := retry.RetryUpdate(ctx, entry.ID, err.Error(), delay, quarantine); e != nil {
		return e
	}
	log.Printf("inbox update %d attempt %d quarantined=%t: %v", entry.ID, entry.Attempts+1, quarantine, err)
	if quarantine {
		w := NewWorker(a.client, a.store, a.cfg)
		return w.notifyAdmins(ctx, fmt.Sprintf("Входящее действие %d изолировано после ошибки; исходные данные сохранены, зависимые действия и удаление архива заблокированы.", entry.ID))
	}
	return nil
}

func (a *Adapter) queueScreen(ctx context.Context, m *Message, actor int64, scenario, text string, markup any, descriptionGeneration ...string) error {
	if a.activeActor != 0 {
		actor = a.activeActor
	}
	a.screenSequence++
	payload := map[string]any{"user_id": actor, "chat_id": m.Chat.ID, "topic_id": m.ThreadID, "text": text, "markup": markup, "scenario": clientAuxScenario(m, scenario, text), "ui_key": fmt.Sprintf("%d:%d", a.activeUpdate, a.screenSequence)}
	var available time.Time
	if len(descriptionGeneration) > 0 {
		payload["draft_generation"] = descriptionGeneration[0]
		payload["draft_step"] = "description"
		available = time.Now().Add(350 * time.Millisecond)
	}
	if buttons, ok := markup.(Markup); ok && m.Chat.Type == "private" {
		for _, row := range buttons.Buttons {
			for _, button := range row {
				if strings.HasPrefix(button.Data, "submit:") {
					payload["draft_generation"] = strings.TrimPrefix(button.Data, "submit:")
					// Briefly collect consecutive messages before showing their preview.
					available = time.Now().Add(350 * time.Millisecond)
				}
			}
		}
	}
	raw, _ := json.Marshal(payload)
	return a.store.Transact(ctx, func(tx domain.Tx) error {
		return tx.Enqueue(ctx, domain.Operation{Kind: "ui_screen", Payload: raw, State: "pending", AvailableAt: available})
	})
}

func (w *Worker) uiScreen(ctx context.Context, op domain.Operation) error {
	var p struct {
		UserID          int64           `json:"user_id"`
		ChatID          int64           `json:"chat_id"`
		TopicID         int64           `json:"topic_id"`
		Text            string          `json:"text"`
		Markup          json.RawMessage `json:"markup"`
		Scenario        string          `json:"scenario"`
		DraftGeneration string          `json:"draft_generation"`
		DraftStep       string          `json:"draft_step"`
	}
	if json.Unmarshal(op.Payload, &p) != nil {
		return domain.ErrInvalid
	}
	current := func() (bool, error) {
		if p.DraftGeneration == "" {
			return true, nil
		}
		store, ok := w.store.(domain.BoundDraftStore)
		if !ok {
			return true, nil
		}
		d, err := store.LoadDraft(ctx, p.UserID)
		if errors.Is(err, domain.ErrNotFound) {
			return false, nil
		}
		step := p.DraftStep
		if step == "" {
			step = "confirm"
		}
		return d.Generation == p.DraftGeneration && d.Step == step, err
	}
	if valid, err := current(); err != nil || !valid {
		return err
	}
	parts := splitText(p.Text, messageLimit)
	for i, text := range parts {
		sent, err := w.confirmedPart(ctx, fmt.Sprintf("ui:%d", op.ID), i, func() (Message, error) {
			if err := w.rate(ctx, p.ChatID, "sendMessage"); err != nil {
				return Message{}, err
			}
			if valid, err := current(); err != nil || !valid {
				if err != nil {
					return Message{}, err
				}
				return Message{}, errObsoleteScreen
			}
			payload := map[string]any{"chat_id": p.ChatID, "text": text}
			if p.TopicID != 0 {
				payload["message_thread_id"] = p.TopicID
			}
			if i == len(parts)-1 && hasUsableKeyboard(p.Markup) {
				payload["reply_markup"] = p.Markup
			}
			var result Message
			err := w.client.Call(ctx, "sendMessage", payload, &result)
			return result, err
		})
		if errors.Is(err, errObsoleteScreen) {
			return nil
		}
		if err != nil {
			return err
		}
		if store, ok := w.store.(domain.WorkflowStore); ok && p.Scenario != "" && sent.ID > 0 {
			if err := store.SaveAuxiliary(ctx, domain.AuxiliaryMessage{UserID: p.UserID, ChatID: p.ChatID, TopicID: p.TopicID, Scenario: p.Scenario, MessageID: sent.ID}); err != nil {
				return err
			}
		}
	}
	return nil
}

var errObsoleteScreen = errors.New("obsolete queued screen")

// hasUsableKeyboard reports whether a stored keyboard is worth sending. Checking only
// for the literal "null" was not enough: a screen whose keyboard was built from an
// empty button list stored `{"inline_keyboard":null}`, which passes that check and
// makes Telegram answer `field "inline_keyboard" must be of type Array`. The screen
// was then lost for good, because a 400 is a permanent failure.
//
// A keyboard is sent only when it is a JSON object that actually carries rows in one
// of the two keyboard forms. Anything else, including malformed JSON, is omitted so
// the message itself still arrives.
func hasUsableKeyboard(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return false
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return false
	}
	for _, field := range []string{"inline_keyboard", "keyboard"} {
		rows, present := object[field]
		if !present {
			continue
		}
		list, ok := rows.([]any)
		if !ok {
			// The field exists but is null or not an array: Telegram rejects the whole
			// request, so this keyboard cannot be used.
			return false
		}
		if len(list) > 0 {
			return true
		}
		// An empty row list draws nothing, so the field is pointless either way.
		return false
	}
	return false
}
