package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

type Service struct {
	store domain.Store
	now   func() time.Time
}

func urgencyText(u domain.Urgency) string {
	switch u {
	case domain.Urgent:
		return "срочно"
	case domain.Routine:
		return "в обычном порядке"
	default:
		return "не указана"
	}
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
	// A company group chat is the conversation surface itself. The bot does not
	// post lifecycle messages there: the client only tags to start a ticket and
	// engineers answer in the same chat.
	if t.IsGroup() {
		return nil
	}
	return s.enqueue(ctx, tx, t, "notify_client", map[string]string{"text": text})
}

func engineerName(u domain.User) string {
	if name := strings.Join(strings.Fields(u.Name), " "); name != "" {
		return name
	}
	if username := strings.TrimSpace(u.Username); username != "" {
		return "@" + strings.TrimPrefix(username, "@")
	}
	return "поддержки"
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
	return s.create(ctx, actorID, department, description, "")
}

func (s *Service) CreateDraft(ctx context.Context, actorID int64, generation string) (domain.Ticket, error) {
	return s.create(ctx, actorID, "", "", generation)
}
func (s *Service) create(ctx context.Context, actorID int64, department domain.Department, description, generation string) (domain.Ticket, error) {
	var t domain.Ticket
	if generation == "" && (!department.Valid() || strings.TrimSpace(description) == "") {
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
		var d domain.Draft
		var draftTx domain.DraftTx
		if generation != "" {
			var ok bool
			draftTx, ok = tx.(domain.DraftTx)
			if !ok {
				return domain.ErrInvalid
			}
			d, err = draftTx.LoadDraft(ctx, actorID)
			if err != nil {
				return err
			}
			if d.Generation != generation {
				return domain.ErrConflict
			}
			if d.Step == "submitted" && d.TicketID > 0 {
				t, err = tx.GetTicket(ctx, d.TicketID)
				return err
			}
			if d.Step != "confirm" || !d.Department.Valid() || !d.Urgency.Valid() || strings.TrimSpace(d.Text) == "" {
				return domain.ErrConflict
			}
			department, description = d.Department, d.Text
		}
		_, err = tx.ActiveTicket(ctx, actorID)
		if err == nil {
			return domain.ErrConflict
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		now := s.now()
		t = domain.Ticket{ClientID: u.ID, CompanyID: u.CompanyID, Department: department, Urgency: d.Urgency, Description: strings.TrimSpace(description), Status: domain.New, RoutingPending: false, CreatedAt: now, UpdatedAt: now}
		if err := tx.SaveTicket(ctx, &t); err != nil {
			return err
		}
		if err := s.event(ctx, tx, t, actorID, "created", nil); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, t, "refresh_card", nil); err != nil {
			return err
		}
		if generation != "" {
			for _, msg := range d.Attachments {
				msg.ID = 0
				msg.TicketID = t.ID
				msg.AuthorID = actorID
				msg.Direction = "inbound"
				msg.DeferredDelivery = true
				msg.RoutingDepartment = t.Department
				msg.CreatedAt = s.now()
				if err = tx.AddMessage(ctx, &msg); err != nil {
					return err
				}
				if err = s.event(ctx, tx, t, actorID, "message", map[string]any{"message_id": msg.ID, "direction": msg.Direction}); err != nil {
					return err
				}
				if err = s.enqueue(ctx, tx, t, "archive_message", msg); err != nil {
					return err
				}
			}
			if err = draftTx.CompleteDraft(ctx, actorID, generation, t.ID); err != nil {
				return err
			}
		}
		return s.notify(ctx, tx, t, "Заявка принята и поставлена в очередь.")
	})
	return t, err
}
func (s *Service) mutate(ctx context.Context, actorID, ticketID int64, fn func(domain.Tx, domain.User, *domain.Ticket) error, otherUsers ...int64) error {
	return s.store.Transact(ctx, func(tx domain.Tx) error {
		// Lock users in ID order before the ticket. A second user lock after
		// the ticket can create a cycle.
		ids := append([]int64{actorID}, otherUsers...)
		slices.Sort(ids)
		var u domain.User
		for _, id := range slices.Compact(ids) {
			user, err := tx.GetUser(ctx, id)
			if err != nil {
				return err
			}
			if id == actorID {
				u = user
			}
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
		if t.IsGroup() {
			// The company group is the conversation surface; there is no topic
			// to prepare and no client notification to send.
			t.RoutingPending = false
		} else {
			t.RoutingPending = true
			t.RoutingGeneration++
		}
		if err := s.save(ctx, tx, t, actorID, "claimed", nil); err != nil {
			return err
		}
		if t.IsGroup() {
			return s.enqueue(ctx, tx, *t, "refresh_card", nil)
		}
		if err := s.activateTopic(ctx, tx, *t); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, *t, "refresh_card", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, fmt.Sprintf("Инженер %s взял вашу заявку в работу\nВаша переписка продолжится прямо в этом чате:", engineerName(u)))
	})
}
func (s *Service) Transfer(ctx context.Context, actorID, ticketID int64, department domain.Department, reason string) error {
	return s.transfer(ctx, actorID, ticketID, department, reason, nil)
}

func (s *Service) TransferWithMessage(ctx context.Context, actorID, ticketID int64, department domain.Department, reason string, msg domain.Message) error {
	return s.transfer(ctx, actorID, ticketID, department, reason, &msg)
}

func (s *Service) transfer(ctx context.Context, actorID, ticketID int64, department domain.Department, reason string, msg *domain.Message) error {
	if !department.Valid() || strings.TrimSpace(reason) == "" {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !team(u, t.Department) {
			return domain.ErrForbidden
		}
		if !t.Status.Active() || t.Department == department || t.RoutingPending {
			return domain.ErrConflict
		}
		if err := s.actionAttachment(ctx, tx, *t, u, msg); err != nil {
			return err
		}
		oldDept := t.Department
		oldChat, oldTopic, oldQueue := t.TopicChatID, t.TopicID, t.QueueMessageID
		oldQueueMedia := t.QueueMediaType
		t.Department = department
		t.Status = domain.New
		t.AssigneeID = 0
		t.RoutingPending = false
		t.TopicChatID = 0
		t.TopicID = 0
		t.QueueMessageID = 0
		t.QueueMediaType = ""
		t.TopicMessageID = 0
		if err := s.save(ctx, tx, t, actorID, "transferred", map[string]any{"from": oldDept, "to": department, "reason": reason, "old_chat_id": oldChat, "old_topic_id": oldTopic, "message_id": attachmentID(msg)}); err != nil {
			return err
		}
		if oldChat != 0 && oldTopic != 0 && !t.IsGroup() {
			if err := s.enqueue(ctx, tx, *t, "close_topic", map[string]any{"chat_id": oldChat, "topic_id": oldTopic, "retire": true, "from_department": oldDept, "to_department": department}); err != nil {
				return err
			}
		}
		if oldQueue != 0 {
			if err := s.enqueue(ctx, tx, *t, "retire_queue_card", map[string]any{"chat_id": oldChat, "message_id": oldQueue, "queue_media_type": oldQueueMedia, "from_department": oldDept, "to_department": department}); err != nil {
				return err
			}
		}
		if err := s.enqueue(ctx, tx, *t, "refresh_card", nil); err != nil {
			return err
		}
		if !t.Urgency.Valid() {
			return s.notify(ctx, tx, *t, fmt.Sprintf("Ваше обращение передали в отдел %s\nИнженеры увидели вашу заявку, скоро возьмут в работу!", department.Name()))
		}
		return s.notify(ctx, tx, *t, fmt.Sprintf("Ваше обращение передали в отдел «%s».\nСрочность сохранена: %s.\nИнженеры увидели вашу заявку, скоро возьмут в работу!", department.Name(), urgencyText(t.Urgency)))
	})
}
func (s *Service) Close(ctx context.Context, actorID, ticketID int64, resolution string) error {
	return s.close(ctx, actorID, ticketID, resolution, nil)
}

func (s *Service) CloseWithMessage(ctx context.Context, actorID, ticketID int64, resolution string, msg domain.Message) error {
	return s.close(ctx, actorID, ticketID, resolution, &msg)
}

func (s *Service) close(ctx context.Context, actorID, ticketID int64, resolution string, msg *domain.Message) error {
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
		if err := s.actionAttachment(ctx, tx, *t, u, msg); err != nil {
			return err
		}
		now := s.now()
		t.Status = domain.Closed
		t.RoutingPending = false
		t.ClosedAt = &now
		t.ClosedBy = u.ID
		t.Resolution = strings.TrimSpace(resolution)
		if err := s.save(ctx, tx, t, actorID, "closed", map[string]string{"resolution": t.Resolution}); err != nil {
			return err
		}
		if t.TopicChatID != 0 && t.TopicID != 0 && !t.IsGroup() {
			if err := s.enqueue(ctx, tx, *t, "close_topic", map[string]any{"chat_id": t.TopicChatID, "topic_id": t.TopicID, "text": fmt.Sprintf("Заявка #%d закрыта. Итог: %s", t.ID, t.Resolution)}); err != nil {
				return err
			}
		}
		if err := s.enqueue(ctx, tx, *t, "refresh_card", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, "Заявка закрыта. Результат: "+t.Resolution)
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
func (s *Service) AddMessage(ctx context.Context, actorID, ticketID int64, msg domain.Message, public bool) error {
	if strings.TrimSpace(msg.Text) == "" && msg.FileID == "" {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if t.IsGroup() {
			// Group messages are recorded by AddGroupMessage; the private relay
			// path must never publish into a company chat.
			return domain.ErrInvalid
		}
		client := u.Role == "client" && u.Approved && u.ID == t.ClientID && u.CompanyID == t.CompanyID
		if !client && !team(u, t.Department) {
			return domain.ErrForbidden
		}
		if !t.Status.Active() {
			return domain.ErrConflict
		}
		if !client {
			if !public || t.Status == domain.New || t.TopicChatID == 0 || t.TopicID == 0 || msg.TelegramChatID != t.TopicChatID || msg.TelegramTopicID != t.TopicID {
				return domain.ErrForbidden
			}
		}
		msg.ID = 0
		msg.TicketID = t.ID
		msg.AuthorID = u.ID
		msg.CreatedAt = s.now()
		msg.Direction = ""
		msg.DeferredDelivery = false
		kind := ""
		resumed := false
		if client {
			msg.Direction = "inbound"
			msg.RoutingDepartment = t.Department
			kind = "team_message"
			msg.DeferredDelivery = t.Status == domain.New
			if msg.DeferredDelivery {
				kind = ""
			}
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
			if err := s.enqueue(ctx, tx, *t, "archive_message", msg); err != nil {
				return err
			}
		}
		if client && t.Status == domain.New {
			return s.enqueue(ctx, tx, *t, "refresh_card", nil)
		}
		return nil
	})
}

// EditMessage preserves revisions. Legacy internal and service messages remain
// archival; editing them does not send them to the client.
func (s *Service) EditMessage(ctx context.Context, actorID, ticketID int64, msg domain.Message) error {
	if msg.TelegramChatID == 0 || msg.TelegramMessageID == 0 || (strings.TrimSpace(msg.Text) == "" && msg.FileID == "") {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		client := u.Role == "client" && u.Approved && u.ID == t.ClientID && u.CompanyID == t.CompanyID
		groupParticipant := t.IsGroup() && msg.TelegramChatID == t.ClientID && (u.Role == "client" || team(u, t.Department))
		if !client && !groupParticipant && !team(u, t.Department) {
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
		if t.IsGroup() {
			// The edited message already exists in the company chat. There is
			// nobody to relay it to and no private menu to refresh.
			return nil
		}
		if client && t.Status == domain.New {
			return s.enqueue(ctx, tx, *t, "refresh_card", nil)
		}
		if client && msg.Direction == "inbound" && t.Status.Active() {
			msg.RoutingDepartment = t.Department
			msg.Text = fmt.Sprintf("Исправление сообщения #%d: %s", msg.TelegramMessageID, msg.Text)
			return s.enqueue(ctx, tx, *t, "team_message", msg)
		}
		if !client && msg.Direction == "outbound" {
			quote := ""
			if len(msg.Revisions) > 0 {
				quote = msg.Revisions[len(msg.Revisions)-1].Text
				v := []rune(quote)
				if len(v) > 200 {
					quote = string(v[:200]) + "…"
				}
			}
			msg.Text = fmt.Sprintf("Исправление ответа #%d\nИсходный ответ: %s\n\n%s", msg.TelegramMessageID, quote, msg.Text)
			return s.enqueue(ctx, tx, *t, "client_message", msg)
		}
		return nil
	})
}

// Cancel records a client withdrawal separately from an engineer completion.
// Clients are not asked for a reason.
func (s *Service) Cancel(ctx context.Context, actorID, ticketID int64) error {
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		owner := u.Approved && u.Role == "client" && u.ID == t.ClientID && u.CompanyID == t.CompanyID
		if !owner && !privileged(u) {
			return domain.ErrForbidden
		}
		if t.Status == domain.Cancelled {
			return nil
		}
		if !t.Status.Active() {
			return domain.ErrConflict
		}
		now := s.now()
		t.Status = domain.Cancelled
		t.ClosedAt = &now
		t.ClosedBy = u.ID
		t.Resolution = ""
		t.RoutingPending = false
		if err := s.save(ctx, tx, t, actorID, "cancelled", nil); err != nil {
			return err
		}
		if t.TopicChatID != 0 && t.TopicID != 0 && !t.IsGroup() {
			if err := s.enqueue(ctx, tx, *t, "close_topic", map[string]any{"chat_id": t.TopicChatID, "topic_id": t.TopicID, "text": fmt.Sprintf("Заявка #%d отменена.", t.ID)}); err != nil {
				return err
			}
		}
		if err := s.enqueue(ctx, tx, *t, "refresh_card", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, "Заявка завершена по вашему запросу.")
	})
}

// Assign changes the primary engineer with the current owner or dispatcher authorization.
func (s *Service) Assign(ctx context.Context, actorID, ticketID, engineerID int64) error {
	if engineerID <= 0 {
		return domain.ErrInvalid
	}
	return s.mutate(ctx, actorID, ticketID, func(tx domain.Tx, u domain.User, t *domain.Ticket) error {
		if !canAct(u, *t) {
			return domain.ErrForbidden
		}
		if t.Status != domain.InProgress && t.Status != domain.Waiting {
			return domain.ErrConflict
		}
		target, err := tx.GetUser(ctx, engineerID)
		if err != nil {
			return err
		}
		if target.Role != "engineer" || !team(target, t.Department) {
			return domain.ErrForbidden
		}
		if t.AssigneeID == engineerID {
			return domain.ErrConflict
		}
		old := t.AssigneeID
		t.AssigneeID = engineerID
		if err := s.save(ctx, tx, t, actorID, "reassigned", map[string]int64{"from": old, "to": engineerID}); err != nil {
			return err
		}
		if err := s.enqueue(ctx, tx, *t, "refresh_card", nil); err != nil {
			return err
		}
		return s.notify(ctx, tx, *t, "По заявке назначен другой инженер.")
	}, engineerID)
}

// activateTopic is called only when an engineer explicitly takes a queued request.
func (s *Service) activateTopic(ctx context.Context, tx domain.Tx, t domain.Ticket) error {
	if t.IsGroup() {
		return nil
	}
	if t.TopicChatID != 0 && t.TopicID != 0 {
		return s.enqueue(ctx, tx, t, "reopen_topic", map[string]any{"routing_generation": t.RoutingGeneration})
	}
	return s.enqueue(ctx, tx, t, "create_topic", map[string]any{"old_chat_id": 0, "old_topic_id": 0, "reason": "", "routing_generation": t.RoutingGeneration})
}

// actionAttachment retains a prompt attachment as service data in the same
// transaction as its state change. Text-only reasons are already captured in events.
func (s *Service) actionAttachment(ctx context.Context, tx domain.Tx, t domain.Ticket, u domain.User, input *domain.Message) error {
	if input == nil || input.FileID == "" {
		return nil
	}
	msg := *input
	msg.ID = 0
	msg.TicketID = t.ID
	msg.AuthorID = u.ID
	msg.Direction = "service"
	msg.CreatedAt = s.now()
	msg.EditedAt = nil
	msg.Revisions = nil
	msg.ArchivePath = ""
	msg.SHA256 = ""
	msg.DeferredDelivery = false
	msg.RoutingDepartment = t.Department
	if err := tx.AddMessage(ctx, &msg); err != nil {
		return err
	}
	if err := s.event(ctx, tx, t, u.ID, "message", map[string]any{"message_id": msg.ID, "direction": msg.Direction, "action_attachment": true}); err != nil {
		return err
	}
	*input = msg
	return s.enqueue(ctx, tx, t, "archive_message", msg)
}

func attachmentID(msg *domain.Message) int64 {
	if msg == nil {
		return 0
	}
	return msg.ID
}
