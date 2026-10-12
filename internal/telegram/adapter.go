package telegram

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bot-bvk/internal/domain"
)

type Team struct {
	ChatID       int64
	QueueTopicID int64
}
type Config struct {
	Departments       map[domain.Department]Team
	DispatcherChatID  int64
	AdminIDs          []int64
	ArchiveDir        string
	ReportCSV         func(context.Context) (string, error)
	BeforeDelete      func(context.Context, domain.Tx, domain.Ticket, int64, int64) error
	BusinessHoursText string
}
type Service interface {
	Register(context.Context, int64, string, int64) error
	ApproveAccess(context.Context, int64, int64, bool) error
	Create(context.Context, int64, domain.Department, string) (domain.Ticket, error)
	Claim(context.Context, int64, int64) error
	Transfer(context.Context, int64, int64, domain.Department, string) error
	Close(context.Context, int64, int64, string) error
	Reopen(context.Context, int64, int64) error
	SetWaiting(context.Context, int64, int64, bool) error
	SetEmergency(context.Context, int64, int64) error
	AddMessage(context.Context, int64, int64, domain.Message, bool) error
}
type DraftStore interface {
	SaveDraft(context.Context, int64, domain.Department, string) error
	Draft(context.Context, int64) (domain.Department, string, error)
	DeleteDraft(context.Context, int64) error
}
type draft struct {
	Department domain.Department
	Text       string
}
type Adapter struct {
	client   *Client
	store    domain.Store
	service  Service
	cfg      Config
	drafts   map[int64]draft
	lastPoll atomic.Int64
}

func NewAdapter(c *Client, s domain.Store, v Service, cfg Config) *Adapter {
	return &Adapter{client: c, store: s, service: v, cfg: cfg, drafts: make(map[int64]draft)}
}
func (a *Adapter) Run(ctx context.Context) error {
	last, err := a.store.LastUpdateID(ctx)
	if err != nil {
		return err
	}
	offset := last + 1
	for ctx.Err() == nil {
		pending, err := a.store.PendingUpdates(ctx, 100)
		if err != nil {
			return err
		}
		failed := false
		for _, entry := range pending {
			var u Update
			if err = json.Unmarshal(entry.Payload, &u); err != nil {
				return fmt.Errorf("invalid stored update %d: %w", entry.ID, err)
			}
			err = a.Handle(ctx, u)
			if err != nil {
				if errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
					a.respondError(ctx, u, err)
				} else {
					failed = true
					break
				}
			}
			if err = a.store.FinishUpdate(ctx, entry.ID); err != nil {
				return err
			}
		}
		if !failed && len(pending) == 100 {
			continue
		}
		if failed {
			if !pause(ctx, 2*time.Second) {
				break
			}
			continue
		}
		updates, err := a.client.Updates(ctx, offset)
		if err != nil {
			if !pause(ctx, 2*time.Second) {
				break
			}
			continue
		}
		a.lastPoll.Store(time.Now().UTC().UnixNano())
		for _, u := range updates {
			raw, e := json.Marshal(u)
			if e != nil {
				return e
			}
			if _, e = a.store.SaveUpdate(ctx, u.ID, raw); e != nil {
				return e
			}
			if u.ID >= offset {
				offset = u.ID + 1
			}
		}
	}
	return ctx.Err()
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func (a *Adapter) respondError(ctx context.Context, u Update, err error) {
	text := "Действие недоступно: " + err.Error()
	if u.Callback != nil {
		_ = a.client.Answer(ctx, u.Callback.ID, text)
	} else if u.Message != nil && (u.Message.Chat.Type == "private" || a.trustedGroup(u.Message.Chat.ID)) {
		_, _ = a.client.Send(ctx, u.Message.Chat.ID, u.Message.ThreadID, text, nil)
	}
}
func (a *Adapter) say(ctx context.Context, m *Message, text string, buttons [][]Button) error {
	_, err := a.client.Send(ctx, m.Chat.ID, m.ThreadID, text, buttons)
	return err
}
func (a *Adapter) Handle(ctx context.Context, u Update) error {
	if u.Callback != nil {
		return a.callback(ctx, u.Callback)
	}
	if u.EditedMessage != nil {
		return a.edit(ctx, u.EditedMessage)
	}
	m := u.Message
	if m == nil {
		return nil
	}
	if m.Chat.Type != "private" && !a.trustedGroup(m.Chat.ID) {
		return domain.ErrForbidden
	}
	if m.From.ID == 0 || m.SenderChat != nil {
		return domain.ErrForbidden
	}
	if m.Chat.Type != "private" {
		return a.staff(ctx, m)
	}
	text := strings.TrimSpace(m.Text)
	if text == "/start" {
		if user, e := a.store.User(ctx, m.From.ID); e == nil && user.Approved {
			return a.say(ctx, m, "Вы зарегистрированы. Новая заявка: /new. История активной заявки: /history.", nil)
		} else if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return e
		}
		companies, err := a.store.Companies(ctx)
		if err != nil {
			return err
		}
		buttons := [][]Button{}
		for _, c := range companies {
			buttons = append(buttons, []Button{{Text: c.Name, Data: fmt.Sprintf("company:%d", c.ID)}})
		}
		return a.say(ctx, m, "Выберите компанию. Доступ подтвердит администратор.", buttons)
	}
	user, err := a.store.User(ctx, m.From.ID)
	if err != nil {
		return err
	}
	if !user.Approved {
		return domain.ErrForbidden
	}
	if text == "/requests" || strings.HasPrefix(text, "/bind ") || strings.HasPrefix(text, "/retry ") || strings.HasPrefix(text, "/revoke ") || strings.HasPrefix(text, "/staff ") {
		return a.admin(ctx, m, user)
	}
	if strings.HasPrefix(text, "/cancel ") || strings.HasPrefix(text, "/assign ") {
		return a.ticketCommand(ctx, m)
	}
	if text == "/report" {
		return a.report(ctx, m, user)
	}
	if text == "/history" || strings.HasPrefix(text, "/history ") {
		return a.historyCommand(ctx, m, user)
	}
	if text == "/tickets" {
		return a.ticketList(ctx, m, user)
	}
	if strings.HasPrefix(text, "/reopen ") {
		id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(text, "/reopen ")), 10, 64)
		if err != nil {
			return domain.ErrInvalid
		}
		return a.service.Reopen(ctx, m.From.ID, id)
	}
	if text == "/new" || text == "Новая заявка" {
		if _, err := a.store.ActiveTicket(ctx, m.From.ID); err == nil {
			return a.say(ctx, m, "У вас уже есть открытая заявка. Следующие сообщения добавляются к ней.", nil)
		} else if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		return a.say(ctx, m, "Выберите направление.", departmentButtons("new"))
	}
	d, ok, draftErr := a.getDraft(ctx, m.From.ID)
	if draftErr != nil {
		return draftErr
	}
	if ok {
		if _, e := a.store.ActiveTicket(ctx, m.From.ID); e == nil {
			if e = a.deleteDraft(ctx, m.From.ID); e != nil {
				return e
			}
			ok = false
		} else if !errors.Is(e, domain.ErrNotFound) {
			return e
		}
	}
	if ok {
		if strings.HasPrefix(text, "/") {
			return domain.ErrInvalid
		}
		d.Text = messageText(m)
		if strings.TrimSpace(d.Text) == "" {
			return a.say(ctx, m, "Сначала опишите проблему текстом. Вложения можно отправить после создания заявки.", nil)
		}
		if err := a.saveDraft(ctx, m.From.ID, d); err != nil {
			return err
		}
		return a.say(ctx, m, "Создать заявку?\n"+d.Text, [][]Button{{{Text: "Отправить", Data: "submit"}, {Text: "Отменить", Data: "cancel"}}})
	}
	t, err := a.store.ActiveTicket(ctx, m.From.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return a.say(ctx, m, "Создайте заявку командой /new.", nil)
		}
		return err
	}
	return a.receive(ctx, m, t, false)
}
func departmentButtons(prefix string) [][]Button {
	return [][]Button{{{Text: "1С", Data: prefix + ":1c"}, {Text: "IT", Data: prefix + ":it"}}, {{Text: "Монтаж", Data: prefix + ":installation"}, {Text: "Выезд", Data: prefix + ":field"}}}
}
func (a *Adapter) callback(ctx context.Context, c *Callback) error {
	if c.Message != nil && c.Message.Chat.Type != "private" && !a.trustedGroup(c.Message.Chat.ID) {
		return domain.ErrForbidden
	}
	parts := strings.Split(c.Data, ":")
	action := parts[0]
	id := int64(0)
	if len(parts) > 1 {
		id, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	if action == "claim" || action == "waiting" || action == "resume" || action == "emergency" || action == "close" || action == "transfer" {
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		if c.Message == nil || !a.currentSource(c.Message, t) {
			return domain.ErrConflict
		}
	}
	var err error
	switch action {
	case "company":
		if c.Message == nil || c.Message.Chat.Type != "private" {
			return domain.ErrForbidden
		}
		companies, e := a.store.Companies(ctx)
		if e != nil {
			return e
		}
		valid := false
		for _, co := range companies {
			valid = valid || co.ID == id
		}
		if !valid {
			return domain.ErrInvalid
		}
		err = a.service.Register(ctx, c.From.ID, strings.TrimSpace(c.From.FirstName+" "+c.From.LastName), id)
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
	case "approve", "reject":
		err = a.service.ApproveAccess(ctx, c.From.ID, id, action == "approve")
	case "new":
		if c.Message == nil || c.Message.Chat.Type != "private" {
			return domain.ErrForbidden
		}
		user, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		if !user.Approved {
			return domain.ErrForbidden
		}
		if len(parts) != 2 || !domain.Department(parts[1]).Valid() {
			return domain.ErrInvalid
		}
		if err := a.saveDraft(ctx, c.From.ID, draft{Department: domain.Department(parts[1])}); err != nil {
			return err
		}
		_, err = a.client.Send(ctx, c.From.ID, 0, "Опишите проблему текстом. Вложения отправьте после создания заявки.", nil)
	case "submit":
		d, ok, e := a.getDraft(ctx, c.From.ID)
		if e != nil {
			return e
		}
		if !ok || d.Text == "" {
			return domain.ErrInvalid
		}
		var t domain.Ticket
		t, err = a.service.Create(ctx, c.From.ID, d.Department, d.Text)
		if err == nil {
			if err = a.deleteDraft(ctx, c.From.ID); err != nil {
				return err
			}
			hours := a.cfg.BusinessHoursText
			if hours == "" {
				hours = "Пн–Пт, 09:00–18:00 по Иркутску"
			}
			_, err = a.client.Send(ctx, c.From.ID, 0, fmt.Sprintf("Заявка #%d сохранена. Пишите дополнения здесь. Обычная обработка: %s. Срочность вне графика определяет диспетчер.", t.ID, hours), nil)
		}
	case "cancel":
		err = a.deleteDraft(ctx, c.From.ID)
	case "claim":
		err = a.service.Claim(ctx, c.From.ID, id)
	case "waiting":
		err = a.service.SetWaiting(ctx, c.From.ID, id, true)
	case "resume":
		err = a.service.SetWaiting(ctx, c.From.ID, id, false)
	case "emergency":
		err = a.service.SetEmergency(ctx, c.From.ID, id)
	case "reopen":
		err = a.service.Reopen(ctx, c.From.ID, id)
	case "close", "transfer":
		if c.Message == nil {
			return domain.ErrInvalid
		}
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		user, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		if !staffAuthorized(user, t) {
			return domain.ErrForbidden
		}
		if action == "close" {
			_, err = a.client.Send(ctx, c.Message.Chat.ID, c.Message.ThreadID, fmt.Sprintf("Для закрытия: /close %d итог решения", id), nil)
		} else {
			_, err = a.client.Send(ctx, c.Message.Chat.ID, c.Message.ThreadID, fmt.Sprintf("Для передачи: /transfer %d it|1c|installation|field причина", id), nil)
		}
	default:
		return domain.ErrInvalid
	}
	if err != nil {
		return err
	}
	return a.client.Answer(ctx, c.ID, "Готово")
}
func staffAuthorized(u domain.User, t domain.Ticket) bool {
	if !u.Approved {
		return false
	}
	if u.Role == "admin" || u.Role == "dispatcher" {
		return true
	}
	if u.Role != "engineer" {
		return false
	}
	for _, d := range u.Departments {
		if d == t.Department {
			return true
		}
	}
	return false
}
func (a *Adapter) staff(ctx context.Context, m *Message) error {
	user, err := a.store.User(ctx, m.From.ID)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(m.Text)
	if text == "/history" || strings.HasPrefix(text, "/history ") {
		return a.historyCommand(ctx, m, user)
	}
	if text == "/tickets" {
		return a.ticketList(ctx, m, user)
	}
	if strings.HasPrefix(text, "/cancel ") || strings.HasPrefix(text, "/assign ") {
		return a.ticketCommand(ctx, m)
	}
	if text == "/report" {
		return a.report(ctx, m, user)
	}
	if strings.HasPrefix(text, "/close ") || strings.HasPrefix(text, "/transfer ") {
		fields := strings.SplitN(text, " ", 3)
		if len(fields) != 3 {
			return domain.ErrInvalid
		}
		id, e := strconv.ParseInt(fields[1], 10, 64)
		if e != nil {
			return domain.ErrInvalid
		}
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		if !staffAuthorized(user, t) {
			return domain.ErrForbidden
		}
		team := a.cfg.Departments[t.Department]
		if !a.currentSource(m, t) && !(m.Chat.ID == team.ChatID && m.ThreadID == team.QueueTopicID) {
			return domain.ErrConflict
		}
		if fields[0] == "/close" {
			return a.service.Close(ctx, m.From.ID, id, fields[2])
		}
		rest := strings.SplitN(fields[2], " ", 2)
		if len(rest) != 2 {
			return domain.ErrInvalid
		}
		return a.service.Transfer(ctx, m.From.ID, id, domain.Department(rest[0]), rest[1])
	}
	t, err := a.store.TopicTicket(ctx, m.Chat.ID, m.ThreadID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !staffAuthorized(user, t) {
		return domain.ErrForbidden
	}
	if text == "/history" {
		return a.history(ctx, m, user, t)
	}
	body := messageText(m)
	public := body == "/reply" || strings.HasPrefix(body, "/reply ") || strings.HasPrefix(body, "/reply\n")
	if public {
		if m.Reply == nil {
			return a.say(ctx, m, "Отправляйте /reply ответом на сообщение клиента.", nil)
		}
		events, e := a.store.Events(ctx, t.ID)
		if e != nil {
			return e
		}
		matches := false
		for _, event := range events {
			if event.Kind != "message_delivered" {
				continue
			}
			var delivery struct {
				ChatID    int64  `json:"telegram_chat_id"`
				MessageID int64  `json:"telegram_message_id"`
				Direction string `json:"direction"`
			}
			if json.Unmarshal(event.Data, &delivery) == nil && delivery.ChatID == m.Chat.ID && delivery.MessageID == m.Reply.ID && delivery.Direction == "inbound" {
				matches = true
			}
		}
		if !matches {
			return domain.ErrInvalid
		}
		if t.AssigneeID != user.ID && user.Role != "admin" && user.Role != "dispatcher" {
			return domain.ErrForbidden
		}
	}
	return a.receive(ctx, m, t, public)
}
func (a *Adapter) currentSource(m *Message, t domain.Ticket) bool {
	team := a.cfg.Departments[t.Department]
	return m.Chat.ID == t.TopicChatID && t.TopicID != 0 && m.ThreadID == t.TopicID || m.Chat.ID == team.ChatID && m.ThreadID == team.QueueTopicID && t.QueueMessageID != 0 && m.ID == t.QueueMessageID
}
func messageText(m *Message) string {
	if m.Text != "" {
		return m.Text
	}
	return m.Caption
}
func normalise(m *Message, public bool) domain.Message {
	msg := domain.Message{AuthorID: m.From.ID, Text: messageText(m), TelegramChatID: m.Chat.ID, TelegramMessageID: m.ID, MediaGroupID: m.MediaGroupID, CreatedAt: time.Now().UTC()}
	if m.EditDate != 0 {
		at := time.Unix(m.EditDate, 0).UTC()
		msg.EditedAt = &at
	}
	if m.Reply != nil {
		msg.ReplyToMessageID = m.Reply.ID
	}
	if public {
		msg.Text = strings.TrimSpace(strings.TrimPrefix(msg.Text, "/reply"))
	}
	var f *File
	switch {
	case m.Document != nil:
		f = m.Document
		msg.MediaType = "document"
	case len(m.Photo) > 0:
		f = &m.Photo[len(m.Photo)-1]
		msg.MediaType = "photo"
	case m.Voice != nil:
		f = m.Voice
		msg.MediaType = "voice"
	case m.Video != nil:
		f = m.Video
		msg.MediaType = "video"
	case m.Audio != nil:
		f = m.Audio
		msg.MediaType = "audio"
	}
	if f != nil {
		msg.FileID = f.ID
		msg.FileName = f.Name
		msg.FileSize = f.Size
	}
	return msg
}
func (a *Adapter) receive(ctx context.Context, m *Message, t domain.Ticket, public bool) error {
	msg := normalise(m, public)
	if msg.Text == "" && msg.FileID == "" {
		return a.say(ctx, m, "Этот формат не поддерживается. Отправьте текст, фото, документ, голосовое или видео.", nil)
	}
	if err := a.service.AddMessage(ctx, m.From.ID, t.ID, msg, public); err != nil {
		return err
	}
	if msg.FileSize > 20<<20 {
		return a.say(ctx, m, "Вложение больше 20 МБ: оно передано, но не может быть скачано в независимый архив стандартным Bot API. Отправьте меньший файл или согласуйте другой способ передачи с диспетчером. Топик не будет удалён без архива.", nil)
	}
	return nil
}
func (a *Adapter) edit(ctx context.Context, m *Message) error {
	if m.Chat.Type != "private" && !a.trustedGroup(m.Chat.ID) {
		return domain.ErrForbidden
	}
	if m.From.ID == 0 || m.SenderChat != nil {
		return domain.ErrForbidden
	}
	var t domain.Ticket
	var err error
	if lookup, ok := a.store.(interface {
		MessageTicket(context.Context, int64, int64) (domain.Ticket, error)
	}); ok {
		t, err = lookup.MessageTicket(ctx, m.Chat.ID, m.ID)
	} else if m.Chat.Type == "private" {
		t, err = a.store.ActiveTicket(ctx, m.From.ID)
	} else {
		t, err = a.store.TopicTicket(ctx, m.Chat.ID, m.ThreadID)
	}
	if err != nil {
		return err
	}
	normalized := normalise(m, false)
	originals, e := a.store.Messages(ctx, t.ID)
	if e != nil {
		return e
	}
	for _, original := range originals {
		if original.TelegramChatID == m.Chat.ID && original.TelegramMessageID == m.ID && original.FileID != normalized.FileID {
			data, _ := json.Marshal(map[string]any{"telegram_message_id": m.ID, "old_file_id": original.FileID, "new_file_id": normalized.FileID, "new_media_type": normalized.MediaType, "new_file_size": normalized.FileSize, "new_file_name": normalized.FileName})
			if e = a.store.Transact(ctx, func(tx domain.Tx) error {
				return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: m.From.ID, Kind: "media_edit_unsupported", Data: data, CreatedAt: time.Now().UTC()})
			}); e != nil {
				return e
			}
			if e = a.say(ctx, m, "Замена вложения через редактирование не поддерживается в архиве. Исходный файл сохранён; пришлите новый файл отдельным сообщением.", nil); e != nil {
				return e
			}
		}
	}
	if editor, ok := a.service.(interface {
		EditMessage(context.Context, int64, int64, domain.Message) error
	}); ok {
		return editor.EditMessage(ctx, m.From.ID, t.ID, normalise(m, false))
	}
	return domain.ErrInvalid
}

func (a *Adapter) history(ctx context.Context, m *Message, u domain.User, t domain.Ticket) error {
	if !historyAuthorized(u, t) || !a.historyDestination(m, u, t) {
		return domain.ErrForbidden
	}
	messages, err := a.store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "История #%d · компания %d · %s\nИсходное обращение: %s\n", t.ID, t.CompanyID, t.Status, t.Description)
	for _, v := range messages {
		if u.Role == "client" && v.Direction == "internal" {
			continue
		}
		fmt.Fprintf(&b, "%s %d [%s]: %s", v.CreatedAt.UTC().Format(time.RFC3339), v.AuthorID, v.Direction, v.Text)
		if v.FileID != "" {
			state := "не сохранено в независимом архиве"
			if v.ArchivePath != "" && v.SHA256 != "" {
				state = "сохранено в архиве"
			}
			fmt.Fprintf(&b, " [вложение #%d: %s (%s), %s]", v.ID, v.FileName, v.MediaType, state)
		}
		b.WriteByte('\n')
		for _, revision := range v.Revisions {
			fmt.Fprintf(&b, "  Предыдущая версия (%s): %s\n", revision.EditedAt.UTC().Format(time.RFC3339), revision.Text)
		}
	}
	return a.sendChunks(ctx, m, b.String())
}
func (a *Adapter) sendChunks(ctx context.Context, m *Message, text string) error {
	r := []rune(text)
	for len(r) > 0 {
		n := len(r)
		if n > 3500 {
			n = 3500
		}
		if err := a.say(ctx, m, string(r[:n]), nil); err != nil {
			return err
		}
		r = r[n:]
	}
	return nil
}
func (a *Adapter) report(ctx context.Context, m *Message, u domain.User) error {
	if m.Chat.Type != "private" && !a.trustedGroup(m.Chat.ID) {
		return domain.ErrForbidden
	}
	if !u.Approved || (u.Role != "admin" && u.Role != "dispatcher") {
		return domain.ErrForbidden
	}
	if a.cfg.ReportCSV != nil {
		text, err := a.cfg.ReportCSV(ctx)
		if err != nil {
			return err
		}
		return a.client.SendCSV(ctx, u.ID, 0, text)
	}
	tickets, err := a.store.Tickets(ctx)
	if err != nil {
		return err
	}
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"id", "company_id", "department", "status", "assignee_id", "created_at", "closed_at", "closed_by"})
	for _, t := range tickets {
		closed := ""
		if t.ClosedAt != nil {
			closed = t.ClosedAt.Format(time.RFC3339)
		}
		_ = w.Write([]string{strconv.FormatInt(t.ID, 10), strconv.FormatInt(t.CompanyID, 10), string(t.Department), string(t.Status), strconv.FormatInt(t.AssigneeID, 10), t.CreatedAt.Format(time.RFC3339), closed, strconv.FormatInt(t.ClosedBy, 10)})
	}
	w.Flush()
	if err = w.Error(); err != nil {
		return err
	}
	return a.client.SendCSV(ctx, u.ID, 0, b.String())
}

func (a *Adapter) getDraft(ctx context.Context, id int64) (draft, bool, error) {
	if store, ok := a.store.(DraftStore); ok {
		dept, text, err := store.Draft(ctx, id)
		if errors.Is(err, domain.ErrNotFound) {
			return draft{}, false, nil
		}
		return draft{Department: dept, Text: text}, err == nil, err
	}
	d, ok := a.drafts[id]
	return d, ok, nil
}
func (a *Adapter) saveDraft(ctx context.Context, id int64, d draft) error {
	if store, ok := a.store.(DraftStore); ok {
		return store.SaveDraft(ctx, id, d.Department, d.Text)
	}
	a.drafts[id] = d
	return nil
}
func (a *Adapter) deleteDraft(ctx context.Context, id int64) error {
	if store, ok := a.store.(DraftStore); ok {
		return store.DeleteDraft(ctx, id)
	}
	delete(a.drafts, id)
	return nil
}

func (a *Adapter) LastPoll() time.Time {
	n := a.lastPoll.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

func (a *Adapter) trustedGroup(chatID int64) bool {
	if chatID == 0 {
		return false
	}
	if a.cfg.DispatcherChatID == chatID {
		return true
	}
	for _, team := range a.cfg.Departments {
		if team.ChatID == chatID {
			return true
		}
	}
	return false
}
func historyAuthorized(u domain.User, t domain.Ticket) bool {
	return u.Approved && ((u.Role == "client" && u.ID == t.ClientID && u.CompanyID == t.CompanyID) || staffAuthorized(u, t))
}
func (a *Adapter) historyDestination(m *Message, u domain.User, t domain.Ticket) bool {
	if m.Chat.Type == "private" {
		return true
	}
	if a.cfg.Departments[t.Department].ChatID == m.Chat.ID {
		return true
	}
	return a.cfg.DispatcherChatID == m.Chat.ID && (u.Role == "admin" || u.Role == "dispatcher")
}
func (a *Adapter) historyCommand(ctx context.Context, m *Message, u domain.User) error {
	parts := strings.Fields(m.Text)
	var t domain.Ticket
	var err error
	if len(parts) == 2 {
		id, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || id <= 0 {
			return domain.ErrInvalid
		}
		t, err = a.store.Ticket(ctx, id)
	} else if len(parts) == 1 {
		if m.Chat.Type == "private" {
			t, err = a.store.ActiveTicket(ctx, m.From.ID)
		} else {
			t, err = a.store.TopicTicket(ctx, m.Chat.ID, m.ThreadID)
		}
	} else {
		return domain.ErrInvalid
	}
	if err != nil {
		return err
	}
	return a.history(ctx, m, u, t)
}
func (a *Adapter) ticketList(ctx context.Context, m *Message, u domain.User) error {
	if !u.Approved {
		return domain.ErrForbidden
	}
	tickets, err := a.store.Tickets(ctx)
	if err != nil {
		return err
	}
	sort.Slice(tickets, func(i, j int) bool { return tickets[i].ID > tickets[j].ID })
	var b strings.Builder
	b.WriteString("Последние заявки (до 50). История: /history ID\n")
	count := 0
	for _, t := range tickets {
		if !historyAuthorized(u, t) || !a.historyDestination(m, u, t) {
			continue
		}
		fmt.Fprintf(&b, "#%d · %s · %s · компания %d\n", t.ID, t.Department, t.Status, t.CompanyID)
		count++
		if count == 50 {
			break
		}
	}
	if count == 0 {
		b.WriteString("Заявок нет.\n")
	}
	return a.sendChunks(ctx, m, b.String())
}
