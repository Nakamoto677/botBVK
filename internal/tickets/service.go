package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

type Service struct {
	store domain.Store
	now   func() time.Time
}

func New(store domain.Store) *Service {
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }}
}
func staff(u domain.User) bool {
	return u.Approved && (u.Role == "engineer" || u.Role == "dispatcher" || u.Role == "admin")
}
func privileged(u domain.User) bool {
	return u.Approved && (u.Role == "dispatcher" || u.Role == "admin")
}
func team(u domain.User, d domain.Department) bool {
	if privileged(u) {
		return true
	}
	if !staff(u) {
		return false
	}
	for _, v := range u.Departments {
		if v == d {
			return true
		}
	}
	return false
}
func canAct(u domain.User, t domain.Ticket) bool {
	return privileged(u) || (team(u, t.Department) && t.AssigneeID == u.ID)
}
func encode(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func (s *Service) enqueue(ctx context.Context, tx domain.Tx, t domain.Ticket, kind string, payload any) error {
	return tx.Enqueue(ctx, domain.Operation{Kind: kind, TicketID: t.ID, Payload: encode(payload), AvailableAt: s.now(), State: "pending"})
}
func (s *Service) event(ctx context.Context, tx domain.Tx, t domain.Ticket, actor int64, kind string, data any) error {
	return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: actor, Kind: kind, Data: encode(data), CreatedAt: s.now()})
}
func (s *Service) notify(ctx context.Context, tx domain.Tx, t domain.Ticket, text string) error {
	return s.enqueue(ctx, tx, t, "notify_client", map[string]string{"text": text})
}
func (s *Service) Register(ctx context.Context, id int64, name string, companyID int64) error {
	if id <= 0 || companyID <= 0 || strings.TrimSpace(name) == "" {
		return domain.ErrInvalid
	}
	companies, err := s.store.Companies(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, c := range companies {
		if c.ID == companyID {
			found = true
		}
	}
	if !found {
		return domain.ErrInvalid
	}
	return s.store.Transact(ctx, func(tx domain.Tx) error {
		u, err := tx.GetUser(ctx, id)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if err == nil && u.Approved {
			return domain.ErrConflict
		}
		if err == nil && u.Role != "client" {
			return domain.ErrForbidden
		}
		if err := tx.SaveUser(ctx, domain.User{ID: id, Name: strings.TrimSpace(name), CompanyID: companyID, Role: "client"}); err != nil {
			return err
		}
		return tx.RequestAccess(ctx, id, companyID)
	})
}
func (s *Service) ApproveAccess(ctx context.Context, actorID, requestID int64, approve bool) error {
	return s.store.Transact(ctx, func(tx domain.Tx) error {
		u, err := tx.GetUser(ctx, actorID)
		if err != nil {
			return err
		}
		if !u.Approved || u.Role != "admin" {
			return domain.ErrForbidden
		}
		r, err := tx.ResolveAccess(ctx, requestID, approve)
		if err != nil {
			return err
		}
		return tx.Enqueue(ctx, domain.Operation{Kind: "access_notification", Payload: encode(map[string]any{"user_id": r.UserID, "text": map[bool]string{true: "Доступ одобрен. Вы можете создать заявку.", false: "Заявка на доступ отклонена."}[approve]}), AvailableAt: s.now(), State: "pending"})
	})
}
func (s *Service) Create(ctx context.Context, actorID int64, department domain.Department, description string) (domain.Ticket, error) {
	var t domain.Ticket
	if !department.Valid() || strings.TrimSpace(description) == "" {
		return t, domain.ErrInvalid
	}
	err := s.store.Transact(ctx, func(tx domain.Tx) error {
		u, err := tx.GetUser(ctx, actorID)
		if err != nil {
			return err
		}
		if !u.Approved || u.Role != "client" {
			return domain.ErrForbidden
		}
		_, err = tx.ActiveTicket(ctx, actorID)
		if err == nil {
			return domain.ErrConflict
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		now := s.now()
		t = domain.Ticket{ClientID: u.ID, CompanyID: u.CompanyID, Department: department, Description: strings.TrimSpace(description), Status: domain.New, CreatedAt: now, UpdatedAt: now}
		if err := tx.SaveTicket(ctx, &t); err != nil {
			return err
		}
		if err := s.event(ctx, tx, t, actorID, "created", nil); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, t, "create_topic", map[string]any{"old_chat_id": 0, "old_topic_id": 0, "reason": ""}); err != nil {
			return err
		}
		return s.notify(ctx, tx, t, "Заявка принята и поставлена в очередь.")
	})
	return t, err
}
func (s *Service) mutate(ctx context.Context, actorID, ticketID int64, fn func(domain.Tx, domain.User, *domain.Ticket) error) error {
	return s.store.Transact(ctx, func(tx domain.Tx) error {
		u, err := tx.GetUser(ctx, actorID)
		if err != nil {
			return err
		}
		t, err := tx.GetTicket(ctx, ticketID)
		if err != nil {
			return err
		}
		return fn(tx, u, &t)
	})
}
func (s *Service) save(ctx context.Context, tx domain.Tx, t *domain.Ticket, actor int64, kind string, data any) error {
	t.UpdatedAt = s.now()
	if err := tx.SaveTicket(ctx, t); err != nil {
		return err
	}
	return s.event(ctx, tx, *t, actor, kind, data)
}
func (s *Service) Claim(ctx context.Context, actorID, ticketID int64) error {
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !team(u, t.Department) {
			return domain.ErrForbidden
		}
		if t.Status != domain.New || t.AssigneeID != 0 {
			return domain.ErrConflict
		}
		t.AssigneeID = u.ID
		t.Status = domain.InProgress
		if err := s.save(ctx, tx, t, actorID, "claimed", nil); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, *t, "refresh_card", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, "Инженер взял заявку в работу.")
	})
}
func (s *Service) Transfer(ctx context.Context, actorID, ticketID int64, department domain.Department, reason string) error {
	if !department.Valid() || strings.TrimSpace(reason) == "" {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !team(u, t.Department) {
			return domain.ErrForbidden
		}
		if !t.Status.Active() || t.Department == department {
			return domain.ErrConflict
		}
		oldDept := t.Department
		oldChat, oldTopic := t.TopicChatID, t.TopicID
		t.Department = department
		t.Status = domain.New
		t.AssigneeID = 0
		t.TopicChatID = 0
		t.TopicID = 0
		t.QueueMessageID = 0
		if err := s.save(ctx, tx, t, actorID, "transferred", map[string]any{"from": oldDept, "to": department, "reason": reason}); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, *t, "create_topic", map[string]any{"old_chat_id": oldChat, "old_topic_id": oldTopic, "reason": reason}); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, "Заявка передана в другой отдел.")
	})
}
func (s *Service) Close(ctx context.Context, actorID, ticketID int64, resolution string) error {
	if strings.TrimSpace(resolution) == "" {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !canAct(u, *t) {
			return domain.ErrForbidden
		}
		if !t.Status.Active() {
			return domain.ErrConflict
		}
		now := s.now()
		t.Status = domain.Closed
		t.ClosedAt = &now
		t.ClosedBy = u.ID
		t.Resolution = strings.TrimSpace(resolution)
		if err := s.save(ctx, tx, t, actorID, "closed", map[string]string{"resolution": t.Resolution}); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, *t, "close_topic", map[string]int64{"chat_id": t.TopicChatID, "topic_id": t.TopicID}); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, "Заявка закрыта. Результат: "+t.Resolution)
	})
}
func (s *Service) Reopen(ctx context.Context, actorID, ticketID int64) error {
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !u.Approved || (!privileged(u) && (u.Role != "client" || u.ID != t.ClientID)) {
			return domain.ErrForbidden
		}
		if t.Status != domain.Closed || t.ClosedAt == nil {
			return domain.ErrConflict
		}
		if !privileged(u) && s.now().Sub(*t.ClosedAt) > 7*24*time.Hour {
			return domain.ErrConflict
		}
		if u.ID != t.ClientID {
			if _, err := tx.GetUser(ctx, t.ClientID); err != nil {
				return err
			}
		}
		_, err := tx.ActiveTicket(ctx, t.ClientID)
		if err == nil {
			return domain.ErrConflict
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		t.Status = domain.New
		t.AssigneeID = 0
		t.ClosedAt = nil
		t.ClosedBy = 0
		t.Resolution = ""
		if err := s.save(ctx, tx, t, actorID, "reopened", nil); err != nil {
			return err
		}
		kind := "reopen_topic"
		if t.TopicID == 0 || t.TopicChatID == 0 {
			kind = "create_topic"
		}
		if err := s.enqueue(ctx, tx, *t, kind, map[string]any{"old_chat_id": 0, "old_topic_id": 0, "reason": "Повторное открытие"}); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, "Заявка повторно открыта.")
	})
}
func (s *Service) SetWaiting(ctx context.Context, actorID, ticketID int64, waiting bool) error {
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !canAct(u, *t) {
			return domain.ErrForbidden
		}
		if (waiting && t.Status != domain.InProgress) || (!waiting && t.Status != domain.Waiting) {
			return domain.ErrConflict
		}
		if waiting {
			t.Status = domain.Waiting
		} else {
			t.Status = domain.InProgress
		}
		if err := s.save(ctx, tx, t, actorID, "status_changed", map[string]domain.Status{"status": t.Status}); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, *t, "refresh_card", nil)
	})
}
func (s *Service) SetEmergency(ctx context.Context, actorID, ticketID int64) error {
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !privileged(u) {
			return domain.ErrForbidden
		}
		if !t.Status.Active() {
			return domain.ErrConflict
		}
		t.Emergency = true
		if err := s.save(ctx, tx, t, actorID, "emergency", nil); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, *t, "refresh_card", nil)
	})
}
func (s *Service) AddMessage(ctx context.Context, actorID, ticketID int64, msg domain.Message, public bool) error {
	if strings.TrimSpace(msg.Text) == "" && msg.FileID == "" {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		client := u.Role == "client" && u.Approved && u.ID == t.ClientID && u.CompanyID == t.CompanyID
		if !client && !team(u, t.Department) {
			return domain.ErrForbidden
		}
		if !t.Status.Active() {
			return domain.ErrConflict
		}
		if !client && public && !canAct(u, *t) {
			return domain.ErrForbidden
		}
		msg.ID = 0
		msg.TicketID = t.ID
		msg.AuthorID = u.ID
		msg.CreatedAt = s.now()
		msg.Direction = "internal"
		kind := ""
		resumed := false
		if client {
			msg.Direction = "inbound"
			kind = "team_message"
			if t.Status == domain.Waiting {
				t.Status = domain.InProgress
				resumed = true
			}
		} else if public {
			msg.Direction = "outbound"
			kind = "client_message"
		}
		if err := tx.AddMessage(ctx, &msg); err != nil {
			return err
		}
		if err := s.save(ctx, tx, t, actorID, "message", map[string]any{"message_id": msg.ID, "direction": msg.Direction}); err != nil {
			return err
		}
		if resumed {
			if err := s.event(ctx, tx, *t, actorID, "status_changed", map[string]domain.Status{"status": t.Status}); err != nil {
				return err
			}
			if err := s.enqueue(ctx, tx, *t, "refresh_card", nil); err != nil {
				return err
			}
		}
		if kind != "" {
			return s.enqueue(ctx, tx, *t, kind, msg)
		}
		if msg.FileID != "" {
			return s.enqueue(ctx, tx, *t, "archive_message", msg)
		}
		return nil
	})
}

// EditMessage archives a revision of an existing message. Engineer edits never
// resend a public response; a deliberate new /reply is required for delivery.
func (s *Service) EditMessage(ctx context.Context, actorID, ticketID int64, msg domain.Message) error {
	if msg.TelegramChatID == 0 || msg.TelegramMessageID == 0 || (strings.TrimSpace(msg.Text) == "" && msg.FileID == "") {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		client := u.Role == "client" && u.Approved && u.ID == t.ClientID && u.CompanyID == t.CompanyID
		if !client && !team(u, t.Department) {
			return domain.ErrForbidden
		}
		msg.TicketID = t.ID
		msg.AuthorID = u.ID
		msg.Direction = ""
		if msg.EditedAt == nil {
			now := s.now()
			msg.EditedAt = &now
		}
		if err := tx.EditMessage(ctx, &msg); err != nil {
			return err
		}
		if err := s.event(ctx, tx, *t, actorID, "message_edited", map[string]any{"message_id": msg.ID, "direction": msg.Direction}); err != nil {
			return err
		}
		if msg.FileID != "" {
			if err := s.enqueue(ctx, tx, *t, "archive_message", msg); err != nil {
				return err
			}
		}
		if client && t.Status.Active() {
			msg.Text = fmt.Sprintf("Исправление сообщения #%d: %s", msg.TelegramMessageID, msg.Text)
			return s.enqueue(ctx, tx, *t, "team_message", msg)
		}
		return nil
	})
}
