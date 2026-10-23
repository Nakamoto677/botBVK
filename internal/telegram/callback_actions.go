package telegram

import (
	"context"
	"errors"
	"strconv"

	"bot-bvk/internal/domain"
)

// Registered-callback actions live here so that callback stays a dispatcher.
// Every method receives the parsed callback and the raw token parts, returns the
// business error, and leaves acknowledgement to the caller.

func (a *Adapter) callbackNewDraft(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "department")
	if err != nil || !valid {
		return err
	}
	if len(parts) != 3 || !a.departmentEnabled(domain.Department(parts[1])) {
		return domain.ErrInvalid
	}
	user, err := a.store.User(ctx, c.From.ID)
	if err != nil {
		return err
	}
	if !user.Approved || user.Role != "client" {
		return domain.ErrForbidden
	}
	if _, err = a.store.ActiveTicket(ctx, c.From.ID); err == nil {
		return domain.ErrConflict
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	d.Department = domain.Department(parts[1])
	returnToReview := d.Urgency.Valid() && d.Text != ""
	d.Generation = a.formGeneration(c.Message)
	d.Step = "urgency"
	if returnToReview {
		d.Step = "confirm"
	}
	if err = a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	if returnToReview {
		return a.confirmDraft(ctx, c.Message, d)
	}
	return a.sayAux(ctx, c.Message, c.From.ID, "draft", draftUrgencyText, draftUrgencyButtons(d))
}

func (a *Adapter) callbackChangeDepartment(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "confirm")
	if err != nil || !valid {
		return err
	}
	d.Step = "department"
	d.Generation = a.formGeneration(c.Message)
	if err = a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	return a.sayAux(ctx, c.Message, c.From.ID, "draft", "Изменить отдел\nКакой отдел поможет с проблемой?", a.draftDepartments(d))
}

func (a *Adapter) callbackChangeDescription(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "confirm")
	if err != nil || !valid {
		return err
	}
	d.Step = "edit_description"
	d.Generation = a.formGeneration(c.Message)
	if err = a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	return a.clientScreen(ctx, c.Message, "draft", draftEditDescriptionText, clientDraftKeyboard)
}

func (a *Adapter) callbackUrgency(ctx context.Context, c *Callback, parts []string) error {
	if len(parts) != 3 {
		return domain.ErrInvalid
	}
	d, valid, err := a.boundDraft(ctx, c, parts, "")
	if err != nil || !valid {
		return err
	}
	if d.Step != "urgency" && d.Step != "change_urgency" {
		return domain.ErrConflict
	}
	u := domain.Urgency(parts[1])
	if !u.Valid() {
		return domain.ErrInvalid
	}
	d.Urgency = u
	d.Generation = a.formGeneration(c.Message)
	if d.Step == "change_urgency" {
		d.Step = "confirm"
	} else {
		d.Step = "description"
	}
	if err := a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	if d.Step == "confirm" {
		return a.confirmDraft(ctx, c.Message, d)
	}
	return a.clientScreen(ctx, c.Message, "draft", draftDescriptionText, clientDraftKeyboard)
}

func (a *Adapter) callbackChangeUrgency(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "confirm")
	if err != nil || !valid {
		return err
	}
	d.Step = "change_urgency"
	if err := a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	return a.sayAux(ctx, c.Message, c.From.ID, "draft", draftUrgencyText, draftUrgencyButtons(d))
}

func (a *Adapter) callbackDraftBack(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "urgency")
	if err != nil || !valid {
		return err
	}
	d.Step = "department"
	d.Generation = a.formGeneration(c.Message)
	if err := a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	return a.sayAux(ctx, c.Message, c.From.ID, "draft", "Новая заявка · 1 из 3\nКакой отдел поможет с проблемой?", a.draftDepartments(d))
}

func (a *Adapter) callbackDraftReview(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "change_urgency")
	if err != nil || !valid {
		return err
	}
	d.Step = "confirm"
	if err := a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	return a.confirmDraft(ctx, c.Message, d)
}

func (a *Adapter) callbackSubmitDraft(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "confirm")
	if err != nil || !valid {
		return err
	}
	if !a.departmentEnabled(d.Department) {
		d.Department = ""
		d.Step = "department"
		if err = a.saveDraft(ctx, c.From.ID, d); err != nil {
			return err
		}
		return a.sayAux(ctx, c.Message, c.From.ID, "draft", "Этот отдел больше недоступен. Выберите направление.", a.draftDepartments(d))
	}
	_ = a.client.Answer(ctx, c.ID, "Отправляем заявку…")
	a.markCallbackAnswered()
	err = a.submitDraft(ctx, c, d)
	if err == nil {
		a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	} else {
		_ = a.say(ctx, c.Message, "Не удалось отправить заявку. Черновик сохранён. Попробуйте ещё раз или откройте его через /form.", nil)
	}
	return err
}

func (a *Adapter) callbackCancel(ctx context.Context, c *Callback, id int64, parts []string) error {
	if id != 0 {
		return a.beginCancel(ctx, c.From.ID, c.Message, id)
	}
	if _, valid, err := a.boundDraft(ctx, c, parts, ""); err != nil || !valid {
		return err
	}
	return a.cancelClientDraft(ctx, c.Message)
}

// Claim refreshes the engineer name from the Telegram profile before the service
// records it, so the client sees the current name instead of a stale one.
func (a *Adapter) callbackClaim(ctx context.Context, c *Callback, id int64) error {
	user, err := a.store.User(ctx, c.From.ID)
	if err != nil {
		return err
	}
	if err = a.updateName(ctx, user, c.From); err != nil {
		return err
	}
	return a.service.Claim(ctx, c.From.ID, id)
}

func (a *Adapter) callbackTickets(ctx context.Context, c *Callback, parts []string) error {
	if c.Message == nil || len(parts) != 3 {
		return domain.ErrInvalid
	}
	page, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil || page < 0 {
		return domain.ErrInvalid
	}
	anchor, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || anchor < 0 {
		return domain.ErrInvalid
	}
	user, err := a.store.User(ctx, c.From.ID)
	if err != nil {
		return err
	}
	return a.ticketListPage(ctx, c.Message, user, int(page), anchor)
}

func (a *Adapter) callbackHistoryFile(ctx context.Context, c *Callback, id int64, parts []string) error {
	if c.Message == nil || len(parts) != 3 {
		return domain.ErrInvalid
	}
	messageID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || messageID <= 0 {
		return domain.ErrInvalid
	}
	t, err := a.store.Ticket(ctx, id)
	if err != nil {
		return err
	}
	user, err := a.store.User(ctx, c.From.ID)
	if err != nil {
		return err
	}
	return a.historyFile(ctx, c.Message, user, t, messageID)
}

func (a *Adapter) callbackTicketOrHistory(ctx context.Context, c *Callback, action string, id int64, parts []string) error {
	if c.Message == nil {
		return domain.ErrInvalid
	}
	t, err := a.store.Ticket(ctx, id)
	if err != nil {
		return err
	}
	user, err := a.store.User(ctx, c.From.ID)
	if err != nil {
		return err
	}
	if action == "ticket" {
		return a.showTicket(ctx, c.Message, user, t)
	}
	page := int64(0)
	anchor := int64(0)
	if len(parts) == 4 {
		var e error
		page, e = strconv.ParseInt(parts[2], 10, 32)
		if e != nil {
			return domain.ErrInvalid
		}
		anchor, e = strconv.ParseInt(parts[3], 10, 64)
		if e != nil || anchor < 0 {
			return domain.ErrInvalid
		}
	} else if len(parts) != 2 {
		return domain.ErrInvalid
	}
	return a.historyPage(ctx, c.Message, user, t, int(page), anchor)
}
