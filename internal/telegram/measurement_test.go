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

// The stage timings must be recorded from the real send path, not only from the
// metrics package: the point of the measurements is to show where the seconds of
// the claim flow actually go.
func TestSendPathRecordsRateLimitAndCallTimings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	worker := NewWorker(client, &workerStore{}, Config{})
	// A group interval far below the real 3.1 s keeps the test quick while still
	// forcing the limiter to make the second send wait for a slot.
	worker.interval = 150 * time.Millisecond

	for i := 0; i < 2; i++ {
		if _, err := worker.send(context.Background(), -100, 5, "Карточка заявки", nil); err != nil {
			t.Fatal(err)
		}
	}

	out := client.Metrics.Render()
	for _, want := range []string{
		`bvk_telegram_call_ms_count{label="sendMessage"} 2`,
		// The limiter wait is labelled by API method, so it stays attributable.
		`bvk_ratelimit_wait_ms_count{label="sendMessage"} 2`,
		`bvk_ratelimit_wait_ms_max{label="sendMessage"}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("send path did not record %q\n%s", want, out)
		}
	}
	if strings.Contains(out, `bvk_ratelimit_wait_ms_max{label="sendMessage"} 0`) {
		t.Fatalf("second send did not wait for the limiter:\n%s", out)
	}
}

func TestOperationDurationIsRecordedPerKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	ticket := domain.Ticket{ID: 3, ClientID: 1, Department: domain.IT, Status: domain.New}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	op := domain.Operation{ID: 9, Kind: "team_notification", TicketID: ticket.ID, Payload: json.RawMessage(`{"text":"hello"}`)}
	if err := worker.runOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	out := client.Metrics.Render()
	if !strings.Contains(out, `bvk_operation_s_count{label="team_notification"} 1`) {
		t.Fatalf("operation duration not recorded:\n%s", out)
	}
}

// Relay time is the user-visible number: how long a written message took to reach
// the other side. It must be attributed to the operation kind that carried it.
func TestRelayTimeIsRecordedFromMessageCreation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	ticket := domain.Ticket{ID: 3, ClientID: 1, Department: domain.IT, Status: domain.InProgress, TopicChatID: -100, TopicID: 5, AssigneeID: 2}
	store := &workerStore{ticket: ticket, current: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	msg := domain.Message{ID: 4, TicketID: ticket.ID, AuthorID: 2, Direction: "outbound", Text: "Ответ инженера", CreatedAt: time.Now().Add(-2500 * time.Millisecond)}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	op := domain.Operation{ID: 11, Kind: "client_message", TicketID: ticket.ID, Payload: raw}
	if err := worker.runOperation(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	out := client.Metrics.Render()
	if !strings.Contains(out, `bvk_ticket_relay_ms_count{label="client_message"} 1`) {
		t.Fatalf("relay time not recorded:\n%s", out)
	}
	// Buckets are inclusive, so 2.5 s of deliberate age must land above the 2000 ms
	// bound, proving the measurement reflects the wait and not only the network call.
	if !strings.Contains(out, `bvk_ticket_relay_ms_bucket{label="client_message",le="2000"} 0`) {
		t.Fatalf("relay measurement ignored the message age:\n%s", out)
	}
	if !strings.Contains(out, `bvk_ticket_relay_ms_bucket{label="client_message",le="3100"} 1`) {
		t.Fatalf("relay measurement landed in the wrong bucket:\n%s", out)
	}
}
