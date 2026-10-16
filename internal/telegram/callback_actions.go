package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

// Registered-callback actions live here so that callback stays a dispatcher.
// Every method receives the parsed callback and the raw token parts, returns the
// business error, and leaves acknowledgement to the caller.

func (a *Adapter) callbackCompany(ctx context.Context, c *Callback, id int64) error {
	if c.Message == nil || c.Message.Chat.Type != "private" {
		return domain.ErrForbidden
	}
	_ = a.client.Answer(ctx, c.ID, "")
	a.markCallbackAnswered()
	companies, err := a.store.Companies(ctx)
	if err != nil {
		return err
	}
	valid := false
	for _, co := range companies {
		valid = valid || co.ID == id
	}
	if !valid {
		return a.registrationCompanies(ctx, c.Message, c.From.ID, true)
	}
	err = a.service.Register(ctx, c.From.ID, strings.TrimSpace(c.From.FirstName+" "+c.From.LastName), id)
	if errors.Is(err, domain.ErrInvalid) {
		return a.registrationCompanies(ctx, c.Message, c.From.ID, true)
	}
	if err == nil {
		registered, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		err = a.updateName(ctx, registered, c.From)
	}
	if err == nil {
		requests, e := a.store.AccessRequests(ctx)
		if e != nil {
			return e
		}
		for _, request := range requests {
			if request.UserID == c.From.ID && request.Status == "pending" {
				data, _ := json.Marshal(map[string]any{"request_id": request.ID, "user_id": request.UserID, "company_id": request.CompanyID})
				err = a.store.Transact(ctx, func(tx domain.Tx) error {
					return tx.Enqueue(ctx, domain.Operation{Kind: "access_request_notification", Payload: data, AvailableAt: time.Now().UTC(), State: "pending"})
				})
				break
			}
		}
	}
	if err == nil {
		a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "registration")
	}
	return err
}

func (a *Adapter) callbackNewDraft(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "department")
	if err != nil || !valid {
		return err
	}
	if len(parts) != 3 || !domain.Department(parts[1]).Valid() {
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
	d.Step = "description"
	if err = a.saveDraft(ctx, c.From.ID, d); err != nil {
		return err
	}
	a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
	return a.clientScreen(ctx, c.Message, "draft", draftDescriptionText, clientDraftKeyboard)
}

func (a *Adapter) callbackSubmitDraft(ctx context.Context, c *Callback, parts []string) error {
	d, valid, err := a.boundDraft(ctx, c, parts, "confirm")
	if err != nil || !valid {
		return err
	}
	err = a.submitDraft(ctx, c, d)
	if err == nil {
		a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
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
