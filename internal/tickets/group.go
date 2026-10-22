package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"bot-bvk/internal/domain"
)

// CreateFromGroup opens the one active ticket of a company group chat. The
// group itself is the client: its negative Telegram chat ID is stored as
// ClientID, so the existing active-ticket index keeps parallel group tickets
// impossible without any schema change.
func (s *Service) CreateFromGroup(ctx context.Context, req domain.GroupTicketRequest) (domain.Ticket, error) {
	var ticket domain.Ticket
	if req.ChatID >= 0 || req.CompanyID <= 0 || req.RequesterID <= 0 || !req.Department.Valid() || !req.Urgency.Valid() {
		return ticket, domain.ErrInvalid
	}
	text := strings.TrimSpace(req.Text)
	hasAttachment := req.Attachment != nil && req.Attachment.FileID != ""
	if text == "" && !hasAttachment {
		return ticket, domain.ErrInvalid
	}
	if text == "" {
		text = "Обращение с вложением без описания."
	}
	title := strings.TrimSpace(req.ChatTitle)
	if title == "" {
		title = "Чат компании"
	}
	requesterName := strings.TrimSpace(req.RequesterName)
	if requesterName == "" {
		requesterName = "Сотрудник компании"
	}

	err := s.store.Transact(ctx, func(tx domain.Tx) error {
		requester, err := ensureGroupRequester(ctx, tx, req.RequesterID, requesterName, req.RequesterUsername, req.CompanyID)
		if err != nil {
			return err
		}
		if requester.Role != "client" {
			// Support staff work from the department queue, not as group clients.
			return domain.ErrForbidden
		}
		if err = ensureGroupClient(ctx, tx, req.ChatID, title, req.CompanyID); err != nil {
			return err
		}
		if _, err = tx.ActiveTicket(ctx, req.ChatID); err == nil {
			return domain.ErrConflict
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		now := s.now()
		ticket = domain.Ticket{
			ClientID:          req.ChatID,
			Channel:           domain.ChannelGroup,
			CompanyID:         req.CompanyID,
			Department:        req.Department,
			Urgency:           req.Urgency,
			Description:       text,
			Status:            domain.New,
			RequesterID:       req.RequesterID,
			RequesterName:     requesterName,
			RequesterUsername: strings.TrimSpace(req.RequesterUsername),
			SourceMessageID:   req.SourceMessageID,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err = tx.SaveTicket(ctx, &ticket); err != nil {
			return err
		}
		if err = s.event(ctx, tx, ticket, req.RequesterID, "created", map[string]any{
			"channel":           string(domain.ChannelGroup),
			"chat_id":           req.ChatID,
			"requester_id":      req.RequesterID,
			"source_message_id": req.SourceMessageID,
		}); err != nil {
			return err
		}
		var attachment domain.Message
		if hasAttachment {
			attachment = *req.Attachment
			attachment.ID = 0
			attachment.TicketID = ticket.ID
			attachment.AuthorID = req.RequesterID
			attachment.Direction = "inbound"
			attachment.RoutingDepartment = ticket.Department
			attachment.DeferredDelivery = false
			attachment.CreatedAt = now
			attachment.EditedAt = nil
			attachment.Revisions = nil
			attachment.ArchivePath = ""
			attachment.SHA256 = ""
			if err = tx.AddMessage(ctx, &attachment); err != nil {
				return err
			}
			if err = s.event(ctx, tx, ticket, req.RequesterID, "message", map[string]any{"message_id": attachment.ID, "direction": attachment.Direction}); err != nil {
				return err
			}
		}
		if err = s.enqueue(ctx, tx, ticket, "refresh_card", nil); err != nil {
			return err
		}
		if hasAttachment {
			if err = s.enqueue(ctx, tx, ticket, "archive_message", attachment); err != nil {
				return err
			}
		}
		return nil
	})
	return ticket, err
}

// AddGroupMessage records a message from an already active company chat. It
// never publishes anything: the message is already visible to both sides.
func (s *Service) AddGroupMessage(ctx context.Context, chatID, companyID, ticketID int64, author domain.GroupAuthor, msg domain.Message) error {
	if chatID >= 0 || companyID <= 0 || ticketID <= 0 || author.ID <= 0 {
		return domain.ErrInvalid
	}
	if strings.TrimSpace(msg.Text) == "" && msg.FileID == "" {
		return domain.ErrInvalid
	}
	return s.store.Transact(ctx, func(tx domain.Tx) error {
		user, err := ensureGroupAuthor(ctx, tx, author, companyID)
		if err != nil {
			return err
		}
		t, err := tx.GetTicket(ctx, ticketID)
		if err != nil {
			return err
		}
		if !t.IsGroup() || t.ClientID != chatID {
			return domain.ErrInvalid
		}
		if !t.Status.Active() {
			return domain.ErrConflict
		}
		isStaff := user.Role == "engineer" || user.Role == "dispatcher" || user.Role == "admin"
		if isStaff && !team(user, t.Department) {
			// Engineers from another department are not participants of this
			// ticket. Do not record their chat as client correspondence.
			return nil
		}
		claimed := false
		if isStaff && t.Status == domain.New {
			t.AssigneeID = user.ID
			t.Status = domain.InProgress
			t.RoutingPending = false
			claimed = true
		}
		resumed := false
		if !isStaff && t.Status == domain.Waiting {
			t.Status = domain.InProgress
			resumed = true
		}
		now := s.now()
		msg.ID = 0
		msg.TicketID = t.ID
		msg.AuthorID = user.ID
		msg.Direction = "inbound"
		if isStaff {
			msg.Direction = "outbound"
		}
		msg.RoutingDepartment = t.Department
		msg.DeferredDelivery = false
		msg.CreatedAt = now
		msg.EditedAt = nil
		msg.Revisions = nil
		msg.ArchivePath = ""
		msg.SHA256 = ""
		if err = tx.AddMessage(ctx, &msg); err != nil {
			return err
		}
		if err = s.event(ctx, tx, t, user.ID, "message", map[string]any{"message_id": msg.ID, "direction": msg.Direction}); err != nil {
			return err
		}
		firstResponse := false
		if isStaff && t.FirstResponseAt == nil {
			firstResponse = true
			t.FirstResponseAt = &now
		}
		t.UpdatedAt = now
		if err = tx.SaveTicket(ctx, &t); err != nil {
			return err
		}
		if claimed {
			// Auto-claim is recorded only when this message actually took the
			// ticket. The claim event stays compatible with analytics.
			if err = s.event(ctx, tx, t, user.ID, "claimed", map[string]any{"channel": string(domain.ChannelGroup)}); err != nil {
				return err
			}
		}
		if firstResponse {
			if err = s.event(ctx, tx, t, user.ID, "first_response", nil); err != nil {
				return err
			}
			data, _ := json.Marshal(map[string]any{
				"source_message_id":   msg.ID,
				"telegram_chat_id":    msg.TelegramChatID,
				"telegram_message_id": msg.TelegramMessageID,
				"telegram_topic_id":   msg.TelegramTopicID,
				"direction":           "outbound",
			})
			if err = s.event(ctx, tx, t, user.ID, "message_delivered", data); err != nil {
				return err
			}
		}
		if resumed {
			if err = s.event(ctx, tx, t, user.ID, "status_changed", map[string]domain.Status{"status": t.Status}); err != nil {
				return err
			}
		}
		if firstResponse || resumed || (!isStaff && t.Status == domain.New) {
			if err = s.enqueue(ctx, tx, t, "refresh_card", nil); err != nil {
				return err
			}
		}
		if msg.FileID != "" {
			if err = s.enqueue(ctx, tx, t, "archive_message", msg); err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureGroupClient creates the durable client identity of a company chat. It
// is not a Telegram user and never receives private notifications.
func ensureGroupClient(ctx context.Context, tx domain.Tx, chatID int64, title string, companyID int64) error {
	u, err := tx.GetUser(ctx, chatID)
	if errors.Is(err, domain.ErrNotFound) {
		return tx.SaveUser(ctx, domain.User{ID: chatID, Name: title, CompanyID: companyID, Role: "client", Approved: true})
	}
	if err != nil {
		return err
	}
	changed := false
	if strings.TrimSpace(u.Name) != strings.TrimSpace(title) {
		u.Name = title
		changed = true
	}
	if u.CompanyID != companyID || u.Role != "client" || !u.Approved {
		u.CompanyID = companyID
		u.Role = "client"
		u.Approved = true
		changed = true
	}
	if changed {
		return tx.SaveUser(ctx, u)
	}
	return nil
}

// ensureGroupRequester records the person who wrote the trigger. A new record
// deliberately stays unapproved: group membership authorizes group tickets, not
// the private bot.
func ensureGroupRequester(ctx context.Context, tx domain.Tx, id int64, name, username string, companyID int64) (domain.User, error) {
	u, err := tx.GetUser(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		u = domain.User{ID: id, Name: name, Username: username, CompanyID: companyID, Role: "client", Approved: false}
		return u, tx.SaveUser(ctx, u)
	}
	if err != nil {
		return u, err
	}
	changed := false
	if strings.TrimSpace(name) != "" && u.Name != name {
		u.Name = name
		changed = true
	}
	if strings.TrimSpace(username) != "" && u.Username != username {
		u.Username = username
		changed = true
	}
	if changed {
		err = tx.SaveUser(ctx, u)
	}
	return u, err
}

// ensureGroupAuthor records any participant who writes during an active ticket.
// Unknown participants become unapproved clients; approved staff keep their
// role and company.
func ensureGroupAuthor(ctx context.Context, tx domain.Tx, author domain.GroupAuthor, companyID int64) (domain.User, error) {
	u, err := tx.GetUser(ctx, author.ID)
	if errors.Is(err, domain.ErrNotFound) {
		u = domain.User{ID: author.ID, Name: strings.TrimSpace(author.Name), Username: strings.TrimSpace(author.Username), CompanyID: companyID, Role: "client", Approved: false}
		return u, tx.SaveUser(ctx, u)
	}
	if err != nil {
		return u, err
	}
	changed := false
	if name := strings.TrimSpace(author.Name); name != "" && u.Name != name {
		u.Name = name
		changed = true
	}
	if username := strings.TrimSpace(author.Username); username != "" && u.Username != username {
		u.Username = username
		changed = true
	}
	if !changed {
		return u, nil
	}
	return u, tx.SaveUser(ctx, u)
}
