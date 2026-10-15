package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const draftDescriptionText = "Опишите проблему одним или несколькими сообщениями. Можно добавить вложения. Когда всё готово, нажмите «Отправить»."

func (a *Adapter) appendDraft(ctx context.Context, m *Message, d draft) error {
	for _, id := range d.MessageIDs {
		if m.ID != 0 && id == m.ID {
			return a.confirmDraft(ctx, m, d)
		}
	}
	text := strings.TrimSpace(messageText(m))
	attachment := normalise(m, false)
	if text == "" && attachment.FileID == "" {
		return a.say(ctx, m, "Добавьте описание текстом или подписью к вложению.", nil)
	}
	if text != "" {
		if d.Text != "" {
			d.Text += "\n\n"
		}
		d.Text += text
	}
	if attachment.FileID != "" {
		found := false
		for _, v := range d.Attachments {
			found = found || v.TelegramChatID == attachment.TelegramChatID && v.TelegramMessageID == attachment.TelegramMessageID
		}
		if !found {
			d.Attachments = append(d.Attachments, attachment)
		}
	}
	if m.ID != 0 {
		d.MessageIDs = append(d.MessageIDs, m.ID)
	}
	d.Generation = a.formGeneration(m)
	d.Step = "confirm"
	if d.Text == "" {
		d.Step = "description"
	}
	if err := a.saveDraft(ctx, m.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(m.From.ID, m), "draft")
	return a.confirmDraft(ctx, m, d)
}

func draftGeneration() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func draftButtons(d draft) [][]Button {
	return [][]Button{{{Text: "Отправить", Data: "submit:" + d.Generation}, {Text: clientCancelDraftLabel, Data: "cancel:" + d.Generation}}}
}
func draftDepartments(d draft) [][]Button {
	buttons := departmentButtons("new")
	for i := range buttons {
		for j := range buttons[i] {
			buttons[i][j].Data += ":" + d.Generation
		}
	}
	return append(buttons, []Button{{Text: clientCancelDraftLabel, Data: "cancel:" + d.Generation}})
}
func (a *Adapter) staleDraft(ctx context.Context, c *Callback) error {
	_ = a.client.Answer(ctx, c.ID, "Этот экран больше не актуален. Откройте текущее состояние: /form или /current.")
	if c.Message != nil {
		_ = a.client.Call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": c.Message.Chat.ID, "message_id": c.Message.ID, "reply_markup": Markup{Buttons: [][]Button{}}}, nil)
	}
	return nil
}
func (a *Adapter) boundDraft(ctx context.Context, c *Callback, parts []string, step string) (draft, bool, error) {
	if c.Message == nil || c.Message.Chat.Type != "private" || c.Message.Chat.ID != c.From.ID {
		return draft{}, false, domain.ErrForbidden
	}
	d, exists, err := a.getDraft(ctx, c.From.ID)
	if err != nil {
		return d, false, err
	}
	token := ""
	if len(parts) > 1 {
		token = parts[len(parts)-1]
	}
	if d.Step == "submitted" || !exists || d.Generation == "" || token != d.Generation || (step != "" && d.Step != step) {
		return d, false, a.staleDraft(ctx, c)
	}
	return d, true, nil
}
func (a *Adapter) resumeDraft(ctx context.Context, m *Message) error {
	d, ok, err := a.getDraft(ctx, m.From.ID)
	if err != nil {
		return err
	}
	if !ok || d.Step == "submitted" {
		return a.say(ctx, m, "Нет незавершённой формы. /current — активная заявка, /new — новая.", nil)
	}
	if d.Generation == "" {
		d.Generation = draftGeneration()
		d.Step = "description"
		if d.Text != "" {
			d.Step = "confirm"
		}
		if err = a.saveDraft(ctx, m.From.ID, d); err != nil {
			return err
		}
	}
	if d.Step == "department" {
		return a.sayAux(ctx, m, m.From.ID, "draft", "Выберите направление.", draftDepartments(d))
	}
	if d.Step == "confirm" {
		return a.confirmDraft(ctx, m, d)
	}
	return a.clientScreen(ctx, m, "draft", draftDescriptionText, clientDraftKeyboard)
}
func (a *Adapter) confirmDraft(ctx context.Context, m *Message, d draft) error {
	if d.Text == "" {
		text := fmt.Sprintf("Вложений принято: %d. Теперь опишите проблему текстом.", len(d.Attachments))
		if a.activeUpdate != 0 {
			return a.queueScreen(ctx, m, m.From.ID, "draft", text, nil, d.Generation)
		}
		return a.say(ctx, m, text, nil)
	}
	text := "Создать заявку?\n" + previewText(d.Text, 3000)
	if len(d.Attachments) > 0 {
		text += fmt.Sprintf("\nВложений принято: %d. Они сохранятся вместе с заявкой.", len(d.Attachments))
	}
	return a.sayAux(ctx, m, m.From.ID, "draft", text, draftButtons(d))
}
func (a *Adapter) submitDraft(ctx context.Context, c *Callback, d draft) error {
	if service, ok := a.service.(interface {
		CreateDraft(context.Context, int64, string) (domain.Ticket, error)
	}); ok {
		_, err := service.CreateDraft(ctx, c.From.ID, d.Generation)
		return err
	}
	_, err := a.service.Create(ctx, c.From.ID, d.Department, d.Text)
	if err == nil {
		d.Step = "submitted"
		err = a.saveDraft(ctx, c.From.ID, d)
	}
	return err
}
func draftNotFound(err error) bool { return errors.Is(err, domain.ErrNotFound) }

func (a *Adapter) formGeneration(m *Message) string {
	if a.activeUpdate == 0 {
		return draftGeneration()
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("draft:%d:%d:%d", a.activeUpdate, m.Chat.ID, m.From.ID)))
	return hex.EncodeToString(sum[:12])
}
