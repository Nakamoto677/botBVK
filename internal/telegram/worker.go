package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bot-bvk/internal/domain"
)

type Worker struct {
	currentOperation int64

	client   *Client
	store    domain.Store
	cfg      Config
	rates    *deliveryRates
	interval time.Duration
}

func NewWorker(c *Client, s domain.Store, cfg Config) *Worker {
	return &Worker{client: c, store: s, cfg: cfg, rates: &deliveryRates{lastChat: make(map[int64]time.Time)}, interval: 3100 * time.Millisecond}
}

// Each operation has isolated send-progress state. The database fences ticket
// and conversation order; all jobs share the same per-chat rate limiter.
func (w *Worker) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		id  int64
		err error
	}
	results := make(chan result, 4)
	active := make(map[int64]bool)
	running := 0
	defer func() {
		cancel()
		for running > 0 {
			<-results
			running--
		}
	}()
	for ctx.Err() == nil {
		if running < 4 {
			var op domain.Operation
			var err error
			if queue, ok := w.store.(interface {
				NextOperationExcluding(context.Context, []int64) (domain.Operation, error)
			}); ok {
				ids := make([]int64, 0, len(active))
				for id := range active {
					ids = append(ids, id)
				}
				op, err = queue.NextOperationExcluding(ctx, ids)
			} else {
				op, err = w.store.NextOperation(ctx)
			}
			if err == nil {
				running++
				active[op.ID] = true
				job := *w
				go func() { results <- result{op.ID, job.runOperation(ctx, op)} }()
				continue
			}
			if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case result := <-results:
			timer.Stop()
			running--
			delete(active, result.id)
			if result.err != nil {
				return result.err
			}
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (w *Worker) runOperation(ctx context.Context, op domain.Operation) error {
	// Per-kind operation duration shows which step really costs the seconds, for
	// example create_topic against notify_client.
	started := time.Now()
	defer func() {
		if w.client != nil && w.client.Metrics != nil {
			w.client.Metrics.ObserveOperation(op.Kind, time.Since(started))
		}
	}()
	if err := w.Handle(ctx, op); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dead := false
		delay := time.Duration(1<<min(op.Attempts, 8)) * time.Second
		var api *APIError
		if errors.As(err, &api) {
			if api.RetryAfter > 0 {
				delay = time.Duration(api.RetryAfter) * time.Second
			}
			dead = (api.Code >= 400 && api.Code < 500 && api.Code != 429) || (op.Kind == "create_topic" && api.Ambiguous)
		}
		if errors.Is(err, domain.ErrInvalid) || errors.Is(err, domain.ErrForbidden) {
			dead = true
		}
		dead = dead || op.Attempts >= 12
		if op.Kind == "delete_topic" && !errors.Is(err, domain.ErrInvalid) && !errors.Is(err, domain.ErrForbidden) {
			dead = false
			if op.Attempts == 0 || op.Attempts == 11 || op.Attempts > 0 && op.Attempts%100 == 0 {
				if e := w.notifyAdmins(ctx, fmt.Sprintf("Удаление прежнего топика заявки #%d отложено: %s. Бот продолжит автоматически после восстановления доступа.", op.TicketID, err)); e != nil {
					return e
				}
			}
		}
		if op.Kind == "client_message" && (op.Attempts <= 1 || dead) {
			if e := w.notifyAuthor(ctx, op, err, dead); e != nil {
				return e
			}
		}
		if e := w.store.RetryOperation(ctx, op.ID, err.Error(), delay, dead); e != nil {
			return e
		}
		if dead && op.Kind != "access_notification" {
			if e := w.notifyAdmins(ctx, fmt.Sprintf("Ошибка операции %d (%s), заявка #%d. Требуется проверка: %s", op.ID, op.Kind, op.TicketID, err)); e != nil {
				return e
			}
		}

	} else if err = w.store.FinishOperation(ctx, op.ID); err != nil {
		return err
	}
	return nil
}

type deliveryRates struct {
	mu       sync.Mutex
	lastChat map[int64]time.Time
	global   time.Time
}

// rate reserves a delivery slot for one call. Only a method that publishes a new
// message into the chat consumes the per-chat message budget; edits, topic
// lifecycle calls and deletions are not charged it, because they do not add to the
// chat's message stream. That single distinction is what keeps topic preparation
// from paying 3.1 s per edit.
func (w *Worker) rate(ctx context.Context, chat int64, method string) error {
	waitStart := time.Now()
	charged := publishesNewMessage(method)
	// Reserve a slot while holding the lock, then wait without blocking other chats.
	w.rates.mu.Lock()
	if charged {
		interval := w.interval
		if chat > 0 && interval > 1100*time.Millisecond {
			interval = 1100 * time.Millisecond
		}
		at := time.Now()
		if next := w.rates.lastChat[chat].Add(interval); next.After(at) {
			at = next
		}
		w.rates.lastChat[chat] = at
		w.rates.mu.Unlock()
		if wait := time.Until(at); wait > 0 && !pause(ctx, wait) {
			return ctx.Err()
		}
	} else {
		w.rates.mu.Unlock()
	}
	if w.interval > 0 {
		w.rates.mu.Lock()
		at := time.Now()
		if next := w.rates.global.Add(35 * time.Millisecond); next.After(at) {
			at = next
		}
		w.rates.global = at
		w.rates.mu.Unlock()
		if wait := time.Until(at); wait > 0 && !pause(ctx, wait) {
			return ctx.Err()
		}
	}
	if w.client != nil && w.client.Metrics != nil {
		w.client.Metrics.ObserveMilliseconds("bvk_ratelimit_wait", method, time.Since(waitStart))
	}
	return ctx.Err()
}

// send delivers one message on the shared path. Every operation-scoped send goes
// through the recorded parts fence so a retry after a crash resumes rather than
// duplicating. parseMode is optional and only engineer cards need HTML; client
// text stays unparsed because it can contain unescaped markup characters.
func (w *Worker) send(ctx context.Context, chat, topic int64, text string, buttons [][]Button, parseMode ...string) (Message, error) {
	if w.currentOperation > 0 {
		hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", chat, topic, text)))
		var markup any
		if buttons != nil {
			markup = Markup{buttons}
		}
		return w.textParts(ctx, fmt.Sprintf("operation:%d:%x", w.currentOperation, hash), chat, topic, text, markup, parseMode...)
	}
	if err := w.rate(ctx, chat, "sendMessage"); err != nil {
		return Message{}, err
	}
	return w.client.Send(ctx, chat, topic, text, buttons, parseMode...)
}
func card(t domain.Ticket) string {
	return engineerCardText(t, "Клиент", "Компания", "Специалист")
}
func cardButtons(t domain.Ticket) [][]Button { return engineerQueueButtons(t) }
func (w *Worker) Handle(ctx context.Context, op domain.Operation) error {
	w.currentOperation = op.ID
	var payload operationPayload
	if len(op.Payload) > 0 {
		if err := json.Unmarshal(op.Payload, &payload); err != nil {
			return fmt.Errorf("%w: operation payload", domain.ErrInvalid)
		}
	}
	if op.Kind == "ui_screen" {
		return w.uiScreen(ctx, op)
	}
	if op.Kind == "access_request_notification" {
		return w.accessRequestNotification(ctx, op)
	}
	if op.Kind == "access_notification" {
		return w.accessNotification(ctx, payload)
	}
	if op.Kind == "delete_aux_message" {
		return w.deleteAuxMessage(ctx, payload)
	}

	t, err := w.store.Ticket(ctx, op.TicketID)
	if err != nil {
		return err
	}
	team, ok := w.cfg.Departments[t.Department]
	if !ok {
		return domain.ErrInvalid
	}
	switch op.Kind {
	case "create_topic":
		return w.createTopic(ctx, op, payload, t, team)
	case "refresh_card":
		return w.refresh(ctx, t)
	case "retire_queue_card":
		return w.retireQueueCard(ctx, payload, t)
	case "team_notification":
		return w.teamNotification(ctx, payload, team)
	case "notify_client":
		return w.clientNotification(ctx, t, payload.Text)
	case "archive_message":
		return w.archiveMessage(ctx, op, t)
	case "client_message", "team_message":
		return w.relayMessage(ctx, op, t)
	case "close_topic":
		return w.closeTopic(ctx, op, payload, t)
	case "reopen_topic":
		return w.reopenTopic(ctx, payload, t)
	case "delete_topic":
		return w.deleteTopic(ctx, payload, t)

	default:
		return domain.ErrInvalid
	}
}
func (w *Worker) retire(ctx context.Context, id, chat, topic int64) error {
	data, _ := json.Marshal(map[string]int64{"chat_id": chat, "topic_id": topic})
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		return tx.AddEvent(ctx, domain.Event{TicketID: id, Kind: "topic_retired", Data: data, CreatedAt: time.Now().UTC()})
	})
}
func (w *Worker) refresh(ctx context.Context, t domain.Ticket) error {
	team := w.cfg.Departments[t.Department]

	text, err := w.card(ctx, t)
	if err != nil {
		return err
	}
	if t.TopicID != 0 && t.TopicMessageID != 0 {
		buttons := engineerTopicButtons(t)
		if t.Status == domain.New {
			buttons = [][]Button{}
		}
		if err = w.rate(ctx, t.TopicChatID, "editMessageText"); err != nil {
			return err
		}
		if err = messageEditError(w.client.Call(ctx, "editMessageText", map[string]any{"chat_id": t.TopicChatID, "message_id": t.TopicMessageID, "text": text, "parse_mode": "HTML", "reply_markup": Markup{Buttons: buttons}}, nil)); err != nil {
			return err
		}
	}
	if t.TopicID != 0 && provisioningStatus(t.Status) {
		if err = w.ensureEngineerKeyboard(ctx, t); err != nil {
			return err
		}
	}
	if t.QueueMessageID != 0 {
		if err = w.rate(ctx, team.ChatID, "editMessageText"); err != nil {
			return err
		}
		err := w.client.Call(ctx, "editMessageText", map[string]any{"chat_id": team.ChatID, "message_id": t.QueueMessageID, "text": text, "parse_mode": "HTML", "reply_markup": Markup{cardButtons(t)}}, nil)
		var api *APIError
		if errors.As(err, &api) && strings.Contains(api.Description, "message is not modified") {
			return nil
		}
		return err
	}
	sent, err := w.sendEngineerCard(ctx, team.ChatID, team.QueueTopicID, text, cardButtons(t))
	if err != nil {
		return err
	}
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		now, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if now.TopicID != t.TopicID || now.Department != t.Department {
			return nil
		}
		now.QueueMessageID = sent.ID
		return tx.SaveTicket(ctx, &now)
	})
}
func (w *Worker) deliver(ctx context.Context, chat, topic int64, msg domain.Message, toTeam bool) (Message, error) {
	name := ""
	if toTeam {
		var err error
		name, err = displayName(ctx, w.store, msg.AuthorID, "Клиент")
		if err != nil {
			return Message{}, err
		}
	}
	return w.deliverNamed(ctx, chat, topic, msg, name)
}
func (w *Worker) deliverNamed(ctx context.Context, chat, topic int64, msg domain.Message, name string) (Message, error) {
	text := msg.Text
	if name != "" {
		text = name + ":\n" + text
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%d:%d:%s:%s:%s", msg.ID, msg.TelegramChatID, msg.TelegramMessageID, chat, msg.FileID, text, msg.MediaType)))
	key := fmt.Sprintf("delivery:%x:%d", digest, topic)
	limit := messageLimit
	if msg.FileID != "" {
		limit = captionLimit
	}
	if name != "" && utf16Length(text) > limit {
		if _, err := w.textParts(ctx, key+":author", chat, topic, name+":", nil); err != nil {
			return Message{}, err
		}
		text = msg.Text
	}
	if msg.FileID == "" {
		return w.textParts(ctx, key, chat, topic, text, nil)
	}
	method := map[string]string{"document": "sendDocument", "photo": "sendPhoto", "voice": "sendVoice", "video": "sendVideo", "audio": "sendAudio"}[msg.MediaType]
	if method == "" {
		return Message{}, domain.ErrInvalid
	}
	caption := text
	remainder := ""
	if utf16Length(text) > captionLimit {
		caption = ""
		remainder = text
	}
	sent, err := w.confirmedPart(ctx, key+":media", 0, func() (Message, error) {
		if err := w.rate(ctx, chat, method); err != nil {
			return Message{}, err
		}
		p := map[string]any{"chat_id": chat, msg.MediaType: msg.FileID, "caption": caption}
		if topic != 0 {
			p["message_thread_id"] = topic
		}
		var result Message
		err := w.client.Call(ctx, method, p, &result)
		return result, err
	})
	if err != nil {
		return sent, err
	}
	if remainder != "" {
		_, err = w.textParts(ctx, key+":text", chat, topic, remainder, nil)
	}
	return sent, err
}
func (w *Worker) archive(ctx context.Context, t domain.Ticket, msg domain.Message) error {
	if w.cfg.ArchiveDir == "" || msg.FileSize > 20<<20 {
		return nil
	}
	messages, err := w.store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, m := range messages {
		if m.TelegramChatID == msg.TelegramChatID && m.TelegramMessageID == msg.TelegramMessageID {
			msg = m
			break
		}
	}
	if msg.ID == 0 {
		return fmt.Errorf("message missing for archive")
	}
	if msg.ArchivePath != "" && msg.SHA256 != "" {
		return nil
	}
	file, err := w.client.GetFile(ctx, msg.FileID)
	if err != nil {
		return err
	}
	if file.Size > 20<<20 {
		return nil
	}
	r, err := w.client.Download(ctx, file.Path)
	if err != nil {
		return err
	}
	defer r.Close()
	dir := filepath.Join(w.cfg.ArchiveDir, fmt.Sprint(t.ID))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "archive-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(r, (20<<20)+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if n > 20<<20 {
		return nil
	}
	path := filepath.Join(dir, fmt.Sprintf("%d.bin", msg.ID))
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	return w.store.SaveArchive(ctx, msg.ID, path, hex.EncodeToString(hash.Sum(nil)))
}

func (w *Worker) ensureTopicCard(ctx context.Context, t domain.Ticket) (domain.Ticket, error) {
	if t.TopicMessageID != 0 {
		if err := w.ensureTransferContext(ctx, t); err != nil {
			return t, err
		}
		return t, w.ensureEngineerKeyboard(ctx, t)
	}
	if t.TopicID == 0 {
		return t, domain.ErrConflict
	}
	text, err := w.card(ctx, t)
	if err != nil {
		return t, err
	}
	sent, err := w.sendEngineerCard(ctx, t.TopicChatID, t.TopicID, text, engineerTopicButtons(t))
	if err != nil {
		return t, err
	}
	err = w.store.Transact(ctx, func(tx domain.Tx) error {
		current, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if current.TopicChatID != t.TopicChatID || current.TopicID != t.TopicID || current.Department != t.Department || current.RoutingGeneration != t.RoutingGeneration || current.AssigneeID != t.AssigneeID || !provisioningStatus(current.Status) {
			return domain.ErrConflict
		}
		current.TopicMessageID = sent.ID
		if e = tx.SaveTicket(ctx, &current); e != nil {
			return e
		}
		data, _ := json.Marshal(map[string]any{"source_message_id": 0, "telegram_chat_id": t.TopicChatID, "telegram_message_id": sent.ID, "telegram_topic_id": t.TopicID, "direction": "inbound", "initial_description": true})
		if e = tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: t.ClientID, Kind: "message_delivered", Data: data, CreatedAt: time.Now().UTC()}); e != nil {
			return e
		}
		t = current
		return nil
	})
	if err == nil {
		// The engineer keyboard is deliberately not installed here. Every caller runs
		// replayDeferred and finishProvision right after this, and refresh at the end
		// installs it. Doing it here as well sent the keyboard notice ahead of the
		// messages that were waiting for the topic, and paid a second events query.
		err = w.ensureTransferContext(ctx, t)
	}
	return t, err
}

func (w *Worker) notifyAdmins(ctx context.Context, text string) error {
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		for _, id := range w.cfg.AdminIDs {
			data, _ := json.Marshal(map[string]any{"user_id": id, "text": text})
			if err := tx.Enqueue(ctx, domain.Operation{Kind: "access_notification", Payload: data, AvailableAt: time.Now().UTC(), State: "pending"}); err != nil {
				return err
			}
		}
		return nil
	})
}
func (w *Worker) card(ctx context.Context, t domain.Ticket) (string, error) {
	name, err := clientContact(ctx, w.store, t.ClientID)
	if err != nil {
		return "", err
	}
	company, err := companyName(ctx, w.store, t.CompanyID)
	if err != nil {
		return "", err
	}
	assignee := "Не назначен"
	if t.AssigneeID != 0 {
		assignee, err = displayName(ctx, w.store, t.AssigneeID, "Специалист")
		if err != nil {
			return "", err
		}
	}
	text := engineerCardText(t, name, company, assignee)
	contextText, e := w.queueTransferContext(ctx, t)
	if e != nil {
		return "", e
	}
	text += contextText
	if t.Status == domain.New {
		messages, e := w.store.Messages(ctx, t.ID)
		if e != nil {
			return "", e
		}
		count := 0
		for _, m := range messages {
			if m.Direction == "inbound" {
				count++
			}
		}
		if count > 0 {
			text += fmt.Sprintf("\nДополнений от клиента: %d", count)
		}
	}
	return text, nil
}
func messageEditError(err error) error {
	var api *APIError
	if errors.As(err, &api) && strings.Contains(strings.ToLower(api.Description), "message is not modified") {
		return nil
	}
	return err
}
func (w *Worker) messageRoute(ctx context.Context, t domain.Ticket, m domain.Message) (int64, int64, error) {
	if t.TopicID != 0 && !t.RoutingPending && (m.RoutingDepartment == "" || m.RoutingDepartment == t.Department) {
		return t.TopicChatID, t.TopicID, nil
	}
	events, err := w.store.Events(ctx, t.ID)
	if err != nil {
		return 0, 0, err
	}
	var selected domain.Event
	var chat, topic int64
	for _, event := range events {
		var data struct {
			From       domain.Department `json:"from"`
			OldChatID  int64             `json:"old_chat_id"`
			OldTopicID int64             `json:"old_topic_id"`
			ChatID     int64             `json:"chat_id"`
			TopicID    int64             `json:"topic_id"`
		}
		if json.Unmarshal(event.Data, &data) != nil {
			continue
		}
		switch event.Kind {
		case "transferred", "reopened":
			if data.From != "" && m.RoutingDepartment != "" && data.From != m.RoutingDepartment {
				continue
			}
		case "completion_cleanup_requested":
			data.OldChatID = data.ChatID
			data.OldTopicID = data.TopicID
		default:
			continue
		}
		if data.OldChatID == 0 || data.OldTopicID == 0 {
			continue
		}
		if !m.CreatedAt.IsZero() && event.CreatedAt.Before(m.CreatedAt) {
			continue
		}
		if topic == 0 || event.CreatedAt.Before(selected.CreatedAt) {
			selected = event
			chat, topic = data.OldChatID, data.OldTopicID
		}
	}
	if topic != 0 {
		return chat, topic, nil
	}
	return 0, 0, fmt.Errorf("message destination topic pending")
}
func (w *Worker) replayDeferred(ctx context.Context, t domain.Ticket) error {
	messages, err := w.store.Messages(ctx, t.ID)
	if err != nil {
		return err
	}
	events, err := w.store.Events(ctx, t.ID)
	if err != nil {
		return err
	}
	delivered := map[int64]bool{}
	for _, event := range events {
		if event.Kind != "message_delivered" {
			continue
		}
		var d struct {
			SourceID int64 `json:"source_message_id"`
			ChatID   int64 `json:"telegram_chat_id"`
			TopicID  int64 `json:"telegram_topic_id"`
		}
		if json.Unmarshal(event.Data, &d) == nil && d.ChatID == t.TopicChatID && d.TopicID == t.TopicID {
			delivered[d.SourceID] = true
		}
	}
	for _, m := range messages {
		if !m.DeferredDelivery || m.Direction != "inbound" || delivered[m.ID] {
			continue
		}
		if m.FileID != "" {
			if err = w.archive(ctx, t, m); err != nil {
				return err
			}
		}
		name, e := displayName(ctx, w.store, m.AuthorID, "Клиент")
		if e != nil {
			return e
		}
		current, e := w.store.Ticket(ctx, t.ID)
		if e != nil {
			return e
		}
		if current.Department != t.Department || current.TopicID != t.TopicID || current.TopicChatID != t.TopicChatID || !provisioningStatus(current.Status) || current.RoutingGeneration != t.RoutingGeneration || current.AssigneeID != t.AssigneeID {
			return domain.ErrConflict
		}
		// RoutingPending forbids transfer; a concurrent close is queued behind
		// this operation. Send to the captured topic without holding a row lock
		// during rate waits/network I/O, then record the actual destination even
		// if the ticket was completed meanwhile. Cleanup waits for this operation.
		sent, e := w.deliverNamed(ctx, t.TopicChatID, t.TopicID, m, name)
		if e != nil {
			return e
		}
		data, _ := json.Marshal(map[string]any{"source_message_id": m.ID, "telegram_chat_id": t.TopicChatID, "telegram_topic_id": t.TopicID, "telegram_message_id": sent.ID, "direction": "inbound"})
		err = w.store.Transact(ctx, func(tx domain.Tx) error {
			return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: m.AuthorID, Kind: "message_delivered", Data: data, CreatedAt: time.Now().UTC()})
		})
		if err != nil {
			return err
		}

		delivered[m.ID] = true
	}
	return nil
}

func provisioningStatus(s domain.Status) bool { return s == domain.InProgress || s == domain.Waiting }
func (w *Worker) finishProvision(ctx context.Context, t domain.Ticket) (domain.Ticket, error) {
	err := w.store.Transact(ctx, func(tx domain.Tx) error {
		current, e := tx.GetTicket(ctx, t.ID)
		if e != nil {
			return e
		}
		if !provisioningStatus(current.Status) || current.Department != t.Department || current.TopicChatID != t.TopicChatID || current.TopicID != t.TopicID || current.AssigneeID != t.AssigneeID || current.RoutingGeneration != t.RoutingGeneration {
			return domain.ErrConflict
		}
		current.RoutingPending = false
		if e = tx.SaveTicket(ctx, &current); e != nil {
			return e
		}
		t = current
		return nil
	})
	return t, err
}

func auxiliaryDeleteError(err error) error {
	var api *APIError
	if errors.As(err, &api) && api.Code == 400 && strings.Contains(strings.ToLower(api.Description), "message to delete not found") {
		return nil
	}
	return err
}
func topicDeleteError(err error) error {
	var api *APIError
	if errors.As(err, &api) && api.Code == 400 {
		description := strings.ToLower(api.Description)
		for _, marker := range []string{"topic_id_invalid", "topic not found", "message thread not found", "message_thread_id_invalid"} {
			if strings.Contains(description, marker) {
				return nil
			}
		}
	}
	return err
}
func (w *Worker) notifyAuthor(ctx context.Context, op domain.Operation, cause error, permanent bool) error {
	var msg domain.Message
	if json.Unmarshal(op.Payload, &msg) != nil || msg.AuthorID == 0 {
		return nil
	}
	text := fmt.Sprintf("Ответ по заявке #%d пока не доставлен клиенту. Бот повторит отправку. Причина: %s", op.TicketID, cause)
	if permanent {
		text = fmt.Sprintf("Ответ по заявке #%d не доставлен клиенту. Администратору отправлено уведомление; требуется восстановить доставку. Причина: %s", op.TicketID, cause)
	}
	raw, _ := json.Marshal(map[string]any{"user_id": msg.AuthorID, "text": text, "notice_key": fmt.Sprintf("delivery:%d:%t", op.ID, permanent)})
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		return tx.Enqueue(ctx, domain.Operation{Kind: "access_notification", Payload: raw, AvailableAt: time.Now().UTC(), State: "pending"})
	})
}

// Remember confirmed delivery independently of closing: a close retry does not
// append another notice. Unknown network outcomes retain normal outbox semantics.
func (w *Worker) topicClosingNotice(ctx context.Context, op domain.Operation, chat, topic int64, text string) error {
	events, err := w.store.Events(ctx, op.TicketID)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Kind != "topic_closing_notice" {
			continue
		}
		var data struct {
			OperationID int64  `json:"operation_id"`
			ChatID      int64  `json:"chat_id"`
			TopicID     int64  `json:"topic_id"`
			Text        string `json:"text"`
		}
		if json.Unmarshal(event.Data, &data) == nil && data.OperationID == op.ID && data.ChatID == chat && data.TopicID == topic && data.Text == text {
			return nil
		}
	}
	sent, err := w.send(ctx, chat, topic, text, nil)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"operation_id": op.ID, "chat_id": chat, "topic_id": topic, "text": text, "message_id": sent.ID})
	return w.store.Transact(ctx, func(tx domain.Tx) error {
		return tx.AddEvent(ctx, domain.Event{TicketID: op.TicketID, Kind: "topic_closing_notice", Data: data, CreatedAt: time.Now().UTC()})
	})
}
