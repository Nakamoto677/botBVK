package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/groupintake"
)

// groupMessage is the only entry point for a configured company group. It
// never sends a lifecycle notification: the trigger starts a ticket and the
// conversation stays in the chat. The sole bot replies are hints to an explicit
// @mention without a usable trigger.
func (a *Adapter) groupMessage(ctx context.Context, m *Message) error {
	group, ok := a.clientGroup(m.Chat.ID)
	if !ok {
		return nil
	}
	text := strings.TrimSpace(messageText(m))
	if strings.HasPrefix(text, "/") {
		// Commands belong to private chats and department groups. They must not
		// create tickets or invoke a menu in a client chat.
		return nil
	}
	active, err := a.store.ActiveTicket(ctx, m.Chat.ID)
	if err == nil {
		if active.SourceMessageID != 0 && active.SourceMessageID == m.ID {
			// A retried update after a committed ticket must not add the trigger
			// message a second time.
			return nil
		}
		return a.receiveGroup(ctx, m, active, group)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	trigger := groupintake.Parse(text, a.groupTags(), a.cfg.BotUsername)
	if trigger.Ambiguous {
		if trigger.Mentioned {
			return a.groupHint(ctx, m, a.groupAmbiguousHint())
		}
		return nil
	}
	if !trigger.HasDepartment() {
		if trigger.Mentioned {
			return a.groupHint(ctx, m, a.groupStartHint())
		}
		return nil
	}
	description := strings.TrimSpace(trigger.Text)
	attachment := groupAttachment(m)
	if attachment != nil {
		attachment.Text = description
	}
	if description == "" && attachment == nil {
		if trigger.Mentioned {
			return a.groupHint(ctx, m, groupEmptyHint)
		}
		return nil
	}
	service, ok := a.service.(groupService)
	if !ok {
		return domain.ErrInvalid
	}
	urgency := domain.Routine
	if trigger.Urgent {
		urgency = domain.Urgent
	}
	created, err := service.CreateFromGroup(ctx, domain.GroupTicketRequest{
		ChatID:            m.Chat.ID,
		ChatTitle:         m.Chat.Title,
		CompanyID:         group.CompanyID,
		RequesterID:       m.From.ID,
		RequesterName:     groupPersonName(m.From),
		RequesterUsername: m.From.Username,
		Department:        trigger.Department,
		Urgency:           urgency,
		Text:              description,
		SourceMessageID:   m.ID,
		Attachment:        attachment,
	})
	if errors.Is(err, domain.ErrForbidden) {
		// A support engineer wrote the tag. Staff work from the department queue;
		// their chat must not open a client ticket.
		return nil
	}
	if errors.Is(err, domain.ErrConflict) {
		active, readErr := a.store.ActiveTicket(ctx, m.Chat.ID)
		if readErr == nil {
			if active.SourceMessageID != 0 && active.SourceMessageID == m.ID {
				return nil
			}
			return a.receiveGroup(ctx, m, active, group)
		}
		return err
	}
	if err != nil {
		return err
	}
	if a.cfg.GroupReceipt {
		return a.groupHint(ctx, m, fmt.Sprintf("Заявка №%d создана для отдела %s. Ответим здесь.", created.ID, departmentName(created.Department)))
	}
	return nil
}

func (a *Adapter) receiveGroup(ctx context.Context, m *Message, ticket domain.Ticket, group ClientGroup) error {
	msg := normalise(m, false)
	if strings.TrimSpace(msg.Text) == "" && msg.FileID == "" {
		return nil
	}
	service, ok := a.service.(groupService)
	if !ok {
		return domain.ErrInvalid
	}
	err := service.AddGroupMessage(ctx, m.Chat.ID, group.CompanyID, ticket.ID, domain.GroupAuthor{
		ID:       m.From.ID,
		Name:     groupPersonName(m.From),
		Username: m.From.Username,
	}, msg)
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrConflict) && a.groupMessageStored(ctx, ticket.ID, m) {
		// The transaction may have committed before FinishUpdate failed. A
		// confirmed source is a receipt, not a new business conflict.
		return nil
	}
	if errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrInvalid) {
		// Unsupported media and unauthorized staff are ignored in a client chat.
		return nil
	}
	return err
}

func (a *Adapter) groupMessageStored(ctx context.Context, ticketID int64, m *Message) bool {
	stored, err := a.store.Messages(ctx, ticketID)
	if err != nil {
		return false
	}
	for _, saved := range stored {
		if saved.TelegramChatID == m.Chat.ID && saved.TelegramMessageID == m.ID && (saved.Direction == "inbound" || saved.Direction == "outbound") {
			return true
		}
	}
	return false
}

// groupEdit records a text edit or an edit revision without publishing anything
// into the company chat. Replacing an attachment is logged but the original file
// remains the archival copy, matching the private-chat rule.
func (a *Adapter) groupEdit(ctx context.Context, m *Message) error {
	if m.From.ID == 0 || m.SenderChat != nil {
		return nil
	}
	active, err := a.store.ActiveTicket(ctx, m.Chat.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !active.IsGroup() || active.ClientID != m.Chat.ID {
		return nil
	}
	lookup, ok := a.store.(interface {
		MessageTicket(context.Context, int64, int64) (domain.Ticket, error)
	})
	if !ok {
		return nil
	}
	owner, err := lookup.MessageTicket(ctx, m.Chat.ID, m.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner.ID != active.ID {
		return nil
	}
	messages, err := a.store.Messages(ctx, active.ID)
	if err != nil {
		return err
	}
	var original domain.Message
	found := false
	for _, saved := range messages {
		if saved.TelegramChatID == m.Chat.ID && saved.TelegramMessageID == m.ID {
			original = saved
			found = true
			break
		}
	}
	if !found || original.AuthorID != m.From.ID {
		return nil
	}
	normalized := normalise(m, false)
	if normalized.FileID != "" && normalized.FileID != original.FileID {
		data := map[string]any{
			"telegram_message_id": m.ID,
			"old_file_id":         original.FileID,
			"new_file_id":         normalized.FileID,
			"new_media_type":      normalized.MediaType,
			"new_file_size":       normalized.FileSize,
			"new_file_name":       normalized.FileName,
		}
		raw, _ := json.Marshal(data)
		if err = a.store.Transact(ctx, func(tx domain.Tx) error {
			return tx.AddEvent(ctx, domain.Event{TicketID: active.ID, ActorID: m.From.ID, Kind: "media_edit_unsupported", Data: raw, CreatedAt: time.Now().UTC()})
		}); err != nil {
			return err
		}
	}
	editor, ok := a.service.(interface {
		EditMessage(context.Context, int64, int64, domain.Message) error
	})
	if !ok {
		return nil
	}
	if err = editor.EditMessage(ctx, m.From.ID, active.ID, normalized); err != nil {
		if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func (a *Adapter) groupHint(ctx context.Context, m *Message, text string) error {
	return a.say(ctx, m, text, nil)
}

func groupAttachment(m *Message) *domain.Message {
	msg := normalise(m, false)
	if msg.FileID == "" {
		return nil
	}
	return &msg
}

func groupPersonName(p Person) string {
	name := strings.TrimSpace(strings.Join([]string{p.FirstName, p.LastName}, " "))
	if name == "" {
		name = strings.TrimSpace(p.Username)
	}
	if name == "" {
		name = "Сотрудник компании"
	}
	return name
}
