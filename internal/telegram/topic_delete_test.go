package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

// Deleting a topic happens inside the same transaction that holds the exclusive
// acceptance fence, so every second Telegram takes there freezes incoming updates
// for every chat. A hung Telegram must therefore not hold the fence for the client's
// full 65 s HTTP timeout.
//
// The deadline is observed on the client side through the transport: an httptest
// server does not receive the caller's context deadline, so the server cannot see it.
func TestTopicDeletionIsBoundedByTheAcceptanceFenceBudget(t *testing.T) {
	var observed time.Duration
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	client := NewClient("test")
	client.BaseURL = server.URL
	inner := client.HTTP.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if deadline, ok := r.Context().Deadline(); ok {
			observed = time.Until(deadline)
		}
		return inner.RoundTrip(r)
	})

	payload := json.RawMessage(`{"chat_id":-100,"topic_id":20,"routing_generation":2,"completion_key":"key"}`)
	ticket := domain.Ticket{ID: 1, Department: domain.IT, Status: domain.Closed, TopicChatID: -100, TopicID: 20, RoutingGeneration: 2}
	store := &workerStore{ticket: ticket, current: ticket, events: []domain.Event{{Kind: "completion_cleanup_requested", Data: payload}}}
	worker := NewWorker(client, store, Config{
		Departments:  map[domain.Department]Team{domain.IT: {ChatID: -100}},
		BeforeDelete: func(context.Context, domain.Tx, domain.Ticket, int64, int64) error { return nil },
	})

	op := domain.Operation{TicketID: 1, Kind: "delete_topic", Payload: payload}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatalf("delete failed before the request: %v", err)
	}
	if calls == 0 {
		t.Fatal("deletion made no request at all")
	}
	if observed == 0 {
		t.Fatal("deletion request carried no deadline, so a hung Telegram would hold the fence")
	}
	if observed > topicDeleteTimeout || observed < topicDeleteTimeout-time.Second {
		t.Fatalf("deletion deadline is %v, expected the %v fence budget", observed, topicDeleteTimeout)
	}
	if observed >= 65*time.Second {
		t.Fatal("deletion fell back to the HTTP client timeout of 65 s, not the fence budget")
	}
}

// roundTripFunc lets a test inspect the outgoing request without a real server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A topic that Telegram reports as already gone must count as success, otherwise a
// completed cleanup would retry forever and keep taking the fence.
func TestAlreadyAbsentTopicCountsAsDeleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"TOPIC_ID_INVALID"}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	payload := json.RawMessage(`{"chat_id":-100,"topic_id":20,"routing_generation":2,"completion_key":"key"}`)
	ticket := domain.Ticket{ID: 1, Department: domain.IT, Status: domain.Closed, TopicChatID: -100, TopicID: 20, RoutingGeneration: 2}
	store := &workerStore{ticket: ticket, current: ticket, events: []domain.Event{{Kind: "completion_cleanup_requested", Data: payload}}}
	worker := NewWorker(client, store, Config{
		Departments:  map[domain.Department]Team{domain.IT: {ChatID: -100}},
		BeforeDelete: func(context.Context, domain.Tx, domain.Ticket, int64, int64) error { return nil },
	})
	op := domain.Operation{TicketID: 1, Kind: "delete_topic", Payload: payload}
	if err := worker.Handle(context.Background(), op); err != nil {
		t.Fatalf("absent topic was not treated as deleted: %v", err)
	}
	deleted := false
	for _, event := range store.events {
		if event.Kind == "topic_deleted" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("absent topic was not recorded as deleted")
	}
	if store.current.TopicID != 0 || store.current.TopicChatID != 0 {
		t.Fatal("absent topic left a stale binding")
	}
}
