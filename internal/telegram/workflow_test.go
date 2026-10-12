package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

func TestQueuedTicketDoesNotCreateTopicAndShowsNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "sendMessage") {
			t.Fatalf("unexpected API before claim: %s", r.URL.Path)
		}
		var p struct {
			Text   string `json:"text"`
			Markup Markup `json:"reply_markup"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if !strings.Contains(p.Text, "Иван Клиент") || !strings.Contains(p.Text, "Компания Тест") || strings.Contains(p.Text, "123456") {
			t.Fatal("client name missing", p.Text)
		}
		for _, row := range p.Markup.Buttons {
			for _, button := range row {
				if strings.HasPrefix(button.Data, "close:") || button.URL != "" {
					t.Fatal("unclaimed ticket has active topic controls", button)
				}
			}
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	ticket := domain.Ticket{ID: 7, ClientID: 123456, CompanyID: 10, Department: domain.IT, Status: domain.New}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "refresh_card", TicketID: 7}); err != nil {
		t.Fatal(err)
	}
	if store.current.TopicID != 0 || store.current.QueueMessageID != 99 {
		t.Fatal("queued state incorrect", store.current)
	}
}

func TestClaimTopicReplaysDeferredMessagesWithoutDuplicates(t *testing.T) {
	messages := []domain.Message{{ID: 1, AuthorID: 12, Direction: "inbound", Text: "First detail", DeferredDelivery: true}, {ID: 2, AuthorID: 12, Direction: "inbound", Text: "Second detail", DeferredDelivery: true}}
	calls := 0
	replayed := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasSuffix(r.URL.Path, "createForumTopic") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_thread_id":10}}`))
			return
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		text, _ := p["text"].(string)
		for _, name := range []string{"First detail", "Second detail"} {
			if strings.Contains(text, name) {
				replayed[name]++
			}
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d}}`, 100+calls)
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	ticket := domain.Ticket{ID: 7, ClientID: 12, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, RoutingPending: true, QueueMessageID: 77}
	store := &workerStore{ticket: ticket, current: ticket, messages: messages}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	worker.interval = 0
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "create_topic", TicketID: 7}); err != nil {
		t.Fatal(err)
	}
	if store.current.TopicID != 10 || store.current.RoutingPending || store.current.QueueMessageID != 77 {
		t.Fatal("claim binding corrupted", store.current)
	}
	if err := worker.replayDeferred(context.Background(), store.current); err != nil {
		t.Fatal(err)
	}
	for name, count := range replayed {
		if count != 1 {
			t.Fatalf("%s delivered %d times", name, count)
		}
	}
	if len(replayed) != 2 {
		t.Fatal("backlog message lost", replayed)
	}
	if len(store.events) != 3 {
		t.Fatal("initial-card or replay mappings missing", store.events)
	}
}

func TestCloseAndTransferButtonsCaptureScopedTextAndCaption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":50}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}
	service := &adapterService{}
	adapter := NewAdapter(client, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	origin := &Message{ID: 20, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10}
	callback := &Callback{ID: "cb", From: Person{ID: 2}, Message: origin, Data: "close:7"}
	if err := adapter.Handle(context.Background(), Update{Callback: callback}); err != nil {
		t.Fatal(err)
	}
	other := &Message{ID: 21, From: Person{ID: 3}, Chat: origin.Chat, ThreadID: 10, Text: "Не мой итог"}
	if consumed, err := adapter.consumePrompt(context.Background(), other, store.user); consumed || err != nil {
		t.Fatal("another engineer captured close prompt")
	}
	reply := &Message{ID: 22, From: Person{ID: 2}, Chat: origin.Chat, ThreadID: 10, Caption: "Исправлена настройка"}
	if err := adapter.Handle(context.Background(), Update{Message: reply}); err != nil {
		t.Fatal(err)
	}
	if service.closedID != 7 || service.closeText != "Исправлена настройка" {
		t.Fatal("close caption not processed")
	}
	callback.Data = "transfer:7"
	if err := adapter.Handle(context.Background(), Update{Callback: callback}); err != nil {
		t.Fatal(err)
	}
	callback.Message = &Message{ID: 50, Chat: origin.Chat, ThreadID: 10}
	callback.Data = "transferdept:7:1c"
	if err := adapter.Handle(context.Background(), Update{Callback: callback}); err != nil {
		t.Fatal(err)
	}
	reply.Caption = "Нужна настройка 1С"
	if err := adapter.Handle(context.Background(), Update{Message: reply}); err != nil {
		t.Fatal(err)
	}
	if service.transferID != 7 || service.transferDepartment != domain.OneC || service.transferReason != "Нужна настройка 1С" {
		t.Fatal("transfer prompt failed")
	}
	store.ticket.AssigneeID = 3
	callback.Message = origin
	callback.Data = "close:7"
	if err := adapter.Handle(context.Background(), Update{Callback: callback}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("unassigned engineer can close", err)
	}
}

func TestApprovalNotificationShowsPersistentClientMenu(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Markup struct {
				Persistent bool `json:"is_persistent"`
				Resize     bool `json:"resize_keyboard"`
				Keyboard   [][]struct {
					Text string `json:"text"`
				} `json:"keyboard"`
			} `json:"reply_markup"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if !p.Markup.Persistent || !p.Markup.Resize || len(p.Markup.Keyboard) != 3 || p.Markup.Keyboard[0][0].Text != "📝 Новая заявка" {
			t.Fatal("persistent client menu missing", p)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	worker := NewWorker(client, &workerStore{}, Config{})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "access_notification", Payload: json.RawMessage(`{"user_id":12,"text":"Доступ одобрен"}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyPauseCannotCloseClaimedTopic(t *testing.T) {
	ticket := domain.Ticket{ID: 7, Department: domain.IT, Status: domain.InProgress, TopicChatID: -100, TopicID: 10}
	worker := NewWorker(nil, &workerStore{ticket: ticket, current: ticket}, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "close_topic", TicketID: 7, Payload: json.RawMessage(`{"chat_id":-100,"topic_id":10,"pause":true}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestReplyButtonExplicitlySendsPublicTextAndAttachment(t *testing.T) {
	client, _ := capturedClient()
	store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, TopicChatID: -100, TopicID: 10, RoutingGeneration: 1}}
	service := &adapterService{}
	adapter := NewAdapter(client, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	origin := &Message{ID: 20, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10}
	if err := adapter.Handle(context.Background(), Update{Callback: &Callback{ID: "cb", From: Person{ID: 2}, Message: origin, Data: "reply:7"}}); err != nil {
		t.Fatal(err)
	}
	message := &Message{ID: 21, From: Person{ID: 2}, Chat: origin.Chat, ThreadID: 10, Document: &File{ID: "document", Name: "Решение.pdf"}, Caption: "Настройка завершена"}
	if err := adapter.Handle(context.Background(), Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 1 || !service.public[0] || service.messages[0].Text != "Настройка завершена" || service.messages[0].FileID != "document" {
		t.Fatal("explicit response didn't preserve attachment", service.messages)
	}
	message.Document = nil
	message.Caption = ""
	message.Text = "Внутренняя заметка"
	if err := adapter.Handle(context.Background(), Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	if service.public[1] {
		t.Fatal("ordinary note became public after prompt finished")
	}
}

func TestReopenedClaimClearsProvisioningAndPreservesTransferredDeferredFiles(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{ID: 7, ClientID: 12, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, TopicChatID: -100, TopicID: 10, TopicMessageID: 99, QueueMessageID: 77, RoutingPending: true, RoutingGeneration: 2}
	store := &workerStore{ticket: ticket, current: ticket, messages: []domain.Message{{ID: 8, AuthorID: 12, Direction: "inbound", Text: "До передачи в IT", DeferredDelivery: true, RoutingDepartment: domain.OneC}}}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	worker.interval = 0
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "reopen_topic", TicketID: 7, Payload: json.RawMessage(`{"routing_generation":2}`)}); err != nil {
		t.Fatal(err)
	}
	if store.current.RoutingPending {
		t.Fatal("reopen claim stuck pending")
	}
	found := false
	for _, body := range capture.bodies {
		if text, _ := body["text"].(string); strings.Contains(text, "До передачи в IT") {
			found = true
		}
	}
	if !found {
		t.Fatal("pre-transfer deferred content was lost")
	}
	if _, _, err := worker.messageRoute(context.Background(), store.current, domain.Message{RoutingDepartment: domain.IT}); err != nil {
		t.Fatal("active messages blocked after reopening", err)
	}
}

func TestStaleClaimGenerationCannotCreateTopic(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{ID: 7, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, RoutingPending: true, RoutingGeneration: 2}
	worker := NewWorker(client, &workerStore{ticket: ticket, current: ticket}, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "create_topic", TicketID: 7, Payload: json.RawMessage(`{"routing_generation":1}`)}); err != nil {
		t.Fatal(err)
	}
	if len(capture.calls) != 0 {
		t.Fatal("stale claim created a topic")
	}
}

func TestClosedTicketCannotReceiveReplayAfterSnapshot(t *testing.T) {
	client, capture := capturedClient()
	snapshot := domain.Ticket{ID: 7, Department: domain.IT, Status: domain.InProgress, AssigneeID: 2, TopicChatID: -100, TopicID: 10, RoutingGeneration: 1}
	current := snapshot
	current.Status = domain.Closed
	store := &workerStore{ticket: snapshot, current: current, messages: []domain.Message{{ID: 8, AuthorID: 12, Direction: "inbound", Text: "Не доставлять после закрытия", DeferredDelivery: true}}}
	worker := NewWorker(client, store, Config{})
	if err := worker.replayDeferred(context.Background(), snapshot); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("stale replay accepted", err)
	}
	if len(capture.calls) != 0 || len(store.events) != 0 {
		t.Fatal("closed ticket received a replay")
	}
}
