package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"bot-bvk/internal/domain"
)

func (a *Adapter) admin(ctx context.Context, m *Message, u domain.User) error {
	if !u.Approved || u.Role != "admin" || m.Chat.Type != "private" {
		return domain.ErrForbidden
	}
	fields := strings.Fields(m.Text)
	if len(fields) == 0 {
		return domain.ErrInvalid
	}
	switch fields[0] {
	case "/requests":
		requests, err := a.store.AccessRequests(ctx)
		if err != nil {
			return err
		}
		for _, r := range requests {
			if r.Status != "pending" {
				continue
			}
			if err = a.say(ctx, m, fmt.Sprintf("Запрос #%d · аккаунт %d · компания %d", r.ID, r.UserID, r.CompanyID), [][]Button{{{Text: "Одобрить", Data: fmt.Sprintf("approve:%d", r.ID)}, {Text: "Отклонить", Data: fmt.Sprintf("reject:%d", r.ID)}}}); err != nil {
				return err
			}
		}
		return nil
	case "/retry":
		if len(fields) != 2 {
			return domain.ErrInvalid
		}
		id, e := strconv.ParseInt(fields[1], 10, 64)
		if e != nil || id <= 0 {
			return domain.ErrInvalid
		}
		if lookup, ok := a.store.(interface {
			Operation(context.Context, int64) (domain.Operation, error)
		}); ok {
			operation, e := lookup.Operation(ctx, id)
			if e != nil {
				return e
			}
			if operation.Kind == "create_topic" && (strings.Contains(operation.LastError, "outcome unknown") || strings.Contains(operation.LastError, "telegram 0:")) {
				ticket, e := a.store.Ticket(ctx, operation.TicketID)
				if e != nil {
					return e
				}
				if ticket.RoutingPending || ticket.TopicID == 0 {
					return a.say(ctx, m, fmt.Sprintf("Результат создания топика неизвестен. Проверьте группу и привяжите существующий топик: /bind %d TOPIC_ID. Затем повторите /retry %d. Автоматический повтор может создать дубль.", ticket.ID, id), nil)
				}
			}
		}
		if recovery, ok := a.store.(interface {
			RetryFailedOperation(context.Context, int64) error
		}); ok {
			if e = recovery.RetryFailedOperation(ctx, id); e != nil {
				return e
			}
			return a.say(ctx, m, "Операция возвращена в очередь. При неизвестном результате создания топика проверьте существующие топики перед повтором, чтобы избежать дублей.", nil)
		}
		return domain.ErrInvalid
	case "/quarantine":
		// Accepted updates that can never complete on their own stay unfinished
		// until an operator reviews and releases them.
		quarantine, ok := a.store.(domain.QuarantineStore)
		if !ok {
			return domain.ErrInvalid
		}
		if len(fields) >= 3 && fields[1] == "clear" {
			id, e := strconv.ParseInt(fields[2], 10, 64)
			if e != nil || id <= 0 {
				return domain.ErrInvalid
			}
			if e = quarantine.ReleaseQuarantine(ctx, id); e != nil {
				if errors.Is(e, domain.ErrNotFound) {
					return a.say(ctx, m, fmt.Sprintf("Обновление %d не находится в карантине.", id), nil)
				}
				return e
			}
			return a.say(ctx, m, fmt.Sprintf("Обновление %d возвращено в обработку. При неустранимой причине оно снова уйдёт в карантин и не заблокирует другие действия.", id), nil)
		}
		if len(fields) != 1 {
			return domain.ErrInvalid
		}
		rows, e := quarantine.QuarantinedUpdates(ctx)
		if e != nil {
			return e
		}
		if len(rows) == 0 {
			return a.say(ctx, m, "Карантин пуст: все принятые обновления обработаны.", nil)
		}
		for _, row := range rows {
			cause := row.LastError
			if runes := []rune(cause); len(runes) > 160 {
				cause = string(runes[:160]) + "…"
			}
			if e = a.say(ctx, m, fmt.Sprintf("Обновление %d · аккаунт %d · чат %d · попыток %d\n%s\nСнять: /quarantine clear %d", row.ID, row.ActorID, row.ChatID, row.Attempts, cause, row.ID), nil); e != nil {
				return e
			}
		}
		return nil
	case "/bind":
		if len(fields) != 3 {
			return domain.ErrInvalid
		}
		id, e := strconv.ParseInt(fields[1], 10, 64)
		if e != nil || id <= 0 {
			return domain.ErrInvalid
		}
		topic, e := strconv.ParseInt(fields[2], 10, 64)
		if e != nil || topic <= 0 {
			return domain.ErrInvalid
		}
		ticket, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		if !ticket.RoutingPending || !ticket.Status.Active() {
			return domain.ErrConflict
		}
		team := a.cfg.Departments[ticket.Department]
		clientTopics, ok := a.store.(domain.ClientTopicStore)
		if !ok {
			return domain.ErrInvalid
		}
		previous, bindingErr := clientTopics.ClientTopic(ctx, ticket.ClientID, ticket.Department)
		if bindingErr != nil && !errors.Is(bindingErr, domain.ErrNotFound) {
			return bindingErr
		}
		if existing, e := a.store.TopicTicket(ctx, team.ChatID, topic); e == nil && existing.ID != ticket.ID {
			return domain.ErrConflict
		} else if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return e
		}
		if _, e = a.client.Send(ctx, team.ChatID, topic, fmt.Sprintf("Проверка привязки заявки #%d. История: /history", id), nil); e != nil {
			return e
		}
		e = a.store.Transact(ctx, func(tx domain.Tx) error {
			actor, e := tx.GetUser(ctx, u.ID)
			if e != nil {
				return e
			}
			if !actor.Approved || actor.Role != "admin" {
				return domain.ErrForbidden
			}
			current, e := tx.GetTicket(ctx, id)
			if e != nil {
				return e
			}
			if current.Department != ticket.Department || !current.RoutingPending || !current.Status.Active() {
				return domain.ErrConflict
			}
			oldChat, oldTopic := current.TopicChatID, current.TopicID
			bindingTx, ok := tx.(domain.ClientTopicTx)
			if !ok {
				return domain.ErrInvalid
			}
			binding := domain.ClientTopic{ClientID: current.ClientID, Department: current.Department, ChatID: team.ChatID, TopicID: topic}
			if errors.Is(bindingErr, domain.ErrNotFound) {
				e = bindingTx.BindClientTopic(ctx, binding)
			} else if previous.ChatID != team.ChatID || previous.TopicID != topic {
				e = bindingTx.ReplaceClientTopic(ctx, binding, previous.TopicID)
			}
			if e != nil {
				return e
			}
			current.TopicChatID = team.ChatID
			current.TopicID = topic
			current.RoutingPending = false
			current.QueueMessageID = 0
			current.QueueMediaType = ""
			current.TopicMessageID = 0
			if e = tx.SaveTicket(ctx, &current); e != nil {
				return e
			}
			data, _ := json.Marshal(map[string]int64{"chat_id": team.ChatID, "topic_id": topic})
			if e = tx.AddEvent(ctx, domain.Event{TicketID: id, ActorID: u.ID, Kind: "topic_bound", Data: data, CreatedAt: time.Now().UTC()}); e != nil {
				return e
			}
			if oldChat != 0 && oldTopic != 0 && (oldChat != team.ChatID || oldTopic != topic) {
				data, _ := json.Marshal(map[string]int64{"chat_id": oldChat, "topic_id": oldTopic})
				if e = tx.AddEvent(ctx, domain.Event{TicketID: id, ActorID: u.ID, Kind: "topic_retired", Data: data, CreatedAt: time.Now().UTC()}); e != nil {
					return e
				}
				if e = tx.Enqueue(ctx, domain.Operation{Kind: "close_topic", TicketID: id, Payload: data, AvailableAt: time.Now().UTC(), State: "pending"}); e != nil {
					return e
				}
			}
			return tx.Enqueue(ctx, domain.Operation{Kind: "refresh_card", TicketID: id, Payload: json.RawMessage(`{}`), AvailableAt: time.Now().UTC(), State: "pending"})
		})
		if e != nil {
			return e
		}
		return a.say(ctx, m, "Топик привязан. Повторите /retry ID для неудачной операции создания топика: повтор использует сохранённую привязку.", nil)
	case "/revoke", "/staff":
		if len(fields) < 2 {
			return domain.ErrInvalid
		}
		id, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || id <= 0 {
			return domain.ErrInvalid
		}
		for _, configured := range a.cfg.AdminIDs {
			if configured == id {
				return a.say(ctx, m, "Измените список базовых администраторов в конфигурации. Их доступ защищён.", nil)
			}
		}
		if fields[0] == "/staff" && (len(fields) < 3 || (fields[2] != "engineer" && fields[2] != "dispatcher" && fields[2] != "admin")) {
			return domain.ErrInvalid
		}
		departments := []domain.Department{}
		if fields[0] == "/staff" {
			for _, field := range fields[3:] {
				d := domain.Department(field)
				if !a.departmentEnabled(d) {
					return domain.ErrInvalid
				}
				departments = append(departments, d)
			}
			if fields[2] == "engineer" && len(departments) == 0 {
				return domain.ErrInvalid
			}
		}
		err = a.store.Transact(ctx, func(tx domain.Tx) error {
			actor, e := tx.GetUser(ctx, u.ID)
			if e != nil {
				return e
			}
			if !actor.Approved || actor.Role != "admin" {
				return domain.ErrForbidden
			}
			user, e := tx.GetUser(ctx, id)
			if e != nil && !errors.Is(e, domain.ErrNotFound) {
				return e
			}
			if errors.Is(e, domain.ErrNotFound) {
				if fields[0] == "/revoke" {
					return e
				}
				user = domain.User{ID: id, Name: fmt.Sprintf("Сотрудник %d", id)}
			}
			if fields[0] == "/revoke" {
				user.Approved = false
			} else {
				user.Role = fields[2]
				user.Approved = true
				user.Departments = departments
				user.CompanyID = 0
			}
			return tx.SaveUser(ctx, user)
		})
		if err != nil {
			return err
		}
		return a.say(ctx, m, "Доступ обновлён.", nil)
	default:
		return domain.ErrInvalid
	}
}

func (a *Adapter) ticketCommand(ctx context.Context, m *Message) error {
	parts := strings.SplitN(strings.TrimSpace(m.Text), " ", 3)
	if len(parts) < 2 || (parts[0] == "/assign" && len(parts) != 3) {
		return domain.ErrInvalid
	}
	id, e := strconv.ParseInt(parts[1], 10, 64)
	if e != nil || id <= 0 {
		return domain.ErrInvalid
	}
	if m.Chat.Type != "private" {
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		team := a.cfg.Departments[t.Department]
		if !a.currentSource(m, t) && !(m.Chat.ID == team.ChatID && m.ThreadID == team.QueueTopicID) {
			return domain.ErrConflict
		}
	}
	switch parts[0] {
	case "/cancel":
		if m.Chat.Type == "private" {
			return a.beginCancel(ctx, m.From.ID, m, id)
		}
		if service, ok := a.service.(interface {
			Cancel(context.Context, int64, int64) error
		}); ok {
			return service.Cancel(ctx, m.From.ID, id)
		}
	case "/assign":
		target, e := strconv.ParseInt(parts[2], 10, 64)
		if e != nil || target <= 0 {
			return domain.ErrInvalid
		}
		if service, ok := a.service.(interface {
			Assign(context.Context, int64, int64, int64) error
		}); ok {
			return service.Assign(ctx, m.From.ID, id, target)
		}
	}
	return domain.ErrInvalid
}
