package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"bot-bvk/internal/domain"
)

type workerStore struct {
	domain.Store
	ticket  domain.Ticket
	current domain.Ticket
	events  []domain.Event
}

func (s *workerStore) Ticket(context.Context, int64) (domain.Ticket, error) { return s.ticket, nil }
func (s *workerStore) Transact(ctx context.Context, f func(domain.Tx) error) error {
	return f(&workerTx{store: s})
}

type workerTx struct {
	domain.Tx
	store *workerStore
}

func (tx *workerTx) GetTicket(context.Context, int64) (domain.Ticket, error) {
	return tx.store.current, nil
}
func (tx *workerTx) SaveTicket(_ context.Context, t *domain.Ticket) error {
	tx.store.current = *t
	return nil
}
func (tx *workerTx) AddEvent(_ context.Context, e domain.Event) error {
	tx.store.events = append(tx.store.events, e)
	return nil
}
func TestFirstResponseRequiresConfirmedDelivery(t *testing.T) {
	for _, ok := range []bool{true, false} {
		t.Run(map[bool]string{true: "confirmed", false: "failed"}[ok], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ok {
					_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":51}}`))
				} else {
					_, _ = w.Write([]byte(`{"ok":false,"error_code":403,"description":"blocked"}`))
				}
			}))
			defer server.Close()
			client := NewClient("test")
			client.BaseURL = server.URL
			ticket := domain.Ticket{ID: 1, Department: domain.IT, ClientID: 12}
			store := &workerStore{ticket: ticket, current: ticket}
			worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
			data, _ := json.Marshal(domain.Message{AuthorID: 2, Text: "Resolved", Direction: "outbound"})
			err := worker.Handle(context.Background(), domain.Operation{Kind: "client_message", TicketID: 1, Payload: data})
			if (err == nil) != ok {
				t.Fatal(err)
			}
			if (store.current.FirstResponseAt != nil) != ok {
				t.Fatalf("first response marked on failed delivery: %#v", store.current.FirstResponseAt)
			}
			if ok && len(store.events) != 2 {
				t.Fatalf("events missing: %#v", store.events)
			}
		})
	}
}
func TestDeleteRechecksReopenUnderLock(t *testing.T) {
	ticket := domain.Ticket{ID: 1, Department: domain.IT, Status: domain.Closed, TopicChatID: -100, TopicID: 10}
	current := ticket
	current.Status = domain.InProgress
	store := &workerStore{ticket: ticket, current: current}
	worker := NewWorker(nil, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	data := json.RawMessage(`{"chat_id":-100,"topic_id":10}`)
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "delete_topic", TicketID: 1, Payload: data}); err != nil {
		t.Fatal(err)
	}
	if len(store.events) != 0 || store.current.TopicID != 10 {
		t.Fatal("reopened topic was deleted")
	}
}
