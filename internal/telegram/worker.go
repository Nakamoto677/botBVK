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
	"time"

	"bot-bvk/internal/domain"
)

type Worker struct {
	currentOperation int64

	client       *Client
	store        domain.Store
	cfg          Config
	interval     time.Duration
	lastRateChat int64
}

func NewWorker(c *Client, s domain.Store, cfg Config) *Worker {
	return &Worker{client: c, store: s, cfg: cfg, interval: 3100 * time.Millisecond}
}
func (w *Worker) Run(ctx context.Context) error {
	var ready <-chan struct{}
	if notifier, ok := w.store.(interface{ OperationReady() <-chan struct{} }); ok {
		ready = notifier.OperationReady()
	}
	for ctx.Err() == nil {
		op, err := w.store.NextOperation(ctx)
		if errors.Is(err, domain.ErrNotFound) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ready:
			case <-time.After(250 * time.Millisecond):
			}
			continue
		}
		if err != nil {
			return err
		}
		if err = w.Handle(ctx, op); err != nil {
			dead := false
			delay := time.Duration(1<<min(op.Attempts, 8)) * time.Second
			var api *APIError
			if errors.As(err, &api) {
				if api.RetryAfter > 0 {
					delay = time.Duration(api.RetryAfter) * time.Second
					if api.Code == 429 {
						w.client.limiter.coolDown(w.lastRateChat, delay)
					}
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
	}
	return ctx.Err()
}
func (w *Worker) rate(ctx context.Context, chat int64) error {
	w.lastRateChat = chat
	interval := w.interval
	if chat > 0 && interval > 0 {
		interval = time.Second
	}
	return w.client.limiter.wait(ctx, chat, interval)
}
func (w *Worker) send(ctx context.Context, chat, topic int64, text string, buttons [][]Button) (Message, error) {
	if w.currentOperation > 0 {
		hash := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", chat, topic, text)))
		var markup any
		if buttons != nil {
			markup = Markup{buttons}
		}
		return w.textParts(ctx, fmt.Sprintf("operation:%d:%x", w.currentOperation, hash), chat, topic, text, markup)
	}
	if err := w.rate(ctx, chat); err != nil {
		return Message{}, err
	}
	return w.client.Send(ctx, chat, topic, text, buttons)
}
func card(t domain.Ticket) string {
	return engineerCardText(t, "Клиент", "Компания", "Специалист")
}
func cardButtons(t domain.Ticket) [][]Button { return engineerQueueButtons(t) }
func (w *Worker) Handle(ctx context.Context, op domain.Operation) error {
	w.currentOperation = op.ID
	w.lastRateChat = 0
	var payload struct {
		Text              string            `json:"text"`
		UserID            int64             `json:"user_id"`
		TicketID          int64             `json:"ticket_id"`
		ChatID            int64             `json:"chat_id"`
		TopicID           int64             `json:"topic_id"`
		OldChatID         int64             `json:"old_chat_id"`
		OldTopicID        int64             `json:"old_topic_id"`
		Reason            string            `json:"reason"`
		Pause             bool              `json:"pause"`
		Retire            bool              `json:"retire"`
		MessageID         int64             `json:"message_id"`
		FromDepartment    domain.Department `json:"from_department"`
		ToDepartment      domain.Department `json:"to_department"`
		RoutingGeneration int64             `json:"routing_generation"`
		CompletionKey     string            `json:"completion_key"`
	}
	if len(op.Payload) > 0 {
		if err := json.Unmarshal(op.Payload, &payload); err != nil {
			return fmt.Errorf("%w: operation payload", domain.ErrInvalid)
		}
	}
	if op.Kind == "ui_screen" {
		return w.uiScreen(ctx, op)
	}
	if op.Kind == "access_request_notification" {
		var request struct {
			RequestID int64 `json:"request_id"`
			UserID    int64 `json:"user_id"`
			CompanyID int64 `json:"company_id"`
		}
		if err := json.Unmarshal(op.Payload, &request); err != nil {
			return domain.ErrInvalid
		}
		for _, id := range w.cfg.AdminIDs {
			if _, err := w.send(ctx, id, 0, fmt.Sprintf("Заявка на доступ #%d. Аккаунт %d, компания %d", request.RequestID, request.UserID, request.CompanyID), [][]Button{{{Text: "Одобрить", Data: fmt.Sprintf("approve:%d", request.RequestID)}, {Text: "Отклонить", Data: fmt.Sprintf("reject:%d", request.RequestID)}}}); err != nil {
				return err
			}
		}
		return nil
	}
	if op.Kind == "access_notification" {
		user, err := w.store.User(ctx, payload.UserID)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if user.Approved && user.Role == "client" {
			return w.clientAccessNotification(ctx, payload.UserID, payload.Text+"\nВыберите действие в меню.")
		}
		_, err = w.send(ctx, payload.UserID, 0, payload.Text, nil)
		return err
	}

	if op.Kind == "delete_aux_message" {
		if payload.ChatID == 0 || payload.MessageID <= 0 {
			return domain.ErrInvalid
		}
		if err := w.rate(ctx, payload.ChatID); err != nil {
			return err
		}
		return auxiliaryDeleteError(w.client.Call(ctx, "deleteMessage", map[string]any{"chat_id": payload.ChatID, "message_id": payload.MessageID}, nil))
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
		if payload.RoutingGeneration != 0 && payload.RoutingGeneration != t.RoutingGeneration {
			return nil
		}
		// Old queued operations must not create a topic before an engineer claims the ticket.
		if t.Status == domain.New {
			return w.refresh(ctx, t)
		}
		if !provisioningStatus(t.Status) {
			return nil
		}
		// A binding matching this department makes a retried operation safe after a crash.
		if t.TopicID != 0 && t.TopicChatID == team.ChatID {
			t, err = w.ensureTopicCard(ctx, t)
			if err != nil {
				return err
			}
			if payload.OldChatID != 0 && payload.OldTopicID != 0 {
				if err = w.client.CloseTopic(ctx, payload.OldChatID, payload.OldTopicID); err != nil {
					return err
				}
				if err = w.retire(ctx, t.ID, payload.OldChatID, payload.OldTopicID); err != nil {
					return err
				}
			}
			if err = w.replayDeferred(ctx, t); err != nil {
				return err
			}
			t, err = w.finishProvision(ctx, t)
			if err != nil {
				return err
			}
			return w.refresh(ctx, t)
		}
		if !t.Status.Active() {
			return nil
		}
		if err = w.rate(ctx, team.ChatID); err != nil {
			return err
		}
		name, err := displayName(ctx, w.store, t.ClientID, "Клиент")
		if err != nil {
			return err
		}
		company, err := companyName(ctx, w.store, t.CompanyID)
		if err != nil {
			return err
		}
		title := []rune(fmt.Sprintf("#%d · %s · %s", t.ID, name, company))
		if len(title) > 120 {
			title = title[:120]
		}
		topic, err := w.client.Topic(ctx, team.ChatID, string(title))
		if err != nil {
			return err
		}
		bound := false
		err = w.store.Transact(ctx, func(tx domain.Tx) error {
			current, e := tx.GetTicket(ctx, t.ID)
			if e != nil {
				return e
			}
			if current.Department != t.Department || !provisioningStatus(current.Status) || !current.RoutingPending || current.AssigneeID != t.AssigneeID || current.RoutingGeneration != t.RoutingGeneration || current.TopicID != t.TopicID || current.TopicChatID != t.TopicChatID {
				return domain.ErrConflict
			}
			current.TopicChatID = team.ChatID
			current.TopicID = topic
			current.TopicMessageID = 0
			if e = tx.SaveTicket(ctx, &current); e != nil {
				return e
			}
			t = current
			bound = true
			return nil
		})
		if err != nil {
			_ = w.client.CloseTopic(ctx, team.ChatID, topic)
			return err
		}
		if !bound {
			return domain.ErrConflict
		}
		t, err = w.ensureTopicCard(ctx, t)
		if err != nil {
			return err
		}
		if payload.Reason != "" {
			if _, err = w.send(ctx, t.TopicChatID, t.TopicID, "Причина передачи: "+payload.Reason, nil); err != nil {
				return err
			}
		}
		if payload.OldChatID != 0 && payload.OldTopicID != 0 {
			if err = w.client.CloseTopic(ctx, payload.OldChatID, payload.OldTopicID); err != nil {
				return err
			}
			if err = w.retire(ctx, t.ID, payload.OldChatID, payload.OldTopicID); err != nil {
				return err
			}
		}
		if err = w.replayDeferred(ctx, t); err != nil {
			return err
		}
		t, err = w.finishProvision(ctx, t)
		if err != nil {
			return err
		}
		return w.refresh(ctx, t)
	case "refresh_card":
		return w.refresh(ctx, t)
	case "retire_queue_card":
		chat := payload.ChatID
		if chat == 0 {
			chat = w.cfg.Departments[payload.FromDepartment].ChatID
		}
		if chat == 0 || payload.MessageID == 0 {
			return nil
		}
		text, e := w.card(ctx, t)
		if e != nil {
			return e
		}
		if err = w.rate(ctx, chat); err != nil {
			return err
		}
		return messageEditError(w.client.Call(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": payload.MessageID, "text": text + "\nПередано в отдел: " + departmentName(payload.ToDepartment), "parse_mode": "HTML", "reply_markup": Markup{Buttons: [][]Button{}}}, nil))
	case "team_notification":
		_, err = w.send(ctx, team.ChatID, team.QueueTopicID, payload.Text, nil)
		return err
	case "notify_client":
		return w.clientNotification(ctx, t, payload.Text)
	case "archive_message":
		var msg domain.Message
		if err = json.Unmarshal(op.Payload, &msg); err != nil {
			return domain.ErrInvalid
		}
		return w.archive(ctx, t, msg)
	case "client_message", "team_message":
		var msg domain.Message
		if err = json.Unmarshal(op.Payload, &msg); err != nil {
			return domain.ErrInvalid
		}
		if op.Kind == "client_message" {
			events, e := w.store.Events(ctx, t.ID)
			if e != nil {
				return e
			}
			for _, event := range events {
				if event.Kind != "message_delivered" {
					continue
				}
				var d struct {
					SourceID int64      `json:"source_message_id"`
					EditedAt *time.Time `json:"edited_at"`
				}
				if json.Unmarshal(event.Data, &d) == nil && d.SourceID == msg.ID && d.EditedAt != nil && (msg.EditedAt == nil || d.EditedAt.After(*msg.EditedAt)) {
					return nil
				}
			}
		}
		if msg.FileID != "" {
			if err = w.archive(ctx, t, msg); err != nil {
				return err
			}
		}
		chat, topic := t.ClientID, int64(0)
		if op.Kind == "team_message" {
			chat, topic, err = w.messageRoute(ctx, t, msg)
			if err != nil {
				return err
			}

			if topic == 0 {
				return fmt.Errorf("topic pending")
			}
		}
		sent, e := w.deliver(ctx, chat, topic, msg, op.Kind == "team_message")
		if e != nil {
			return e
		}
		data, _ := json.Marshal(map[string]any{"source_message_id": msg.ID, "telegram_chat_id": chat, "telegram_message_id": sent.ID, "telegram_topic_id": topic, "direction": msg.Direction, "edited_at": msg.EditedAt})
		return w.store.Transact(ctx, func(tx domain.Tx) error {
			if op.Kind == "client_message" {
				current, e := tx.GetTicket(ctx, t.ID)
				if e != nil {
					return e
				}
				if current.FirstResponseAt == nil {
					now := time.Now().UTC()
					current.FirstResponseAt = &now
					if e = tx.SaveTicket(ctx, &current); e != nil {
						return e
					}
					if e = tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: msg.AuthorID, Kind: "first_response", CreatedAt: now, Data: json.RawMessage(`{}`)}); e != nil {
						return e
					}
				}
			}
			return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, ActorID: msg.AuthorID, Kind: "message_delivered", Data: data, CreatedAt: time.Now().UTC()})
		})
	case "close_topic":
		if payload.ChatID == 0 {
			payload.ChatID = t.TopicChatID
		}
		if payload.TopicID == 0 {
			payload.TopicID = t.TopicID
		}
		if payload.TopicID == 0 {
			return nil
		}
		if payload.ChatID == t.TopicChatID && payload.TopicID == t.TopicID && t.Status.Active() && t.Status != domain.New {
			return nil
		}
		notice := payload.Text
		if payload.Retire && payload.ToDepartment.Valid() {
			notice = "Передано в отдел " + departmentName(payload.ToDepartment)
		}
		if notice != "" {
			if err = w.topicClosingNotice(ctx, op, payload.ChatID, payload.TopicID, notice); err != nil {
				if !topicMissing(err) {
					return err
				}
				if payload.Retire {
					return w.retire(ctx, t.ID, payload.ChatID, payload.TopicID)
				}
				return nil
			}
		}
		if err = w.client.CloseTopic(ctx, payload.ChatID, payload.TopicID); err != nil && !topicMissing(err) {
			return err
		}
		if payload.Retire {
			return w.retire(ctx, t.ID, payload.ChatID, payload.TopicID)
		}
		return nil
	case "reopen_topic":
		if payload.RoutingGeneration != 0 && payload.RoutingGeneration != t.RoutingGeneration {
			return nil
		}
		if t.TopicID == 0 || !provisioningStatus(t.Status) {
			return nil
		}
		if err = w.client.ReopenTopic(ctx, t.TopicChatID, t.TopicID); err != nil {
			return err
		}
		t, err = w.ensureTopicCard(ctx, t)
		if err != nil {
			return err
		}
		if err = w.replayDeferred(ctx, t); err != nil {
			return err
		}
		t, err = w.finishProvision(ctx, t)
		if err != nil {
			return err
		}
		return w.refresh(ctx, t)
	case "delete_topic":
		if payload.ChatID == 0 || payload.TopicID == 0 || payload.CompletionKey == "" {
			return domain.ErrInvalid
		}
		events, e := w.store.Events(ctx, t.ID)
		if e != nil {
			return e
		}
		intent := false
		for _, event := range events {
			if event.Kind != "completion_cleanup_requested" {
				continue
			}
			var saved struct {
				ChatID     int64  `json:"chat_id"`
				TopicID    int64  `json:"topic_id"`
				Generation int64  `json:"routing_generation"`
				Key        string `json:"completion_key"`
			}
			if json.Unmarshal(event.Data, &saved) == nil && saved.ChatID == payload.ChatID && saved.TopicID == payload.TopicID && saved.Generation == payload.RoutingGeneration && saved.Key == payload.CompletionKey {
				intent = true
			}
		}
		if !intent {
			return domain.ErrForbidden
		}
		data, _ := json.Marshal(map[string]any{"chat_id": payload.ChatID, "topic_id": payload.TopicID, "routing_generation": payload.RoutingGeneration, "key": payload.CompletionKey, "completion_key": payload.CompletionKey})
		return w.store.Transact(ctx, func(tx domain.Tx) error {
			now, e := tx.GetTicket(ctx, t.ID)
			if e != nil {
				return e
			}
			if now.TopicChatID == payload.ChatID && now.TopicID == payload.TopicID && now.Status.Active() {
				return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "cleanup_deferred", Data: data, CreatedAt: time.Now().UTC()})
			}
			if w.cfg.BeforeDelete == nil {
				return domain.ErrForbidden
			}
			if e = w.cfg.BeforeDelete(ctx, tx, now, payload.ChatID, payload.TopicID); e != nil {
				if errors.Is(e, domain.ErrConflict) {
					return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "cleanup_deferred", Data: data, CreatedAt: time.Now().UTC()})
				}
				return e
			}
			if e = topicDeleteError(w.client.Call(ctx, "deleteForumTopic", map[string]any{"chat_id": payload.ChatID, "message_thread_id": payload.TopicID}, nil)); e != nil {
				return e
			}
			if now.TopicChatID == payload.ChatID && now.TopicID == payload.TopicID && !now.Status.Active() {
				now.TopicChatID = 0
				now.TopicID = 0
				now.TopicMessageID = 0
				if e = tx.SaveTicket(ctx, &now); e != nil {
					return e
				}
			}
			return tx.AddEvent(ctx, domain.Event{TicketID: t.ID, Kind: "topic_deleted", Data: data, CreatedAt: time.Now().UTC()})
		})

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
		if err = w.rate(ctx, t.TopicChatID); err != nil {
			return err
		}
		if err = messageEditError(w.client.Call(ctx, "editMessageText", map[string]any{"chat_id": t.TopicChatID, "message_id": t.TopicMessageID, "text": text, "parse_mode": "HTML", "reply_markup": Markup{Buttons: buttons}}, nil)); err != nil && !(messageToEditMissing(err) && !t.Status.Active()) {
			return err
		}
	}
	if t.TopicID != 0 && provisioningStatus(t.Status) {
		if err = w.ensureEngineerKeyboard(ctx, t); err != nil {
			return err
		}
	}
	if t.QueueMessageID != 0 {
		if err = w.rate(ctx, team.ChatID); err != nil {
			return err
		}
		err := w.client.Call(ctx, "editMessageText", map[string]any{"chat_id": team.ChatID, "message_id": t.QueueMessageID, "text": text, "parse_mode": "HTML", "reply_markup": Markup{cardButtons(t)}}, nil)
		if !messageToEditMissing(err) {
			return messageEditError(err)
		}
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
		if now.TopicID != t.TopicID || now.Department != t.Department || now.QueueMessageID != t.QueueMessageID {
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
		if err := w.rate(ctx, chat); err != nil {
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
		err = w.ensureTransferContext(ctx, t)
		if err == nil {
			err = w.ensureEngineerKeyboard(ctx, t)
		}
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
func messageToEditMissing(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Code == 400 && strings.Contains(strings.ToLower(api.Description), "message to edit not found")
}
func topicMissing(err error) bool {
	var api *APIError
	if !errors.As(err, &api) || api.Code != 400 {
		return false
	}
	description := strings.ToLower(api.Description)
	for _, marker := range []string{"topic_id_invalid", "topic not found", "message thread not found", "message_thread_id_invalid"} {
		if strings.Contains(description, marker) {
			return true
		}
	}
	return false
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
		err = w.store.Transact(ctx, func(tx domain.Tx) error {
			current, e := tx.GetTicket(ctx, t.ID)
			if e != nil {
				return e
			}
			if current.Department != t.Department || current.TopicID != t.TopicID || current.TopicChatID != t.TopicChatID || !provisioningStatus(current.Status) || current.RoutingGeneration != t.RoutingGeneration || current.AssigneeID != t.AssigneeID {
				return domain.ErrConflict
			}
			sent, e := w.deliverNamed(ctx, t.TopicChatID, t.TopicID, m, name)
			if e != nil {
				return e
			}
			data, _ := json.Marshal(map[string]any{"source_message_id": m.ID, "telegram_chat_id": t.TopicChatID, "telegram_topic_id": t.TopicID, "telegram_message_id": sent.ID, "direction": "inbound"})
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
	if topicMissing(err) {
		return nil
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
