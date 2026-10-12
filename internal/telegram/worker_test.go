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
	ticket   domain.Ticket
	current  domain.Ticket
	events   []domain.Event
	messages []domain.Message
}

func (s *workerStore) User(_ context.Context, id int64) (domain.User, error) {
	return domain.User{ID: id, Name: "Иван Клиент", Approved: true, Role: "client"}, nil
}
func (s *workerStore) Companies(context.Context) ([]domain.Company, error) {
	return []domain.Company{{ID: 10, Name: "Компания Тест"}}, nil
}
func (s *workerStore) Messages(context.Context, int64) ([]domain.Message, error) {
	return s.messages, nil
}
func (s *workerStore) Events(context.Context, int64) ([]domain.Event, error) { return s.events, nil }
func (s *workerStore) Ticket(context.Context, int64) (domain.Ticket, error) {
	if s.current.ID != 0 {
		return s.current, nil
	}
	return s.ticket, nil
}
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

func TestDeleteRequiresCurrentBackupProof(t *testing.T) {
	ticket := domain.Ticket{ID: 1, Department: domain.IT, Status: domain.Closed, TopicChatID: -100, TopicID: 10}
	store := &workerStore{ticket: ticket, current: ticket}
	called := false
	worker := NewWorker(nil, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}, BeforeDelete: func(_ context.Context, _ domain.Tx, _ domain.Ticket, chat, topic int64) error {
		called = true
		if chat != -100 || topic != 10 {
			t.Fatal("wrong binding")
		}
		return domain.ErrConflict
	}})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "delete_topic", TicketID: 1, Payload: json.RawMessage(`{"chat_id":-100,"topic_id":10}`)}); err != nil {
		t.Fatal(err)
	}
	if !called || len(store.events) != 1 || store.events[0].Kind != "cleanup_deferred" {
		t.Fatalf("missing deferral: %#v", store.events)
	}
	var data map[string]any
	_ = json.Unmarshal(store.events[0].Data, &data)
	if data["key"] != "cleanup:-100:10" {
		t.Fatal(data)
	}
}

func TestPollHealthStartsUnknown(t *testing.T) {
	a := NewAdapter(nil, nil, nil, Config{})
	if !a.LastPoll().IsZero() {
		t.Fatal("unstarted poll marked healthy")
	}
}

func TestPendingTransferPreservesPriorMessagesAndDefersNewOnes(t *testing.T) {
	for _, department := range []domain.Department{domain.OneC, domain.IT} {
		t.Run(string(department), func(t *testing.T) {
			called := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body["chat_id"] != float64(-100) || body["message_thread_id"] != float64(10) {
					t.Fatal("prior message routed to wrong group", body)
				}
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":80}}`))
			}))
			defer server.Close()
			client := NewClient("test")
			client.BaseURL = server.URL
			ticket := domain.Ticket{ID: 1, Department: domain.IT, RoutingPending: true, TopicChatID: -100, TopicID: 10}
			store := &workerStore{ticket: ticket, current: ticket, events: []domain.Event{{Kind: "transferred", Data: json.RawMessage(`{"from":"1c","old_chat_id":-100,"old_topic_id":10}`)}}}
			worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -200, QueueTopicID: 1}}})
			payload, _ := json.Marshal(domain.Message{Text: "message", Direction: "inbound", RoutingDepartment: department})
			err := worker.Handle(context.Background(), domain.Operation{Kind: "team_message", TicketID: 1, Payload: payload})
			if department == domain.OneC {
				if err != nil || !called {
					t.Fatalf("prior message wasn't delivered: %v", err)
				}
			} else if err == nil || called {
				t.Fatal("new message leaked to previous department")
			}
		})
	}
}

func TestInitialTopicCardSupportsPublicReplyAndRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	ticket := domain.Ticket{ID: 7, ClientID: 1, Department: domain.IT, TopicChatID: -100, TopicID: 10, Description: "Initial description", Status: domain.InProgress, AssigneeID: 2}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{})
	bound, err := worker.ensureTopicCard(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if bound.TopicMessageID != 99 || len(store.events) != 1 {
		t.Fatal("initial card not persisted", bound, store.events)
	}
	if _, err = worker.ensureTopicCard(context.Background(), bound); err != nil || calls != 1 {
		t.Fatalf("retry duplicated card: %v calls %d", err, calls)
	}
	adapterStore := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: bound, events: store.events}
	service := &adapterService{}
	adapter := NewAdapter(nil, adapterStore, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err = adapter.Handle(context.Background(), Update{Message: &Message{ID: 100, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "/reply Ответ на исходное обращение", Reply: &Message{ID: 99}}}); err != nil {
		t.Fatal(err)
	}
	if len(service.public) != 1 || !service.public[0] {
		t.Fatal("cannot reply to initial card")
	}
}
