package telegram

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bot-bvk/internal/domain"
)

type Team struct {
	ChatID       int64
	QueueTopicID int64
	Tags         []string
}

// ClientGroup is a company group chat admitted by the group intake channel.
// CompanyID is resolved from the stable company config key at startup.
type ClientGroup struct {
	CompanyID int64
	Link      string
}

type Config struct {
	Departments  map[domain.Department]Team
	ClientGroups map[int64]ClientGroup
	BotUsername  string
	StartHour    int
	EndHour      int
	AdminIDs     []int64
	ArchiveDir   string
	ReportCSV    func(context.Context) (string, error)
}
type Service interface {
	Register(context.Context, int64, string, int64) error
	ApproveAccess(context.Context, int64, int64, bool) error
	Create(context.Context, int64, domain.Department, string) (domain.Ticket, error)
	Claim(context.Context, int64, int64) error
	Transfer(context.Context, int64, int64, domain.Department, string) error
	Close(context.Context, int64, int64, string) error
	SetWaiting(context.Context, int64, int64, bool) error
	AddMessage(context.Context, int64, int64, domain.Message, bool) error
}

// groupService is implemented by tickets.Service. Keeping it as a separate
// optional interface leaves the private-chat service contract unchanged for
// existing tests and lightweight adapters.
type groupService interface {
	CreateFromGroup(context.Context, domain.GroupTicketRequest) (domain.Ticket, error)
	AddGroupMessage(context.Context, int64, int64, int64, domain.GroupAuthor, domain.Message) error
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
	// answeredCallback records that this update already answered its callback
	// query, so an error path must not answer again and replace the text the user
	// just received. Only the single inbox goroutine writes it.
	answeredCallback bool

	client    *Client
	store     domain.Store
	service   Service
	cfg       Config
	drafts    map[int64]draft
	lastPoll  atomic.Int64
	prompts   map[promptKey]prompt
	auxiliary map[auxKey][]domain.AuxiliaryMessage
	// unrecognised records group chats the bot can see but has no rule for, so
	// a mistyped client_groups entry is reported once instead of on every message.
	unrecognised sync.Map
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
		// A screen that already explained itself to the user must not be replaced
		// by a generic "action unavailable" answer.
		if !a.answeredCallback {
			_ = a.client.Answer(ctx, u.Callback.ID, text)
			a.markCallbackAnswered()
		}
	} else if u.Message != nil && (u.Message.Chat.Type == "private" || a.trustedGroup(u.Message.Chat.ID)) {
		_, _ = a.client.Send(ctx, u.Message.Chat.ID, u.Message.ThreadID, text, nil)
	}
}

// markCallbackAnswered records that the current update answered its callback
// query. Every acknowledgement must go through it so respondError stays quiet.
func (a *Adapter) markCallbackAnswered() { a.answeredCallback = true }
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
	if u.MyChatMember != nil {
		// Adding the bot to a group is the cheapest way to obtain the real chat
		// id: the warning names it even before any message is written there.
		a.noteUnrecognisedChat(u.MyChatMember.Chat)
		return nil
	}
	if u.EditedMessage != nil {
		if a.isClientGroup(u.EditedMessage.Chat.ID) {
			return a.groupEdit(ctx, u.EditedMessage)
		}
		if u.EditedMessage.Chat.Type != "private" && !a.trustedGroup(u.EditedMessage.Chat.ID) {
			a.noteUnrecognisedChat(u.EditedMessage.Chat)
		}
		return a.edit(ctx, u.EditedMessage)
	}
	m := u.Message
	if m == nil {
		return nil
	}
	if m.From.IsBot || m.serviceMessage() {
		return nil
	}
	if a.isClientGroup(m.Chat.ID) {
		if m.From.ID == 0 || m.SenderChat != nil {
			return nil
		}
		return a.groupMessage(ctx, m)
	}
	if m.Chat.Type != "private" && !a.trustedGroup(m.Chat.ID) {
		a.noteUnrecognisedChat(m.Chat)
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
			return a.say(ctx, m, "Активной заявки нет. Нажмите «Создать заявку».", nil)
		}
		if e != nil {
			return e
		}
		if text == "/cancel_current" {
			return a.beginCancel(ctx, m.From.ID, m, t.ID)
		}
		return a.showTicket(ctx, m, user, t)
	}
	if text == "/requests" || text == "/quarantine" || strings.HasPrefix(text, "/quarantine ") || strings.HasPrefix(text, "/bind ") || strings.HasPrefix(text, "/retry ") || strings.HasPrefix(text, "/revoke ") || strings.HasPrefix(text, "/staff ") {
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
	if text == "/reopen" || strings.HasPrefix(text, "/reopen ") {
		return a.say(ctx, m, "Заявка завершена. Создайте новую заявку через меню.", nil)
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
			return a.sayAux(ctx, m, m.From.ID, "draft", "Новая заявка · 1 из 3\nКакой отдел поможет с проблемой?", a.draftDepartments(d))
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
		if !a.departmentEnabled(d.Department) {
			return a.resumeDraft(ctx, m)
		}
		if !d.Urgency.Valid() || d.Step == "change_urgency" {
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

// departmentButtons builds the shared department picker, two per row, from the
// canonical department order so labels and data never diverge from the domain.
func (a *Adapter) configuredDepartments() []domain.Department {
	if a.cfg.Departments == nil {
		return domain.Departments()
	}
	departments := make([]domain.Department, 0, len(a.cfg.Departments))
	for _, department := range domain.Departments() {
		if _, ok := a.cfg.Departments[department]; ok {
			departments = append(departments, department)
		}
	}
	return departments
}

func (a *Adapter) departmentEnabled(department domain.Department) bool {
	if a.cfg.Departments == nil {
		return department.Valid()
	}
	_, ok := a.cfg.Departments[department]
	return ok
}

func (a *Adapter) departmentButtons(prefix string) [][]Button {
	rows := [][]Button{}
	row := []Button{}
	for _, d := range a.configuredDepartments() {
		row = append(row, Button{Text: d.Name(), Data: prefix + ":" + string(d)})
		if len(row) == 2 {
			rows = append(rows, row)
			row = []Button{}
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}
func (a *Adapter) callback(ctx context.Context, c *Callback) error {
	if c.Message != nil && a.isClientGroup(c.Message.Chat.ID) {
		// The group channel has no client-side buttons. Accepting callbacks
		// there would let anyone in the chat act on an internal ticket.
		return domain.ErrForbidden
	}
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
		err = a.callbackCompany(ctx, c, id)
	case "approve", "reject":
		err = a.service.ApproveAccess(ctx, c.From.ID, id, action == "approve")
	case "new":
		return a.callbackNewDraft(ctx, c, parts)
	case "urgency":
		return a.callbackUrgency(ctx, c, parts)
	case "changeurgency":
		return a.callbackChangeUrgency(ctx, c, parts)
	case "draftback":
		return a.callbackDraftBack(ctx, c, parts)
	case "draftreview":
		return a.callbackDraftReview(ctx, c, parts)
	case "submit":
		return a.callbackSubmitDraft(ctx, c, parts)
	case "cancel":
		return a.callbackCancel(ctx, c, id, parts)
	case "claim":
		return a.callbackClaim(ctx, c, id)
	case "waiting":
		err = a.service.SetWaiting(ctx, c.From.ID, id, true)
	case "resume":
		err = a.service.SetWaiting(ctx, c.From.ID, id, false)
	case "reopen":
		if c.Message != nil {
			_ = a.client.Answer(ctx, c.ID, "Заявка завершена. Создайте новую заявку.")
			a.markCallbackAnswered()
			return a.say(ctx, c.Message, "Заявка завершена. Создайте новую заявку через меню.", nil)
		}
		return domain.ErrConflict
	case "close", "transfer":
		err = a.beginStaffPrompt(ctx, c, id, action)
	case "transferdept", "cancelconfirm", "promptcancel":
		err = a.promptCallback(ctx, c, action, id, parts)
	case "tickets":
		return a.callbackTickets(ctx, c, parts)
	case "historyfile":
		return a.callbackHistoryFile(ctx, c, id, parts)
	case "ticket", "history":
		return a.callbackTicketOrHistory(ctx, c, action, id, parts)
	default:
		return domain.ErrInvalid
	}
	if err != nil {
		return err
	}
	if c.Message != nil && (action == "new" || action == "submit" || action == "cancel" && id == 0) {
		_ = a.client.HideMessageKeyboard(ctx, c.Message.Chat.ID, c.Message.ID)
	}
	if action != "company" {
		_ = a.client.Answer(ctx, c.ID, "")
		a.markCallbackAnswered()
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
	if m.Reply != nil {
		if lookup, ok := a.store.(interface {
			MessageTicket(context.Context, int64, int64) (domain.Ticket, error)
		}); ok {
			original, e := lookup.MessageTicket(ctx, m.Chat.ID, m.Reply.ID)
			if e == nil && original.ID != t.ID {
				return a.say(ctx, m, fmt.Sprintf("Это сообщение относится к заявке #%d. Ответьте в рамках текущей заявки #%d отдельным сообщением.", original.ID, t.ID), nil)
			}
			if e != nil && !errors.Is(e, domain.ErrNotFound) {
				return e
			}
		}
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
		return a.say(ctx, m, "Вложение больше 20 МБ: оно передано, но не может быть скачано в независимый архив стандартным Bot API. Отправьте меньший файл или согласуйте другой способ передачи с поддержкой.", nil)
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

func (a *Adapter) clientGroup(chatID int64) (ClientGroup, bool) {
	group, ok := a.cfg.ClientGroups[chatID]
	return group, ok
}

// noteUnrecognisedChat names a group the bot receives messages from but cannot
// use. Without this line a wrong chat_id in client_groups is silent: the bot
// ignores the chat, and getUpdates is not an option because it would confirm
// updates the bot has not stored yet. The real id is printed here once.
func (a *Adapter) noteUnrecognisedChat(chat Chat) {
	if chat.Type == "" || chat.Type == "private" || a.isClientGroup(chat.ID) || a.trustedGroup(chat.ID) {
		return
	}
	if _, seen := a.unrecognised.LoadOrStore(chat.ID, true); seen {
		return
	}
	slog.Warn("message from an unrecognised group chat: add it to client_groups or departments",
		"chat_id", chat.ID, "type", chat.Type, "title", chat.Title,
		"hint", "supergroup ids are -100<internal> where <internal> is the number in a t.me/c/<internal>/<message> link")
}

func (a *Adapter) isClientGroup(chatID int64) bool {
	_, ok := a.clientGroup(chatID)
	return ok
}

func (a *Adapter) groupTags() map[domain.Department][]string {
	tags := make(map[domain.Department][]string, len(a.cfg.Departments))
	for _, department := range a.configuredDepartments() {
		team := a.cfg.Departments[department]
		if len(team.Tags) == 0 {
			tags[department] = domain.DefaultDepartmentTags(department)
			continue
		}
		tags[department] = team.Tags
	}
	return tags
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
