package telegram

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
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
	AddMessage(context.Context, int64, int64, domain.Message, bool) error
}
type DraftStore interface {
	SaveDraft(context.Context, int64, domain.Department, string) error
	Draft(context.Context, int64) (domain.Department, string, error)
	DeleteDraft(context.Context, int64) error
}
type draft = domain.Draft
type Adapter struct {
	activeUpdate   int64
	activeActor    int64
	screenSequence int

	client    *Client
	store     domain.Store
	service   Service
	cfg       Config
	drafts    map[int64]draft
	lastPoll  atomic.Int64
	prompts   map[promptKey]prompt
	auxiliary map[auxKey][]domain.AuxiliaryMessage
}

func NewAdapter(c *Client, s domain.Store, v Service, cfg Config) *Adapter {
	return &Adapter{client: c, store: s, service: v, cfg: cfg, drafts: make(map[int64]draft), prompts: make(map[promptKey]prompt), auxiliary: make(map[auxKey][]domain.AuxiliaryMessage)}
}
func (a *Adapter) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wake := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- a.poll(ctx, wake); close(done) }()
	err := a.drainInbox(ctx, wake, done)
	cancel()
	<-done
	return err
}

// Only this loop mutates adapter state. Polling can keep receiving durably while
// a handler is busy; due retries never wait for a Telegram long poll to finish.
func (a *Adapter) drainInbox(ctx context.Context, wake <-chan struct{}, done <-chan error) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case err := <-done:
			return err
		default:
		}
		// Re-evaluate dependencies after every action: it can create a ticket or
		// change the workflow used to route the next message.
		pending, err := a.store.PendingUpdates(ctx, 1)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			if err = a.processUpdate(ctx, pending[0]); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			// Run also joins the polling goroutine.
			return err
		case <-wake:
		case <-ticker.C:
		}
	}
	return ctx.Err()
}

func (a *Adapter) poll(ctx context.Context, wake chan<- struct{}) error {
	last, err := a.store.LastUpdateID(ctx)
	if err != nil {
		return err
	}
	offset := last + 1
	for ctx.Err() == nil {
		updates, err := a.client.updatesTimeout(ctx, offset, 50)
		if err != nil {
			if !pause(ctx, 2*time.Second) {
				break
			}
			continue
		}
		a.lastPoll.Store(time.Now().UTC().UnixNano())
		for _, u := range updates {
			raw, e := json.Marshal(u)
			if len(u.Raw) > 0 {
				raw = u.Raw
			}
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
		if len(updates) > 0 {
			select {
			case wake <- struct{}{}:
			default:
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
	var private *Message
	var actor int64
	if u.Callback != nil {
		private = u.Callback.Message
		actor = u.Callback.From.ID
	} else if u.Message != nil {
		private = u.Message
		actor = u.Message.From.ID
	}
	if private != nil && private.Chat.Type == "private" {
		if user, e := a.store.User(ctx, actor); e == nil && user.Role == "client" {
			text = clientErrorText(err)
		}
	}
	if u.Callback != nil {
		_ = a.client.Answer(ctx, u.Callback.ID, text)
	} else if u.Message != nil && (u.Message.Chat.Type == "private" || a.trustedGroup(u.Message.Chat.ID)) {
		_, _ = a.client.Send(ctx, u.Message.Chat.ID, u.Message.ThreadID, text, nil)
	}
}
func (a *Adapter) say(ctx context.Context, m *Message, text string, buttons [][]Button) error {
	if a.activeUpdate != 0 {
		var markup any
		if buttons != nil {
			markup = Markup{buttons}
		}
		return a.queueScreen(ctx, m, m.From.ID, "", text, markup)
	}
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
	if m.From.IsBot || m.serviceMessage() {
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
	if command, ok := menuAliases[text]; ok {
		text = command
	}
	if text == "/start" {
		if user, e := a.store.User(ctx, m.From.ID); e == nil && user.Approved {
			if e = a.updateName(ctx, user, m.From); e != nil {
				return e
			}
			key := scope(m.From.ID, m)
			if p, ok, e := a.loadPrompt(ctx, key); e != nil {
				return e
			} else if ok {
				a.finishPrompt(ctx, key, p)
			}
			if e := a.deleteDraft(ctx, m.From.ID); e != nil {
				return e
			}
			a.cleanupBestEffort(ctx, key, "draft")
			return a.menu(ctx, m, user)
		} else if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return e
		}
		return a.registrationCompanies(ctx, m, m.From.ID, false)
	}
	user, err := a.store.User(ctx, m.From.ID)
	if err != nil {
		return err
	}
	if !user.Approved {
		return a.say(ctx, m, "Доступ ожидает подтверждения администратора. Для выбора компании нажмите /start.", nil)
	}
	if err = a.updateName(ctx, user, m.From); err != nil {
		return err
	}
	if text == "/menu" {
		key := scope(m.From.ID, m)
		if p, exists, e := a.loadPrompt(ctx, key); e != nil {
			return e
		} else if exists {
			a.finishPrompt(ctx, key, p)
		}
		if m.Chat.Type == "private" {
			if e := a.deleteDraft(ctx, m.From.ID); e != nil {
				return e
			}
			a.cleanupBestEffort(ctx, key, "draft")
		}
		return a.menu(ctx, m, user)
	}
	if text == "/form" {
		return a.resumeDraft(ctx, m)
	}
	if text == "/help" {
		return a.help(ctx, m, user)
	}
	if text == "/cancel_draft" {
		return a.cancelClientDraft(ctx, m)
	}
	if consumed, e := a.consumePrompt(ctx, m, user); consumed {
		return e
	}
	if text == "/current" || text == "/cancel_current" {
		t, e := a.store.ActiveTicket(ctx, m.From.ID)
		if errors.Is(e, domain.ErrNotFound) {
			return a.say(ctx, m, "Активной заявки нет. Нажмите Новая заявка.", nil)
		}
		if e != nil {
			return e
		}
		if text == "/cancel_current" {
			return a.beginCancel(ctx, m.From.ID, m, t.ID)
		}
		return a.showTicket(ctx, m, user, t)
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
	if text == "/ticket" || strings.HasPrefix(text, "/ticket ") {
		return a.exactTicket(ctx, m, user)
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
		if d, exists, err := a.getDraft(ctx, m.From.ID); err != nil {
			return err
		} else if exists && d.Step != "submitted" {
			return a.resumeDraft(ctx, m)
		}
		d := draft{Generation: a.formGeneration(m), Step: "department"}
		if err := a.saveDraft(ctx, m.From.ID, d); err != nil {
			return err
		}
		a.cleanupBestEffort(ctx, scope(m.From.ID, m), "client_home")
		a.cleanupBestEffort(ctx, scope(m.From.ID, m), "draft")
		if err := a.clientScreen(ctx, m, "draft", "Создание заявки.", clientDraftKeyboard); err != nil {
			return err
		}
		return a.sayAux(ctx, m, m.From.ID, "draft", "Выберите направление.", draftDepartments(d))
	}
	if strings.HasPrefix(text, "/") {
		return a.help(ctx, m, user)
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
	if ok && d.Step != "submitted" {
		if !d.Department.Valid() {
			return a.resumeDraft(ctx, m)
		}
		if strings.HasPrefix(text, "/") {
			return domain.ErrInvalid
		}
		return a.appendDraft(ctx, m, d)
	}
	t, err := a.store.ActiveTicket(ctx, m.From.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return a.say(ctx, m, "Нажмите «Создать заявку» в меню, чтобы описать проблему.", nil)
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
	if action == "claim" || action == "waiting" || action == "resume" || action == "close" || action == "transfer" {
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
		_ = a.client.Answer(ctx, c.ID, "")
		companies, e := a.store.Companies(ctx)
		if e != nil {
			return e
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
	case "approve", "reject":
		err = a.service.ApproveAccess(ctx, c.From.ID, id, action == "approve")
	case "new":
		d, valid, e := a.boundDraft(ctx, c, parts, "department")
		if e != nil || !valid {
			return e
		}
		if len(parts) != 3 || !domain.Department(parts[1]).Valid() {
			return domain.ErrInvalid
		}
		u, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		if !u.Approved || u.Role != "client" {
			return domain.ErrForbidden
		}
		if _, e = a.store.ActiveTicket(ctx, c.From.ID); e == nil {
			return domain.ErrConflict
		} else if !errors.Is(e, domain.ErrNotFound) {
			return e
		}
		d.Department = domain.Department(parts[1])
		d.Step = "description"
		if err = a.saveDraft(ctx, c.From.ID, d); err != nil {
			return err
		}
		a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
		err = a.clientScreen(ctx, c.Message, "draft", draftDescriptionText, clientDraftKeyboard)
	case "submit":
		d, valid, e := a.boundDraft(ctx, c, parts, "confirm")
		if e != nil || !valid {
			return e
		}
		err = a.submitDraft(ctx, c, d)
		if err == nil {
			a.cleanupBestEffort(ctx, scope(c.From.ID, c.Message), "draft")
		}
	case "cancel":
		if id == 0 {
			_, valid, e := a.boundDraft(ctx, c, parts, "")
			if e != nil || !valid {
				return e
			}
			err = a.cancelClientDraft(ctx, c.Message)
		} else {
			err = a.beginCancel(ctx, c.From.ID, c.Message, id)
		}
	case "claim":
		err = a.service.Claim(ctx, c.From.ID, id)
	case "waiting":
		err = a.service.SetWaiting(ctx, c.From.ID, id, true)
	case "resume":
		err = a.service.SetWaiting(ctx, c.From.ID, id, false)
	case "reopen":
		err = a.service.Reopen(ctx, c.From.ID, id)
	case "close", "transfer":
		err = a.beginStaffPrompt(ctx, c, id, action)
	case "transferdept", "cancelconfirm", "promptcancel":
		err = a.promptCallback(ctx, c, action, id, parts)
	case "tickets":
		if c.Message == nil || len(parts) != 3 {
			return domain.ErrInvalid
		}
		page, e := strconv.ParseInt(parts[1], 10, 32)
		if e != nil || page < 0 {
			return domain.ErrInvalid
		}
		anchor, e := strconv.ParseInt(parts[2], 10, 64)
		if e != nil || anchor < 0 {
			return domain.ErrInvalid
		}
		u, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		err = a.ticketListPage(ctx, c.Message, u, int(page), anchor)
	case "historyfile":
		if c.Message == nil || len(parts) != 3 {
			return domain.ErrInvalid
		}
		messageID, e := strconv.ParseInt(parts[2], 10, 64)
		if e != nil || messageID <= 0 {
			return domain.ErrInvalid
		}
		t, e := a.store.Ticket(ctx, id)
		if e != nil {
			return e
		}
		u, e := a.store.User(ctx, c.From.ID)
		if e != nil {
			return e
		}
		err = a.historyFile(ctx, c.Message, u, t, messageID)
	case "ticket", "history":
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
		if action == "ticket" {
			err = a.showTicket(ctx, c.Message, user, t)
		} else {
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
			err = a.historyPage(ctx, c.Message, user, t, int(page), anchor)
		}

	default:
		return domain.ErrInvalid
	}
	if err != nil {
		return err
	}
	if c.Message != nil && (action == "new" || action == "submit" || action == "cancel" && id == 0) {
		_ = a.client.Call(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": c.Message.Chat.ID, "message_id": c.Message.ID, "reply_markup": Markup{Buttons: [][]Button{}}}, nil)
	}
	if action != "company" {
		_ = a.client.Answer(ctx, c.ID, "")
	}
	return nil
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
	if consumed, e := a.engineerKeyboardAction(ctx, m); consumed {
		return e
	}
	text := strings.TrimSpace(m.Text)
	if text == "/help" || text == "/menu" {
		if text == "/menu" {
			key := scope(m.From.ID, m)
			if p, ok, e := a.loadPrompt(ctx, key); e != nil {
				return e
			} else if ok {
				a.finishPrompt(ctx, key, p)
			}
		}
		return a.help(ctx, m, user)
	}
	if consumed, e := a.consumePrompt(ctx, m, user); consumed {
		return e
	}
	if text == "/history" || strings.HasPrefix(text, "/history ") {
		return a.historyCommand(ctx, m, user)
	}
	if text == "/ticket" || strings.HasPrefix(text, "/ticket ") {
		return a.exactTicket(ctx, m, user)
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
	if strings.HasPrefix(strings.TrimSpace(messageText(m)), "/") {
		return a.help(ctx, m, user)
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
	if !provisioningStatus(t.Status) || t.RoutingPending || m.Chat.ID != t.TopicChatID || m.ThreadID != t.TopicID || m.ThreadID == a.cfg.Departments[t.Department].QueueTopicID {
		return nil
	}
	return a.receive(ctx, m, t, true)
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
	msg := domain.Message{AuthorID: m.From.ID, Text: messageText(m), TelegramChatID: m.Chat.ID, TelegramMessageID: m.ID, TelegramTopicID: m.ThreadID, MediaGroupID: m.MediaGroupID, CreatedAt: time.Now().UTC()}
	if m.EditDate != 0 {
		at := time.Unix(m.EditDate, 0).UTC()
		msg.EditedAt = &at
	}
	if m.Reply != nil {
		msg.ReplyToMessageID = m.Reply.ID
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
		if !errors.Is(err, domain.ErrConflict) || m.ID == 0 {
			return err
		}
		// The transaction may have committed before FinishUpdate failed. A
		// confirmed source is a receipt, not a new business conflict for the user.
		messages, lookupErr := a.store.Messages(ctx, t.ID)
		if lookupErr != nil {
			return lookupErr
		}
		accepted := false
		for _, saved := range messages {
			if saved.AuthorID == m.From.ID && saved.TelegramChatID == m.Chat.ID && saved.TelegramMessageID == m.ID && (saved.Direction == "inbound" || saved.Direction == "outbound") {
				accepted = true
				break
			}
		}
		if !accepted {
			return err
		}
	}
	if msg.FileSize > 20<<20 {
		return a.say(ctx, m, "Вложение больше 20 МБ: оно передано, но не может быть скачано в независимый архив стандартным Bot API. Отправьте меньший файл или согласуйте другой способ передачи с поддержкой. Топик не будет удалён без архива.", nil)
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
				user, err := tx.GetUser(ctx, m.From.ID)
				if err != nil {
					return err
				}
				if !user.Approved || original.AuthorID != m.From.ID || !historyAuthorized(user, t) {
					return domain.ErrForbidden
				}
				if _, err = tx.GetTicket(ctx, t.ID); err != nil {
					return err
				}
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
	return a.historyPage(ctx, m, u, t, 0, 0)
}
func (a *Adapter) sendChunks(ctx context.Context, m *Message, text string) error {
	for _, part := range splitText(text, messageLimit) {
		if err := a.say(ctx, m, part, nil); err != nil {
			return err
		}
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
	if store, ok := a.store.(domain.BoundDraftStore); ok {
		d, err := store.LoadDraft(ctx, id)
		if errors.Is(err, domain.ErrNotFound) {
			return draft{}, false, nil
		}
		return d, err == nil, err
	}
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
	if store, ok := a.store.(domain.BoundDraftStore); ok {
		return store.PutDraft(ctx, id, d)
	}
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
		return m.Chat.ID == u.ID
	}
	if a.cfg.Departments[t.Department].ChatID == m.Chat.ID {
		return true
	}
	return false
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
	return a.ticketListPage(ctx, m, u, 0, 0)
}
