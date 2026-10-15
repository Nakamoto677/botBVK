package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/tickets"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestBurstDraftKeepsAllMessagesAndOnlyLatestPreview(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx := t.Context()
	client, capture := capturedClient()
	a := NewAdapter(client, store, tickets.New(store), Config{})
	if err := store.PutDraft(ctx, 1, draft{Generation: "initial", Step: "description", Department: domain.IT}); err != nil {
		t.Fatal(err)
	}
	messages := []Message{
		{ID: 8, Photo: []File{{ID: "photo-first"}}},
		{ID: 9, Photo: []File{{ID: "photo-second"}}},
		{ID: 10, Text: "Не запускается программа"},
		{ID: 11, Text: "После обновления"},
		{ID: 12, Photo: []File{{ID: "photo-one"}}, Caption: "Текст ошибки"},
		{ID: 13, Photo: []File{{ID: "photo-two"}}},
	}
	var last domain.StoredUpdate
	for i := range messages {
		messages[i].Chat = Chat{ID: 1, Type: "private"}
		messages[i].From = Person{ID: 1}
		raw, _ := json.Marshal(Update{Message: &messages[i]})
		last = domain.StoredUpdate{ID: int64(i + 1), Payload: raw}
		if _, err := store.SaveUpdate(ctx, last.ID, raw); err != nil {
			t.Fatal(err)
		}
		if err := a.processUpdate(ctx, last); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a restart after saving the draft but before acknowledging the update.
	a = NewAdapter(client, store, tickets.New(store), Config{})
	if err := a.processUpdate(ctx, last); err != nil {
		t.Fatal(err)
	}
	d, err := store.LoadDraft(ctx, 1)
	if err != nil || d.Text != "Не запускается программа\n\nПосле обновления\n\nТекст ошибки" || len(d.Attachments) != 4 || len(d.MessageIDs) != 6 {
		t.Fatalf("draft=%+v err=%v", d, err)
	}
	// A repeated menu tap resumes, rather than discards, a completed description.
	m := messages[0]
	m.ID = 20
	m.Text = "/new"
	if err := a.Handle(ctx, Update{Message: &m}); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.LoadDraft(ctx, 1)
	if err != nil || resumed.Generation != d.Generation || resumed.Text != d.Text {
		t.Fatalf("resume lost input: %+v %v", resumed, err)
	}
	before := len(capture.bodies)
	if !pause(ctx, 400*time.Millisecond) {
		t.Fatal(ctx.Err())
	}
	worker := NewWorker(client, store, Config{})
	worker.interval = 0
	for {
		op, err := store.NextOperation(ctx)
		if errors.Is(err, domain.ErrNotFound) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = worker.Handle(ctx, op); err != nil {
			t.Fatal(err)
		}
		if err = store.FinishOperation(ctx, op.ID); err != nil {
			t.Fatal(err)
		}
	}
	previews := 0
	for _, p := range capture.bodies[before:] {
		text, _ := p["text"].(string)
		if strings.HasPrefix(text, "Вложений принято:") {
			t.Fatal("outdated request for description", text)
		}
		if strings.HasPrefix(text, "Создать заявку?") {
			previews++
			if !strings.Contains(text, d.Text) || !strings.Contains(text, "Вложений принято: 4") {
				t.Fatal(text)
			}
		}
	}
	if previews != 1 {
		t.Fatalf("burst produced %d previews", previews)
	}
	ticket, err := tickets.New(store).CreateDraft(ctx, 1, d.Generation)
	if err != nil || ticket.Description != d.Text {
		t.Fatalf("submission: %+v %v", ticket, err)
	}
	attachments, err := store.Messages(ctx, ticket.ID)
	if err != nil || len(attachments) != 4 {
		t.Fatalf("attachments: %v %v", attachments, err)
	}
}

func TestDraftAcceptsUncaptionedAlbumBeforeDescription(t *testing.T) {
	a, _, _, m, _ := clientUIFixture(t)
	d := draft{Generation: "album", Step: "description", Department: domain.IT}
	m.Text = ""
	m.ID = 71
	m.Photo = []File{{ID: "one"}}
	if err := a.appendDraft(t.Context(), m, d); err != nil {
		t.Fatal(err)
	}
	d = a.drafts[1]
	if d.Step != "description" || len(d.Attachments) != 1 {
		t.Fatalf("%+v", d)
	}
	m.ID = 72
	m.Photo = []File{{ID: "two"}}
	if err := a.appendDraft(t.Context(), m, d); err != nil {
		t.Fatal(err)
	}
	d = a.drafts[1]
	m.ID = 73
	m.Photo = nil
	m.Text = "Ошибка на снимках"
	if err := a.appendDraft(t.Context(), m, d); err != nil {
		t.Fatal(err)
	}
	d = a.drafts[1]
	if d.Step != "confirm" || len(d.Attachments) != 2 || d.Text != m.Text {
		t.Fatalf("%+v", d)
	}
}

func TestInboxRetryRunsWhileLongPollIsWaiting(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var polls, sends atomic.Int32
	var processed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			if polls.Add(1) == 1 {
				fmt.Fprint(w, `{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":99},"chat":{"id":99,"type":"private"},"text":"/report"}}]}`)
				return
			}
			<-r.Context().Done()
			return
		}
		if strings.HasSuffix(r.URL.Path, "/sendDocument") {
			if sends.Add(1) == 1 {
				fmt.Fprint(w, `{"ok":false,"error_code":500,"description":"temporary"}`)
				return
			}
			processed.Store(true)
			cancel()
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":10}}`)
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	a := NewAdapter(client, store, tickets.New(store), Config{})
	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) || !processed.Load() || polls.Load() != 2 || sends.Load() != 2 {
		t.Fatalf("retry waited for long poll: %v processed=%v polls=%d sends=%d", err, processed.Load(), polls.Load(), sends.Load())
	}
}

func TestSlowDeliveryDoesNotBlockAnotherConversationAndKeepsFIFO(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := make(chan struct{})
	independent := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var mu sync.Mutex
	order := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Text string `json:"text"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		order = append(order, p.Text)
		mu.Unlock()
		switch p.Text {
		case "first":
			close(first)
			select {
			case <-release:
			case <-ctx.Done():
			}
		case "independent":
			close(independent)
		case "second":
			cancel()
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":20}}`)
	}))
	defer server.Close()
	for _, entry := range []struct {
		user int64
		text string
	}{{1, "first"}, {1, "second"}, {2, "independent"}} {
		raw, _ := json.Marshal(map[string]any{"user_id": entry.user, "chat_id": entry.user, "text": entry.text})
		if err := store.Transact(ctx, func(tx domain.Tx) error { return tx.Enqueue(ctx, domain.Operation{Kind: "ui_screen", Payload: raw}) }); err != nil {
			t.Fatal(err)
		}
	}
	client := NewClient("test")
	client.BaseURL = server.URL
	worker := NewWorker(client, store, Config{})
	worker.interval = 0
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("first not started")
	}
	select {
	case <-independent:
	case <-ctx.Done():
		t.Fatal("independent chat blocked")
	}
	mu.Lock()
	early := append([]string{}, order...)
	mu.Unlock()
	for _, text := range early {
		if text == "second" {
			t.Fatal("same conversation overtook its predecessor")
		}
	}
	releaseOnce.Do(func() { close(release) })
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[2] != "second" {
		t.Fatal(order)
	}
}

func TestClientNotificationDoesNotWaitBeforeFirstSend(t *testing.T) {
	client, _ := capturedClient()
	store := &workerStore{ticket: domain.Ticket{ID: 1, ClientID: 1, Status: domain.InProgress}}
	worker := NewWorker(client, store, Config{})
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	if err := worker.clientNotification(ctx, store.ticket, "Уведомление"); err != nil {
		t.Fatalf("first notification hit duplicate rate wait: %v", err)
	}
}

func TestRateWaitInGroupDoesNotReserveOtherChats(t *testing.T) {
	worker := NewWorker(nil, nil, Config{})
	worker.interval = 250 * time.Millisecond
	if err := worker.rate(t.Context(), -100); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.rate(t.Context(), -100) }()
	// Wait until the second group slot was reserved, without relying on a sleep.
	deadline := time.Now().Add(time.Second)
	for {
		worker.rates.mu.Lock()
		reserved := time.Until(worker.rates.lastChat[-100]) > 100*time.Millisecond
		worker.rates.mu.Unlock()
		if reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("group slot not reserved")
		}
		pause(t.Context(), time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	if err := worker.rate(ctx, 1); err != nil {
		t.Fatal("group blocked private chat", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type contentionStore struct {
	domain.Store
	retried, finished bool
}

func (s *contentionStore) User(context.Context, int64) (domain.User, error) {
	return domain.User{}, &pgconn.PgError{Code: "40P01"}
}
func (s *contentionStore) RetryUpdate(_ context.Context, _ int64, _ string, _ time.Duration, quarantine bool) error {
	s.retried = !quarantine
	return nil
}
func (s *contentionStore) FinishUpdate(context.Context, int64) error { s.finished = true; return nil }
func TestDatabaseContentionDoesNotConsumeMessage(t *testing.T) {
	s := &contentionStore{}
	a := NewAdapter(nil, s, nil, Config{})
	err := a.processUpdate(t.Context(), domain.StoredUpdate{ID: 1, Payload: json.RawMessage(`{"message":{"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"hello"}}`)})
	if err != nil || !s.retried || s.finished {
		t.Fatalf("err=%v retried=%v finished=%v", err, s.retried, s.finished)
	}
}

func TestReplayDoesNotLockTicketDuringSlowTelegramSend(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	svc := tickets.New(store)
	ticket, err := svc.Create(ctx, 1, domain.IT, "problem")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.AddMessage(ctx, 1, ticket.ID, domain.Message{Text: "detail", TelegramChatID: 1, TelegramMessageID: 1}, false); err != nil {
		t.Fatal(err)
	}
	if err = svc.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		ticket, err = tx.GetTicket(ctx, ticket.ID)
		if err != nil {
			return err
		}
		ticket.TopicID = 15
		ticket.TopicChatID = -100
		return tx.SaveTicket(ctx, &ticket)
	}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":100}}`)
	}))
	defer server.Close()
	defer once.Do(func() { close(release) })
	client := NewClient("test")
	client.BaseURL = server.URL
	worker := NewWorker(client, store, Config{})
	worker.interval = 0
	done := make(chan error, 1)
	go func() { done <- worker.replayDeferred(ctx, ticket) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("delivery did not start")
	}
	readCtx, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	err = store.Transact(readCtx, func(tx domain.Tx) error { _, e := tx.GetTicket(readCtx, ticket.ID); return e })
	stop()
	once.Do(func() { close(release) })
	sendErr := <-done
	if err != nil {
		t.Fatal("Telegram call held ticket lock", err)
	}
	if sendErr != nil {
		t.Fatal(sendErr)
	}
}

func TestAcceptedMessageRetryDoesNotShowBusinessErrorOrDuplicate(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx := t.Context()
	svc := tickets.New(store)
	ticket, err := svc.Create(ctx, 1, domain.IT, "problem")
	if err != nil {
		t.Fatal(err)
	}
	client, capture := capturedClient()
	a := NewAdapter(client, store, svc, Config{})
	m := &Message{ID: 53, From: Person{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Text: "detail"}
	for range 2 {
		if err = a.receive(ctx, m, ticket, false); err != nil {
			t.Fatal(err)
		}
	}
	messages, err := store.Messages(ctx, ticket.ID)
	if err != nil || len(messages) != 1 || len(capture.bodies) != 0 {
		t.Fatalf("retry: messages=%v err=%v UI=%v", messages, err, capture.bodies)
	}
}
