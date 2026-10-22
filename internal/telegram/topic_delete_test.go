package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"bot-bvk/internal/domain"
)

func TestLegacyDeleteOperationNeverCallsTelegram(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{"ok":true,"result":true}`)) }))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	worker := NewWorker(client, &workerStore{}, Config{})
	if err := worker.Handle(context.Background(), domain.Operation{Kind: "delete_topic", TicketID: 1}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("legacy delete reached Telegram")
	}
}
