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

func TestIndependentOutboxOperationsRunWhileAnotherChatIsSlow(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	companies, err := store.Companies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var tickets [4]domain.Ticket
	for i := range tickets {
		clientID := int64(i + 10)
		tickets[i] = domain.Ticket{ClientID: clientID, CompanyID: companies[0].ID, Department: domain.IT, Status: domain.New}
		if err := store.Transact(ctx, func(tx domain.Tx) error {
			if err := tx.SaveUser(ctx, domain.User{ID: clientID, Role: "client", Approved: true, CompanyID: companies[0].ID}); err != nil {
				return err
			}
			return tx.SaveTicket(ctx, &tickets[i])
		}); err != nil {
			t.Fatal(err)
		}
	}
	slowStarted := make(chan struct{}, 1)
	releaseSlow := make(chan struct{})
	sent := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Text == "slow" {
			slowStarted <- struct{}{}
			select {
			case <-releaseSlow:
			case <-r.Context().Done():
				return
			}
		}
		sent <- body.Text
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	enqueue := func(ticket domain.Ticket, chat int64, message string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"user_id": ticket.ClientID, "chat_id": chat, "text": message})
		if err := store.Transact(ctx, func(tx domain.Tx) error {
			return tx.Enqueue(ctx, domain.Operation{Kind: "ui_screen", TicketID: ticket.ID, Payload: payload})
		}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(tickets[0], -100, "slow")
	errs := make(chan error, 4)
	for range 4 {
		worker := NewWorker(client, store, Config{})
		go func() { errs <- worker.Run(ctx) }()
	}
	select {
	case <-slowStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("slow operation did not start")
	}
	enqueue(tickets[0], tickets[0].ClientID, "ordered")
	for i := range 3 {
		enqueue(tickets[i+1], tickets[i+1].ClientID, "fast")
	}
	for range 3 {
		select {
		case message := <-sent:
			if message != "fast" {
				t.Fatalf("same-user operation bypassed slow predecessor: %q", message)
			}
		case <-time.After(750 * time.Millisecond):
			t.Fatal("independent operations stalled behind slow group chat")
		}
	}
	close(releaseSlow)
	for _, expected := range []string{"slow", "ordered"} {
		select {
		case message := <-sent:
			if message != expected {
				t.Fatalf("same-user order changed: got %q, want %q", message, expected)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not finish", expected)
		}
	}
	cancel()
	for range 4 {
		select {
		case <-errs:
		case <-time.After(time.Second):
			t.Fatal("worker did not stop")
		}
	}
}
