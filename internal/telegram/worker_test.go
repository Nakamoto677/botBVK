package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

type workerStore struct {
	domain.Store
	ticket   domain.Ticket
	current  domain.Ticket
	events   []domain.Event
	messages []domain.Message
	parts    map[string]int64
}

// Confirmed send parts make the retry fence active in tests. Without them the
// worker falls back to a plain send and a replay is silently duplicated.
func (s *workerStore) SendPart(_ context.Context, key string, part int) (int64, error) {
	if s.parts == nil {
		return 0, domain.ErrNotFound
	}
	id, ok := s.parts[fmt.Sprintf("%s:%d", key, part)]
	if !ok {
		return 0, domain.ErrNotFound
	}
	return id, nil
}
func (s *workerStore) SaveSendPart(_ context.Context, key string, part int, id int64) error {
	if s.parts == nil {
		s.parts = map[string]int64{}
	}
	s.parts[fmt.Sprintf("%s:%d", key, part)] = id
	return nil
}

// FinishOperation and RetryOperation let a test call runOperation, which is the real
// wrapper around Handle that records per-kind stage timings.
func (s *workerStore) FinishOperation(context.Context, int64) error { return nil }
func (s *workerStore) RetryOperation(context.Context, int64, string, time.Duration, bool) error {
	return nil
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
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	worker.interval = 0
	bound, err := worker.ensureTopicCard(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	// One card delivery is recorded here. The engineer keyboard notice is installed
	// by refresh after provisioning finishes, so it is not part of this step.
	if bound.TopicMessageID != 99 || len(store.events) != 1 || store.events[0].Kind != "message_delivered" {
		t.Fatal("initial card not persisted", bound, store.events)
	}
	if _, err = worker.ensureTopicCard(context.Background(), bound); err != nil || calls != 2 {
		t.Fatalf("retry duplicated card: %v calls %d", err, calls)
	}
	adapterStore := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: bound, events: store.events}
	service := &adapterService{}
	adapter := NewAdapter(nil, adapterStore, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err = adapter.Handle(context.Background(), Update{Message: &Message{ID: 100, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "Ответ на исходное обращение", Reply: &Message{ID: 99}}}); err != nil {
		t.Fatal(err)
	}
	if len(service.public) != 1 || !service.public[0] {
		t.Fatal("cannot reply to initial card")
	}
}

// The engineer card is HTML and used to be sent outside the recorded-parts fence,
// so a crash between the send and the ticket update duplicated it. It now travels
// the shared send path: HTML must survive and a retry must not send again.
func TestEngineerCardIsHtmlAndRetryDoesNotResend(t *testing.T) {
	sends := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		sends = append(sends, p)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	ticket := domain.Ticket{ID: 7, ClientID: 1, Department: domain.IT, TopicChatID: -100, TopicID: 10, Description: "Initial description", Status: domain.InProgress, AssigneeID: 2}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	op := domain.Operation{ID: 5, Kind: "create_topic", TicketID: ticket.ID, Payload: json.RawMessage(`{"chat_id":-100,"topic_id":10,"routing_generation":0}`)}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	if len(sends) == 0 {
		t.Fatal("card was not sent")
	}
	cardSends := 0
	for _, p := range sends {
		text, _ := p["text"].(string)
		if !strings.Contains(text, "<b>Заявка #7") {
			continue
		}
		cardSends++
		if p["parse_mode"] != "HTML" {
			t.Fatalf("card lost its HTML parse mode: %v", p["parse_mode"])
		}
		if p["chat_id"] != float64(-100) {
			t.Fatalf("card targeted the wrong chat: %v", p)
		}
		// The queue card goes to the department queue topic, the bound card to the
		// ticket topic. Any other thread would deliver the card to the wrong place.
		if thread, ok := p["message_thread_id"]; ok {
			if thread != float64(10) && thread != float64(1) {
				t.Fatalf("card targeted an unrelated topic: %v", thread)
			}
		}
	}
	if cardSends == 0 {
		t.Fatal("no HTML card was sent")
	}
	// Replaying the same operation must resume from the recorded part instead of
	// sending the ticket card again. Only ticket-topic sends are compared: the
	// queue card and the keyboard notice are distinct messages with their own
	// recorded keys, so they may legitimately still be delivered once.
	topicSends := 0
	for _, p := range sends {
		if p["message_thread_id"] == float64(10) {
			topicSends++
		}
	}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	after := 0
	for _, p := range sends {
		if p["message_thread_id"] == float64(10) {
			after++
		}
	}
	if after != topicSends {
		t.Fatalf("retry resent the bound ticket card: %d -> %d", topicSends, after)
	}
}

func TestCompletionDeleteUsesExactOldBindingAndAbsentTopicIsSuccess(t *testing.T) {
	ctx := context.Background()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if p["message_thread_id"] != float64(10) {
			t.Errorf("new topic targeted %v", p)
		}
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"TOPIC_ID_INVALID"}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	current := domain.Ticket{ID: 1, Department: domain.IT, Status: domain.InProgress, TopicChatID: -100, TopicID: 20, RoutingGeneration: 2}
	payload := json.RawMessage(`{"chat_id":-100,"topic_id":10,"routing_generation":1,"completion_key":"old"}`)
	store := &workerStore{current: current, events: []domain.Event{{Kind: "completion_cleanup_requested", Data: payload}}}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100}}, BeforeDelete: func(context.Context, domain.Tx, domain.Ticket, int64, int64) error { return nil }})
	op := domain.Operation{TicketID: 1, Kind: "delete_topic", Payload: payload}
	if err := worker.Handle(ctx, op); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || store.current.TopicID != 20 || store.events[len(store.events)-1].Kind != "topic_deleted" {
		t.Fatal("old cleanup affected reopened ticket")
	}
	op.Payload = json.RawMessage(`{"chat_id":-100,"topic_id":20,"routing_generation":2,"completion_key":"old"}`)
	if err := worker.Handle(ctx, op); err != domain.ErrForbidden {
		t.Fatalf("unmatched binding allowed: %v", err)
	}
	if calls != 1 {
		t.Fatal("invalid intent reached Telegram")
	}
}
func TestAuxiliaryDeletionHasNoTicketDependencyAndMissingMessageIsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"error_code":400,"description":"message to delete not found"}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	worker := NewWorker(client, nil, Config{})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "delete_aux_message", Payload: json.RawMessage(`{"chat_id":-100,"message_id":9}`)}); err != nil {
		t.Fatal(err)
	}
}

func (s *workerStore) ActiveTicket(_ context.Context, id int64) (domain.Ticket, error) {
	if s.current.ClientID == id && s.current.Status.Active() {
		return s.current, nil
	}
	return domain.Ticket{}, domain.ErrNotFound
}

func TestClosingNoticeTargetsOldTopicAndSurvivesCloseRetry(t *testing.T) {
	for _, tc := range []struct {
		payload, want string
		status        domain.Status
	}{
		{`{"chat_id":-100,"topic_id":10,"retire":true,"to_department":"1c"}`, "Передано в отдел 1С", domain.New},
		{`{"chat_id":-100,"topic_id":10,"text":"Тикет закрыт инженером"}`, "Тикет закрыт инженером", domain.Closed},
	} {
		t.Run(tc.want, func(t *testing.T) {
			calls := []string{}
			sends := 0
			closes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var p map[string]any
				json.NewDecoder(r.Body).Decode(&p)
				calls = append(calls, r.URL.Path)
				if p["chat_id"] != float64(-100) || p["message_thread_id"] != float64(10) {
					t.Error("wrong topic", p)
				}
				if r.URL.Path == "/bottest/sendMessage" {
					sends++
					if p["text"] != tc.want {
						t.Error(p)
					}
				} else {
					closes++
					if closes == 1 {
						w.Write([]byte(`{"ok":false,"error_code":500,"description":"retry"}`))
						return
					}
				}
				w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
			}))
			defer server.Close()
			client := NewClient("test")
			client.BaseURL = server.URL
			store := &workerStore{current: domain.Ticket{ID: 7, Department: domain.OneC, Status: tc.status}}
			worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.OneC: {ChatID: -200}}})
			worker.interval = 0
			op := domain.Operation{ID: 42, TicketID: 7, Kind: "close_topic", Payload: json.RawMessage(tc.payload)}
			if err := worker.Handle(context.Background(), op); err == nil {
				t.Fatal("close failure hidden")
			}
			if err := worker.Handle(context.Background(), op); err != nil {
				t.Fatal(err)
			}
			if sends != 1 || closes != 2 || calls[0] != "/bottest/sendMessage" {
				t.Fatal(calls)
			}
		})
	}
}
