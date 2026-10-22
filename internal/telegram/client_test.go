package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func TestTopicLifecycleRetriesAreIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: TOPIC_NOT_MODIFIED"}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	if err := client.CloseTopic(context.Background(), -100, 10); err != nil {
		t.Fatal(err)
	}
	if err := client.ReopenTopic(context.Background(), -100, 10); err != nil {
		t.Fatal(err)
	}
	if topicLifecycleError(&APIError{Code: 403, Description: "Forbidden"}) == nil {
		t.Fatal("permission failure hidden")
	}
}

func checkClientServer(t *testing.T, privacy bool, groupStatus string, groupChat map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var result any
		switch method {
		case "getMe":
			result = map[string]any{"id": 9, "username": "supportbot", "can_read_all_group_messages": privacy}
		case "getChat":
			chat, _ := p["chat_id"].(float64)
			if int64(chat) == -1001 {
				result = map[string]any{"type": "supergroup", "is_forum": true}
			} else {
				result = groupChat
			}
		case "getChatMember":
			chat, _ := p["chat_id"].(float64)
			if int64(chat) == -1001 {
				result = map[string]any{"status": "administrator", "can_manage_topics": true, "can_delete_messages": true}
			} else {
				result = map[string]any{"status": groupStatus}
			}
		default:
			result = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
}

func TestCheckClientGroups(t *testing.T) {
	cfg := Config{
		Departments:  map[domain.Department]Team{domain.IT: {ChatID: -1001, QueueTopicID: 2}},
		ClientGroups: map[int64]ClientGroup{-100500: {CompanyID: 10}},
	}
	t.Run("administrator passes", func(t *testing.T) {
		server := checkClientServer(t, false, "administrator", map[string]any{"type": "supergroup", "is_forum": false})
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		if err := client.Check(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if client.Username != "supportbot" {
			t.Fatalf("username: %q", client.Username)
		}
	})
	t.Run("privacy mode without admin fails", func(t *testing.T) {
		server := checkClientServer(t, false, "member", map[string]any{"type": "supergroup", "is_forum": false})
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		if err := client.Check(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "privacy mode") {
			t.Fatalf("privacy accepted: %v", err)
		}
	})
	t.Run("privacy disabled member passes", func(t *testing.T) {
		server := checkClientServer(t, true, "member", map[string]any{"type": "supergroup", "is_forum": false})
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		if err := client.Check(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("forum rejected", func(t *testing.T) {
		server := checkClientServer(t, true, "administrator", map[string]any{"type": "supergroup", "is_forum": true})
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		if err := client.Check(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "forum") {
			t.Fatalf("forum accepted: %v", err)
		}
	})
	t.Run("basic group takes the invite link from getChat", func(t *testing.T) {
		server := checkClientServer(t, true, "administrator", map[string]any{"type": "group", "is_forum": false, "invite_link": "https://t.me/+basicInvite"})
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		// A basic group has no t.me/c form. Refusing to start over a missing
		// link would cost the channel, so the link is derived and written back.
		basic := Config{Departments: cfg.Departments, ClientGroups: map[int64]ClientGroup{-500: {CompanyID: 10}}}
		if err := client.Check(context.Background(), basic); err != nil {
			t.Fatal(err)
		}
		if link := basic.ClientGroups[-500].Link; link != "https://t.me/+basicInvite" {
			t.Fatalf("derived link: %q", link)
		}
	})
	t.Run("basic group without a visible link keeps running", func(t *testing.T) {
		server := checkClientServer(t, true, "administrator", map[string]any{"type": "group", "is_forum": false})
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		basic := Config{ClientGroups: map[int64]ClientGroup{-500: {CompanyID: 10}}}
		if err := client.Check(context.Background(), basic); err != nil {
			t.Fatalf("a missing invite link must not disable the chat: %v", err)
		}
		if link := basic.ClientGroups[-500].Link; link != "" {
			t.Fatalf("link must stay empty: %q", link)
		}
	})
}

// failingChatServer answers every startup check like a healthy installation
// except getChat for one chat, which is what a company group the bot cannot see
// looks like.
func failingChatServer(t *testing.T, failChat int64, code int, description string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		chat, _ := p["chat_id"].(float64)
		if method == "getChat" && int64(chat) == failChat {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": code, "description": description})
			return
		}
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 9, "username": "supportbot"}
		case "getChat":
			result = map[string]any{"type": "supergroup", "is_forum": true}
		case "getChatMember":
			result = map[string]any{"status": "administrator", "can_manage_topics": true, "can_delete_messages": true}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
}

func TestCheckRuntimeReportsClientGroupProblemsInsteadOfFailing(t *testing.T) {
	cfg := Config{
		Departments:  map[domain.Department]Team{domain.IT: {ChatID: -1001, QueueTopicID: 2}},
		ClientGroups: map[int64]ClientGroup{-100500: {CompanyID: 10}},
	}
	t.Run("chat not found is permanent and explained", func(t *testing.T) {
		server := failingChatServer(t, -100500, 400, "Bad Request: chat not found")
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		problems, err := client.CheckRuntime(context.Background(), cfg)
		if err != nil {
			t.Fatalf("client group problem must not fail the department check: %v", err)
		}
		if len(problems) != 1 {
			t.Fatalf("problems: %+v", problems)
		}
		problem := problems[0]
		if problem.ChatID != -100500 || problem.CompanyID != 10 {
			t.Fatalf("problem target: %+v", problem)
		}
		if problem.Transient {
			t.Fatal("chat not found must not be treated as transient")
		}
		// The raw answer alone does not tell an operator what to fix, so the
		// hint has to name the bot, the membership and the id shape.
		for _, want := range []string{"@supportbot", "not a member", "-100", "unrecognised group chat"} {
			if !strings.Contains(problem.Hint, want) {
				t.Fatalf("hint misses %q: %s", want, problem.Hint)
			}
		}
		if err := client.Check(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "chat not found") {
			t.Fatalf("-check must stay strict: %v", err)
		}
	})
	t.Run("telegram failure stays transient", func(t *testing.T) {
		server := failingChatServer(t, -100500, 500, "Internal Server Error")
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		problems, err := client.CheckRuntime(context.Background(), cfg)
		if err != nil || len(problems) != 1 {
			t.Fatalf("problems: %+v, err: %v", problems, err)
		}
		if !problems[0].Transient {
			t.Fatal("a 5xx must keep the company chat enabled")
		}
	})
	t.Run("department failure remains fatal", func(t *testing.T) {
		server := failingChatServer(t, -1001, 400, "Bad Request: chat not found")
		defer server.Close()
		client := NewClient("test")
		client.BaseURL = server.URL
		if _, err := client.CheckRuntime(context.Background(), cfg); err == nil {
			t.Fatal("a broken department chat must still stop the start")
		}
	})
}

func TestCheckRuntimeRejectsMissingQueueTopic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
			return
		}
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 9, "username": "supportbot"}
		case "getChat":
			result = map[string]any{"is_forum": true}
		case "getChatMember":
			result = map[string]any{"status": "administrator", "can_manage_topics": true, "can_delete_messages": true}
		case "sendChatAction":
			if p["chat_id"] != float64(-1001) || p["message_thread_id"] != float64(2) || p["action"] != "typing" {
				t.Errorf("wrong queue probe: %#v", p)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: message thread not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	_, err := client.CheckRuntime(context.Background(), Config{Departments: map[domain.Department]Team{
		domain.OneC: {ChatID: -1001, QueueTopicID: 2},
	}})
	if err == nil {
		t.Fatal("missing queue topic passed startup check")
	}
	for _, want := range []string{"1c", "chat_id=-1001", "queue_topic_id=2", "message thread not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q misses %q", err, want)
		}
	}
}

func TestVerifyRetriesTransientStartupFailures(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 502, "description": "Bad Gateway"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"type": "group"}})
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	client.retryDelay = time.Millisecond
	var chat map[string]any
	if err := client.verify(context.Background(), "getChat", map[string]any{"chat_id": -500}, &chat); err != nil {
		t.Fatalf("a blip must not fail the start: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls: %d, want one retry", calls.Load())
	}
}

func TestVerifyGivesUpAfterBoundedRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 500, "description": "Internal Server Error"})
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	client.retryDelay = time.Millisecond
	err := client.verify(context.Background(), "getChat", map[string]any{"chat_id": -500}, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "getChat") {
		t.Fatalf("the failing call must be named: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls: %d, want exactly three attempts", calls.Load())
	}
}

func TestVerifyDoesNotRetryPermanentFailures(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": "Bad Request: chat not found"})
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	client.retryDelay = time.Millisecond
	if err := client.verify(context.Background(), "getChat", map[string]any{"chat_id": -500}, &struct{}{}); err == nil {
		t.Fatal("a wrong chat_id must fail")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls: %d, a definitive 4xx must not be retried", calls.Load())
	}
}

func TestVerifyReportsAnUnreachableNetwork(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := NewClient("test")
	client.BaseURL = server.URL
	client.retryDelay = time.Millisecond
	server.Close() // every attempt hits a closed port
	err := client.verify(context.Background(), "getMe", map[string]any{}, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "transport failure") || !strings.Contains(err.Error(), "getMe") {
		t.Fatalf("error must name the call and the transport failure: %v", err)
	}
}
