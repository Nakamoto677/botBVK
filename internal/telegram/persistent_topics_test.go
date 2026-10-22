package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

// topicServer records the calls the worker makes while provisioning a topic.
type topicServer struct {
	created  []string
	reopened int
	sends    []map[string]any
	calls    []string
}

func newTopicServer(t *testing.T) (*Client, *topicServer) {
	t.Helper()
	record := &topicServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		record.calls = append(record.calls, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/createForumTopic"):
			name, _ := p["name"].(string)
			record.created = append(record.created, name)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":55}}`))
		case strings.HasSuffix(r.URL.Path, "/reopenForumTopic"):
			record.reopened++
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			record.sends = append(record.sends, p)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":75}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		}
	}))
	t.Cleanup(server.Close)
	client := NewClient("test")
	client.BaseURL = server.URL
	return client, record
}

func (s *topicServer) sentTo(chat, topic int64) []string {
	var texts []string
	for _, body := range s.sends {
		if body["chat_id"] != float64(chat) || body["message_thread_id"] != float64(topic) {
			continue
		}
		if text, _ := body["text"].(string); text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func TestFirstClaimNamesClientTopicAndLaterTicketReusesIt(t *testing.T) {
	ctx := context.Background()
	client, record := newTopicServer(t)
	chatID := int64(-1001234567890)
	first := domain.Ticket{ID: 7, ClientID: 12, CompanyID: 10, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, RoutingPending: true, RoutingGeneration: 1, CreatedAt: time.Now().UTC()}
	store := &workerStore{ticket: first, current: first}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: chatID, QueueTopicID: 1}}})
	worker.interval = 0
	if err := worker.Handle(ctx, domain.Operation{ID: 1, Kind: "create_topic", TicketID: first.ID, Payload: json.RawMessage(`{"routing_generation":1}`)}); err != nil {
		t.Fatal(err)
	}
	if len(record.created) != 1 || record.created[0] != "Клиент №1 · Иван Клиент · Компания Тест" {
		t.Fatalf("client topic name = %q", record.created)
	}
	if binding := store.bindings[domain.IT]; binding.ClientID != 12 || binding.ChatID != chatID || binding.TopicID != 55 {
		t.Fatalf("client binding = %+v", binding)
	}
	if store.current.TopicID != 55 || store.current.TopicChatID != chatID || store.current.TopicMessageID != 75 {
		t.Fatalf("ticket was not bound to the created card: %+v", store.current)
	}
	foundCard := false
	for _, text := range record.sentTo(chatID, 55) {
		foundCard = foundCard || strings.Contains(text, "Заявка #7")
	}
	if !foundCard {
		t.Fatal("ticket card was not posted in the permanent topic")
	}

	previous := store.current
	previous.Status = domain.Closed
	previous.TopicMessageID = 75
	second := domain.Ticket{ID: 8, ClientID: 12, CompanyID: 10, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, RoutingPending: true, RoutingGeneration: 1, CreatedAt: time.Now().UTC()}
	reuse := &workerStore{
		ticket:   second,
		current:  second,
		bindings: map[domain.Department]domain.ClientTopic{domain.IT: {ClientID: 12, Department: domain.IT, ChatID: chatID, TopicID: 55}},
		previous: previous,
	}
	before := len(record.sends)
	reuseWorker := NewWorker(client, reuse, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: chatID, QueueTopicID: 1}}})
	reuseWorker.interval = 0
	if err := reuseWorker.Handle(ctx, domain.Operation{ID: 2, Kind: "create_topic", TicketID: second.ID, Payload: json.RawMessage(`{"routing_generation":1}`)}); err != nil {
		t.Fatal(err)
	}
	if len(record.created) != 1 || record.reopened != 1 {
		t.Fatalf("permanent topic was not reused: created=%v reopened=%d", record.created, record.reopened)
	}
	if reuse.current.TopicID != 55 || reuse.current.TopicMessageID != 75 {
		t.Fatalf("second ticket was not bound to the same topic: %+v", reuse.current)
	}
	cardStarted, cardLinked, cardURL := false, false, false
	for _, body := range record.sends[before:] {
		if body["chat_id"] != float64(chatID) || body["message_thread_id"] != float64(55) {
			continue
		}
		text, _ := body["text"].(string)
		cardStarted = cardStarted || strings.Contains(text, "Заявка #8")
		cardLinked = cardLinked || strings.Contains(text, "Предыдущая заявка в этом отделе: #7")
		cardURL = cardURL || strings.Contains(text, "https://t.me/c/1234567890/75")
	}
	if !cardStarted || !cardLinked || !cardURL {
		t.Fatalf("new card does not start the ticket with a previous-card link: started=%v link=%v url=%v %v", cardStarted, cardLinked, cardURL, record.sentTo(chatID, 55))
	}
}

func TestDelayedCloseDoesNotCloseTopicClaimedByNewTicket(t *testing.T) {
	ctx := context.Background()
	client, record := newTopicServer(t)
	closedAt := time.Now().UTC()
	old := domain.Ticket{ID: 7, ClientID: 12, CompanyID: 10, Department: domain.IT, Status: domain.Closed, TopicChatID: -100, TopicID: 10, ClosedAt: &closedAt, ClosedBy: 2}
	store := &workerStore{
		ticket:      old,
		current:     old,
		activeTopic: domain.Ticket{ID: 8, ClientID: 12, CompanyID: 10, Department: domain.IT, Status: domain.InProgress, TopicChatID: -100, TopicID: 10},
	}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	worker.interval = 0
	op := domain.Operation{ID: 42, TicketID: 7, Kind: "close_topic", Payload: json.RawMessage(`{"chat_id":-100,"topic_id":10,"text":"Заявка #7 закрыта. Итог: Готово"}`)}
	if err := worker.Handle(ctx, op); err != nil {
		t.Fatal(err)
	}
	if len(record.calls) != 0 {
		t.Fatalf("delayed close touched the conversation of the new ticket: %v", record.calls)
	}

	// Without an active successor the same operation closes the topic normally.
	store.activeTopic = domain.Ticket{}
	if err := worker.Handle(ctx, op); err != nil {
		t.Fatal(err)
	}
	if len(record.calls) != 2 || !strings.HasSuffix(record.calls[0], "/sendMessage") || !strings.HasSuffix(record.calls[1], "/closeForumTopic") {
		t.Fatalf("close calls = %v", record.calls)
	}
}

func TestClosingNoticeNamesTicketResolutionAuthorAndTime(t *testing.T) {
	ctx := context.Background()
	client, record := newTopicServer(t)
	closedAt := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC) // 10:00 Irkutsk
	ticket := domain.Ticket{ID: 7, ClientID: 12, CompanyID: 10, Department: domain.IT, Status: domain.Closed, TopicChatID: -100, TopicID: 10, ClosedAt: &closedAt, ClosedBy: 2}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	worker.interval = 0
	if err := worker.Handle(ctx, domain.Operation{ID: 42, TicketID: 7, Kind: "close_topic", Payload: json.RawMessage(`{"chat_id":-100,"topic_id":10,"text":"Заявка #7 закрыта. Итог: Готово"}`)}); err != nil {
		t.Fatal(err)
	}
	want := "Заявка #7 закрыта. Итог: Готово\nЗавершил: Иван Клиент · 20.10.2026 10:00 (Иркутск)"
	texts := record.sentTo(-100, 10)
	if len(texts) != 1 || texts[0] != want {
		t.Fatalf("closing notice = %q, want %q", texts, want)
	}
}

func TestCloseOnDeletedTopicDoesNotBecomePermanentFailure(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"TOPIC_ID_INVALID"}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	closedAt := time.Now().UTC()
	ticket := domain.Ticket{ID: 7, ClientID: 12, CompanyID: 10, Department: domain.IT, Status: domain.Closed, TopicChatID: -1001234567890, TopicID: 10, ClosedAt: &closedAt, ClosedBy: 2}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -1001234567890, QueueTopicID: 1}}})
	worker.interval = 0
	if err := worker.Handle(context.Background(), domain.Operation{ID: 42, TicketID: 7, Kind: "close_topic", Payload: json.RawMessage(`{"chat_id":-1001234567890,"topic_id":10,"text":"Заявка #7 закрыта. Итог: Готово"}`)}); err != nil {
		t.Fatalf("a deleted topic kept the close operation failing: %v", err)
	}
	if len(calls) != 2 || !strings.HasSuffix(calls[0], "/sendMessage") || !strings.HasSuffix(calls[1], "/closeForumTopic") {
		t.Fatalf("calls = %v", calls)
	}
}
