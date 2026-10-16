package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"bot-bvk/internal/domain"
)

// A client message that arrives while the topic is still being prepared must not be
// lost and must reach the topic once preparation finishes, exactly once. The window
// that matters is "binding already written, provisioning not finished": that is when
// the message cannot be sent yet and must be retained.
func TestMessageArrivingDuringTopicPreparationIsDeliveredAfterwards(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if text, ok := p["text"].(string); ok {
			mu.Lock()
			texts = append(texts, text)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL

	// The ticket is bound to a topic but provisioning has not finished, exactly as
	// during a claim that is still creating and filling the topic.
	ticket := domain.Ticket{
		ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT,
		Status: domain.InProgress, AssigneeID: 2,
		TopicChatID: -100, TopicID: 10, RoutingPending: true, RoutingGeneration: 1,
	}
	store := &workerStore{
		ticket:  ticket,
		current: ticket,
		messages: []domain.Message{{
			ID: 8, TicketID: 7, AuthorID: 1, Direction: "inbound",
			Text: "клиент написал во время подготовки", DeferredDelivery: true,
		}},
	}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	// This test is about retention, not pacing, so drop the delivery interval.
	worker.interval = 0

	// The already-bound branch of create_topic is the retry path a crash lands in,
	// and it is the same sequence a fresh claim runs after writing the binding.
	op := domain.Operation{ID: 5, Kind: "create_topic", TicketID: 7, Payload: json.RawMessage(`{"routing_generation":1}`)}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	deliveries := 0
	for _, text := range texts {
		if strings.Contains(text, "клиент написал во время подготовки") {
			deliveries++
		}
	}
	if deliveries != 1 {
		t.Fatalf("deferred message delivered %d times, want exactly 1; sends=%v", deliveries, texts)
	}
	// The deferred message carries the author's name, which is how the engineers see
	// who wrote it.
	found := false
	for _, text := range texts {
		if strings.Contains(text, "клиент написал во время подготовки") && strings.Contains(text, "Иван Клиент") {
			found = true
		}
	}
	if !found {
		t.Fatalf("deferred message lost its author: %v", texts)
	}
	// Provisioning must be finished, otherwise the ticket would stay in the
	// preparing state forever and later messages would keep deferring.
	if store.current.RoutingPending {
		t.Fatal("provisioning did not complete")
	}
}

// The reverse direction: a message written before the ticket was claimed is stored
// deferred and must be delivered by the topic setup, not dropped. This is the
// guarantee the user asked for, checked end to end through the worker.
func TestMessageWrittenBeforeClaimIsDeliveredByTopicSetup(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		if text, ok := p["text"].(string); ok {
			mu.Lock()
			texts = append(texts, text)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL

	ticket := domain.Ticket{
		ID: 9, ClientID: 1, CompanyID: 10, Department: domain.IT,
		Status: domain.InProgress, AssigneeID: 2,
		TopicChatID: -100, TopicID: 12, RoutingPending: true, RoutingGeneration: 3,
	}
	store := &workerStore{
		ticket:  ticket,
		current: ticket,
		messages: []domain.Message{{
			ID: 21, TicketID: 9, AuthorID: 1, Direction: "inbound",
			Text: "написано до взятия в работу", DeferredDelivery: true,
		}},
	}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	// This test is about retention, not pacing, so drop the delivery interval.
	worker.interval = 0
	op := domain.Operation{ID: 6, Kind: "create_topic", TicketID: 9, Payload: json.RawMessage(`{"routing_generation":3}`)}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	seen := false
	for _, text := range texts {
		if strings.Contains(text, "написано до взятия в работу") {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("message written before the claim was lost: %v", texts)
	}
}
