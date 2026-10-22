package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

const draftDescriptionText = "Опишите, с чем нужна помощь.\nУкажите, что произошло и как это влияет на работу. При необходимости приложите фото или файл."
const draftUrgencyText = "Насколько срочно требуется помощь?\n\nСрочно — работа остановлена или возникла критическая проблема, требующая оперативного вмешательства.\n\nВ обычном порядке — нужна консультация, настройка или решение проблемы, которая не блокирует работу."

func urgencyLabel(u domain.Urgency) string {
	switch u {
	case domain.Urgent:
		return "🔴 Срочно"
	case domain.Routine:
		return "в обычном порядке"
	default:
		return "не указана"
	}
}

func draftUrgencyButtons(d draft) [][]Button {
	back := Button{Text: "Назад", Data: "draftback:" + d.Generation}
	if d.Step == "change_urgency" {
		back = Button{Text: "Назад к заявке", Data: "draftreview:" + d.Generation}
	}
	return [][]Button{
		{{Text: "🔴 Срочно", Data: "urgency:urgent:" + d.Generation}},
		{{Text: "В обычном порядке", Data: "urgency:routine:" + d.Generation}},
		{back},
		{{Text: clientCancelDraftLabel, Data: "cancel:" + d.Generation}},
	}
}

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
	return [][]Button{{{Text: "Отправить", Data: "submit:" + d.Generation}}, {{Text: "Изменить срочность", Data: "changeurgency:" + d.Generation}}, {{Text: clientCancelDraftLabel, Data: "cancel:" + d.Generation}}}
}
func (a *Adapter) draftDepartments(d draft) [][]Button {
	buttons := a.departmentButtons("new")
	for i := range buttons {
		for j := range buttons[i] {
			buttons[i][j].Data += ":" + d.Generation
		}
	}
	return append(buttons, []Button{{Text: clientCancelDraftLabel, Data: "cancel:" + d.Generation}})
}

// staleDraft tells the user that the tapped screen is outdated, drops its buttons
// and marks the callback answered. The marker is what keeps the shared error path
// from answering the same callback a second time with a generic message and
// replacing this explanation.
func (a *Adapter) staleDraft(ctx context.Context, c *Callback) error {
	_ = a.client.Answer(ctx, c.ID, "Этот экран больше не актуален. Откройте текущее состояние: /form или /current.")
	a.markCallbackAnswered()
	if c.Message != nil {
		_ = a.client.HideMessageKeyboard(ctx, c.Message.Chat.ID, c.Message.ID)
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
		// A rejected screen is a business conflict, not a silent success: return
		// the typed error so the caller stops instead of finishing the update as
		// if it had succeeded, which also left the client spinner hanging.
		_ = a.staleDraft(ctx, c)
		return d, false, domain.ErrConflict
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
	if d.Department != "" && !a.departmentEnabled(d.Department) {
		d.Department = ""
		d.Step = "department"
		if err = a.saveDraft(ctx, m.From.ID, d); err != nil {
			return err
		}
	}
	if a.departmentEnabled(d.Department) && !d.Urgency.Valid() && d.Step != "department" && d.Step != "urgency" {
		d.Step = "urgency"
		if err = a.saveDraft(ctx, m.From.ID, d); err != nil {
			return err
		}
	}
	if d.Step == "department" {
		return a.sayAux(ctx, m, m.From.ID, "draft", "Выберите направление.", a.draftDepartments(d))
	}
	if d.Step == "urgency" || d.Step == "change_urgency" {
		return a.sayAux(ctx, m, m.From.ID, "draft", draftUrgencyText, draftUrgencyButtons(d))
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
	text := fmt.Sprintf("Проверьте заявку перед отправкой\n\nОтдел: %s\nСрочность: %s\n\nОписание:\n%s", d.Department.Name(), urgencyLabel(d.Urgency), previewText(d.Text, 3000))
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
func (a *Adapter) formGeneration(m *Message) string {
	if a.activeUpdate == 0 {
		return draftGeneration()
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("draft:%d:%d:%d", a.activeUpdate, m.Chat.ID, m.From.ID)))
	return hex.EncodeToString(sum[:12])
}
