package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

type captureTransport struct {
	calls  []string
	bodies []map[string]any
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	c.calls = append(c.calls, r.URL.Path)
	c.bodies = append(c.bodies, body)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":77,"message_thread_id":10}}`))}, nil
}
func capturedClient() (*Client, *captureTransport) {
	transport := &captureTransport{}
	client := NewClient("test")
	client.HTTP = &http.Client{Transport: transport}
	return client, transport
}

func TestNewTicketOnlyAppearsInQueueEvenWithLegacyCreateOperation(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{ID: 7, ClientID: 12345, CompanyID: 10, Department: domain.IT, Status: domain.New, Description: "Ошибка"}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "create_topic", TicketID: 7}); err != nil {
		t.Fatal(err)
	}
	if len(capture.calls) != 1 || !strings.HasSuffix(capture.calls[0], "sendMessage") {
		t.Fatal(capture.calls)
	}
	body := capture.bodies[0]
	if body["chat_id"] != float64(-100) || body["message_thread_id"] != float64(1) {
		t.Fatal(body)
	}
	text := body["text"].(string)
	if !strings.Contains(text, "Иван Клиент") || strings.Contains(text, "12345") {
		t.Fatal(text)
	}
	if store.current.TopicID != 0 || store.current.QueueMessageID != 77 {
		t.Fatal(store.current)
	}
	markup, _ := json.Marshal(body["reply_markup"])
	if !strings.Contains(string(markup), "claim:7") || strings.Contains(string(markup), "close:7") || strings.Contains(string(markup), "emergency") {
		t.Fatal(string(markup))
	}
}
func TestDeferredMessagesReplayOncePerCurrentTopic(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{ID: 7, ClientID: 1, Department: domain.IT, TopicChatID: -100, TopicID: 10, Status: domain.InProgress}
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{{ID: 8, AuthorID: 1, Text: "Дополнение", Direction: "inbound", DeferredDelivery: true}, {ID: 9, Text: "Внутренняя заметка", Direction: "internal", DeferredDelivery: true}}}
	worker := NewWorker(client, store, Config{})
	for i := 0; i < 2; i++ {
		if err := worker.replayDeferred(context.Background(), ticket); err != nil {
			t.Fatal(err)
		}
	}
	if len(capture.calls) != 1 || len(store.events) != 1 {
		t.Fatal(capture.calls, store.events)
	}
	if capture.bodies[0]["text"] != "Иван Клиент:\nДополнение" {
		t.Fatal(capture.bodies)
	}
	// Delivery to a previous department must not suppress delivery to the new topic.
	ticket.TopicChatID = -200
	ticket.TopicID = 20
	store.current = ticket
	store.ticket = ticket
	if err := worker.replayDeferred(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(capture.calls) != 2 {
		t.Fatal(capture.calls)
	}
}

type promptService struct {
	Service
	closed      int64
	transferred int64
	department  domain.Department
	reason      string
}

func (s *promptService) Close(_ context.Context, _ int64, id int64, reason string) error {
	s.closed = id
	s.reason = reason
	return nil
}
func (s *promptService) Transfer(_ context.Context, _ int64, id int64, d domain.Department, reason string) error {
	s.transferred = id
	s.department = d
	s.reason = reason
	return nil
}
func TestEngineerButtonsCollectResolutionAndTransferReasonInTheirOwnScope(t *testing.T) {
	for _, action := range []string{"close", "transfer"} {
		t.Run(action, func(t *testing.T) {
			client, _ := capturedClient()
			store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, AssigneeID: 2, Status: domain.InProgress, TopicChatID: -100, TopicID: 10, QueueMessageID: 77}}
			service := &promptService{}
			adapter := NewAdapter(client, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
			message := &Message{ID: 77, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 1, From: Person{ID: 2}}
			callback := &Callback{ID: "cb", From: Person{ID: 2}, Message: message, Data: action + ":7"}
			if err := adapter.callback(context.Background(), callback); err != nil {
				t.Fatal(err)
			}
			other := *message
			other.ThreadID = 10
			other.Text = "Не причина"
			if consumed, err := adapter.consumePrompt(context.Background(), &other, store.user); consumed || err != nil {
				t.Fatal("prompt escaped scope", err)
			}
			if action == "transfer" {
				callback.Data = "transferdept:7:1c"
				if err := adapter.callback(context.Background(), callback); err != nil {
					t.Fatal(err)
				}
			}
			message.Text = "Исправлен доступ"
			if consumed, err := adapter.consumePrompt(context.Background(), message, store.user); !consumed || err != nil {
				t.Fatal(err)
			}
			if service.reason != message.Text || (action == "close" && service.closed != 7) || (action == "transfer" && (service.transferred != 7 || service.department != domain.OneC)) {
				t.Fatal(service)
			}
			if len(adapter.prompts) != 0 {
				t.Fatal("prompt retained")
			}
		})
	}
}
func TestApprovedClientReceivesPersistentSimpleMenu(t *testing.T) {
	client, capture := capturedClient()
	store := &adapterStore{user: domain.User{ID: 1, Role: "client", Approved: true}}
	adapter := NewAdapter(client, store, nil, Config{})
	if err := adapter.Handle(context.Background(), Update{Message: &Message{From: Person{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Text: "/start"}}); err != nil {
		t.Fatal(err)
	}
	if len(capture.bodies) != 1 {
		t.Fatal(capture.bodies)
	}
	markup, _ := json.Marshal(capture.bodies[0]["reply_markup"])
	for _, label := range []string{"Создать заявку", "Помощь"} {
		if !strings.Contains(string(markup), label) {
			t.Fatal(string(markup))
		}
	}
}
