package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

func TestAPIRequestAndRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest/sendMessage" {
			t.Errorf("path: %s", r.URL.Path)
		}
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p["chat_id"] != float64(42) || p["message_thread_id"] != float64(7) {
			t.Errorf("payload: %#v", p)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":31}}`))
	}))
	defer server.Close()
	c := NewClient("test")
	c.BaseURL = server.URL
	_, err := c.Send(context.Background(), 42, 7, "hello", nil)
	var api *APIError
	if !errors.As(err, &api) || api.RetryAfter != 31 || api.Ambiguous {
		t.Fatalf("unexpected error: %#v", err)
	}
}
func TestTransportFailureDoesNotExposeToken(t *testing.T) {
	c := NewClient("secret-token")
	c.BaseURL = "http://127.0.0.1:1"
	err := c.Call(context.Background(), "getMe", nil, nil)
	var api *APIError
	if !errors.As(err, &api) || !api.Ambiguous {
		t.Fatal(err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Fatal("token exposed")
	}
}
func TestDecodeMediaAndReply(t *testing.T) {
	var u Update
	err := json.Unmarshal([]byte(`{"update_id":12,"message":{"message_id":25,"message_thread_id":9,"from":{"id":777},"chat":{"id":-1001,"type":"supergroup"},"caption":"/reply ответ","document":{"file_id":"abc","file_name":"manual.pdf","file_size":100},"reply_to_message":{"message_id":20},"media_group_id":"album"}}`), &u)
	if err != nil {
		t.Fatal(err)
	}
	if u.Message.Document.ID != "abc" || messageText(u.Message) != "/reply ответ" || u.Message.Reply.ID != 20 {
		t.Fatalf("decode: %#v", u)
	}
}
func TestStaffAuthorization(t *testing.T) {
	ticket := domain.Ticket{Department: domain.IT}
	for _, tc := range []struct {
		user domain.User
		want bool
	}{{domain.User{Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, true}, {domain.User{Role: "engineer", Approved: true, Departments: []domain.Department{domain.OneC}}, false}, {domain.User{Role: "admin", Approved: false}, false}, {domain.User{Role: "dispatcher", Approved: true}, true}, {domain.User{Role: "client", Approved: true}, false}} {
		if got := staffAuthorized(tc.user, ticket); got != tc.want {
			t.Errorf("%#v => %v", tc.user, got)
		}
	}
}
func TestCurrentSourceRejectsRetiredQueueAndTopics(t *testing.T) {
	a := NewAdapter(nil, nil, nil, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	ticket := domain.Ticket{Department: domain.IT, TopicChatID: -100, TopicID: 10, QueueMessageID: 50}
	for _, tc := range []struct {
		m    Message
		want bool
	}{{Message{Chat: Chat{ID: -100}, ThreadID: 10, ID: 20}, true}, {Message{Chat: Chat{ID: -100}, ThreadID: 9, ID: 20}, false}, {Message{Chat: Chat{ID: -100}, ThreadID: 1, ID: 50}, true}, {Message{Chat: Chat{ID: -100}, ThreadID: 1, ID: 49, Text: "old queue card"}, false}} {
		if got := a.currentSource(&tc.m, ticket); got != tc.want {
			t.Errorf("%#v => %v", tc.m, got)
		}
	}
}
