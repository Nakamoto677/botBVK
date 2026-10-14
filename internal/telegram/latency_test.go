package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/postgres"
	"bot-bvk/internal/tickets"
)

type latencyStore struct {
	*postgres.Store
	mu       sync.Mutex
	saved    map[int64]time.Time
	started  map[int64]time.Time
	finished map[int64]time.Time
	claimed  map[string]time.Time
	ready    map[string]time.Time
}

func (s *latencyStore) SaveUpdate(ctx context.Context, id int64, payload json.RawMessage) (bool, error) {
	inserted, err := s.Store.SaveUpdate(ctx, id, payload)
	if err == nil && inserted {
		s.mu.Lock()
		s.saved[id] = time.Now()
		s.mu.Unlock()
	}
	return inserted, err
}

func (s *latencyStore) PendingUpdates(ctx context.Context, limit int) ([]domain.StoredUpdate, error) {
	updates, err := s.Store.PendingUpdates(ctx, limit)
	if err == nil {
		s.mu.Lock()
		for _, u := range updates {
			if s.started[u.ID].IsZero() {
				s.started[u.ID] = time.Now()
			}
		}
		s.mu.Unlock()
	}
	return updates, err
}

func (s *latencyStore) FinishUpdate(ctx context.Context, id int64) error {
	err := s.Store.FinishUpdate(ctx, id)
	if err == nil {
		s.mu.Lock()
		s.finished[id] = time.Now()
		s.mu.Unlock()
	}
	return err
}

func (s *latencyStore) NextOperation(ctx context.Context) (domain.Operation, error) {
	op, err := s.Store.NextOperation(ctx)
	if err == nil {
		s.mu.Lock()
		s.claimed[op.Kind] = time.Now()
		s.ready[op.Kind] = op.AvailableAt
		s.mu.Unlock()
	}
	return op, err
}

type latencySend struct {
	method string
	start  time.Time
	end    time.Time
}

// This uses a real isolated PostgreSQL schema and a local Bot API. A one-second
// fake long-poll timeout keeps the pre-fix regression measurable in tests.
func TestLatencyPipeline(t *testing.T) {
	base := reliabilityDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &latencyStore{Store: base, saved: map[int64]time.Time{}, started: map[int64]time.Time{}, finished: map[int64]time.Time{}, claimed: map[string]time.Time{}, ready: map[string]time.Time{}}
	companies, err := store.Companies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ticket := domain.Ticket{ClientID: 1, CompanyID: companies[0].ID, Department: domain.IT, Status: domain.InProgress, Description: "Performance fixture", TopicChatID: -100, TopicID: 10}
	if err := store.Transact(ctx, func(tx domain.Tx) error { return tx.SaveTicket(ctx, &ticket) }); err != nil {
		t.Fatal(err)
	}

	updates := make(chan Update, 4)
	sends := make(chan latencySend, 8)
	polling := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[len("/bottest/"):]
		if method == "getUpdates" {
			select {
			case polling <- struct{}{}:
			default:
			}
			select {
			case u := <-updates:
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []Update{u}})
			case <-time.After(time.Second):
				_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
			case <-r.Context().Done():
			}
			return
		}
		start := time.Now()
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":123}}`))
		if method == "sendMessage" || method == "sendPhoto" {
			sends <- latencySend{method: method, start: start, end: time.Now()}
		}
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	cfg := Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}}
	adapter := NewAdapter(client, store, tickets.New(store), cfg)
	worker := NewWorker(client, store, cfg)
	errs := make(chan error, 2)
	go func() { errs <- adapter.Run(ctx) }()
	go func() { errs <- worker.Run(ctx) }()
	select {
	case <-polling:
	case <-time.After(3 * time.Second):
		t.Fatal("poller did not start")
	}

	cases := []struct {
		name   string
		kind   string
		method string
		make   func(int64) Update
	}{
		{"text", "team_message", "sendMessage", func(id int64) Update {
			return Update{ID: id, Message: &Message{ID: id, From: Person{ID: 1, FirstName: "Client"}, Chat: Chat{ID: 1, Type: "private"}, Text: "text"}}
		}},
		{"photo", "team_message", "sendPhoto", func(id int64) Update {
			return Update{ID: id, Message: &Message{ID: id, From: Person{ID: 1, FirstName: "Client"}, Chat: Chat{ID: 1, Type: "private"}, Caption: "photo", Photo: []File{{ID: "photo-id", Size: 100}}}}
		}},
		{"button", "ui_screen", "sendMessage", func(id int64) Update {
			return Update{ID: id, Callback: &Callback{ID: fmt.Sprint(id), From: Person{ID: 1}, Data: fmt.Sprintf("ticket:%d", ticket.ID), Message: &Message{ID: 55, Chat: Chat{ID: 1, Type: "private"}}}}
		}},
		{"button_again", "ui_screen", "sendMessage", func(id int64) Update {
			return Update{ID: id, Callback: &Callback{ID: fmt.Sprint(id), From: Person{ID: 1}, Data: fmt.Sprintf("ticket:%d", ticket.ID), Message: &Message{ID: 55, Chat: Chat{ID: 1, Type: "private"}}}}
		}},
	}
	for i, tc := range cases {
		id := int64(i + 1)
		arrived := time.Now()
		updates <- tc.make(id)
		var send latencySend
		select {
		case send = <-sends:
		case err := <-errs:
			t.Fatalf("%s worker stopped: %v", tc.name, err)
		case <-time.After(8 * time.Second):
			t.Fatalf("%s send timed out", tc.name)
		}
		if send.method != tc.method {
			t.Fatalf("%s delivered via %s", tc.name, send.method)
		}
		deadline := time.Now().Add(2 * time.Second)
		var saved, started, finished, claimed, ready time.Time
		for time.Now().Before(deadline) {
			store.mu.Lock()
			saved, started, finished = store.saved[id], store.started[id], store.finished[id]
			claimed, ready = store.claimed[tc.kind], store.ready[tc.kind]
			store.mu.Unlock()
			if !finished.IsZero() && !claimed.IsZero() {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if saved.IsZero() || started.IsZero() || finished.IsZero() || claimed.IsZero() {
			t.Fatalf("%s missing stage timestamps", tc.name)
		}
		t.Logf("%s receive=%s inbox_wait=%s processing=%s outbox_wait=%s worker_to_api=%s bot_api=%s total=%s", tc.name, saved.Sub(arrived).Round(time.Millisecond), started.Sub(saved).Round(time.Millisecond), finished.Sub(started).Round(time.Millisecond), claimed.Sub(ready).Round(time.Millisecond), send.start.Sub(claimed).Round(time.Millisecond), send.end.Sub(send.start).Round(time.Millisecond), send.end.Sub(arrived).Round(time.Millisecond))
	}
	cancel()
	for range 2 {
		select {
		case <-errs:
		case <-time.After(2 * time.Second):
			t.Fatal("pipeline did not stop")
		}
	}
}
