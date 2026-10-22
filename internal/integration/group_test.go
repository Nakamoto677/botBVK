package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"bot-bvk/internal/domain"
	"bot-bvk/internal/postgres"
	"bot-bvk/internal/telegram"
	"bot-bvk/internal/tickets"
	"github.com/jackc/pgx/v5"
)

func TestGroupIntakeCreatesTicketWithoutTopicAndClosesSilently(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close(ctx)
	name := fmt.Sprintf("bvk_group_%x", time.Now().UnixNano())
	if _, err = base.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, `DROP DATABASE "`+name+`" WITH (FORCE)`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	store, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.BootstrapCompanies(ctx, 99, []domain.CompanyDefinition{{Key: "groupco", Name: "Group Co"}}); err != nil {
		t.Fatal(err)
	}
	companyID, err := store.CompanyIDByIdentifier(ctx, "groupco")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		return tx.SaveUser(ctx, domain.User{ID: 2, Name: "Анна Инженер", Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT, domain.OneC}})
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls []string
	var sent []map[string]any
	edited := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		calls = append(calls, method)
		messageID := 0
		switch method {
		case "sendMessage":
			sent = append(sent, p)
			messageID = 100 + len(sent)
		case "editMessageText":
			edited++
		}
		mu.Unlock()
		result := any(true)
		switch method {
		case "sendMessage":
			result = map[string]any{"message_id": messageID, "chat": map[string]any{"id": p["chat_id"]}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer server.Close()

	client := telegram.NewClient("test")
	client.BaseURL = server.URL
	cfg := telegram.Config{
		Departments: map[domain.Department]telegram.Team{
			domain.IT:   {ChatID: -1001, QueueTopicID: 2, Tags: []string{"#it", "#ит"}},
			domain.OneC: {ChatID: -1002, QueueTopicID: 2, Tags: []string{"#1c", "#1с"}},
		},
		ClientGroups: map[int64]telegram.ClientGroup{-100500: {CompanyID: companyID}},
		BotUsername:  "supportbot",
		ArchiveDir:   t.TempDir(),
	}
	svc := tickets.New(store)
	adapter := telegram.NewAdapter(client, store, svc, cfg)
	worker := telegram.NewWorker(client, store, cfg)
	drain := func() {
		t.Helper()
		for range 30 {
			op, e := store.NextOperation(ctx)
			if errors.Is(e, domain.ErrNotFound) {
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = worker.Handle(ctx, op); e != nil {
				t.Fatalf("operation %s: %v", op.Kind, e)
			}
			if e = store.FinishOperation(ctx, op.ID); e != nil {
				t.Fatal(e)
			}
		}
		t.Fatal("operation queue did not drain")
	}
	groupMessage := func(text string, id int64, author int64) *telegram.Message {
		return &telegram.Message{
			ID:   id,
			From: telegram.Person{ID: author, FirstName: "Иван", LastName: "Петров", Username: "ivan"},
			Chat: telegram.Chat{ID: -100500, Type: "supergroup", Title: "ООО Ромашка"},
			Text: text,
		}
	}
	handle := func(m *telegram.Message) {
		t.Helper()
		if err := adapter.Handle(ctx, telegram.Update{Message: m}); err != nil {
			t.Fatal(err)
		}
		drain()
	}

	handle(groupMessage("#1C Не открывается 1С", 55, 100))
	ticket, err := store.ActiveTicket(ctx, -100500)
	if err != nil {
		t.Fatal(err)
	}
	if !ticket.IsGroup() || ticket.Department != domain.OneC || ticket.CompanyID != companyID || ticket.RequesterID != 100 || ticket.SourceMessageID != 55 {
		t.Fatalf("ticket: %+v", ticket)
	}
	mu.Lock()
	for _, method := range calls {
		if method == "createForumTopic" {
			mu.Unlock()
			t.Fatal("group ticket created a department topic")
		}
	}
	mu.Unlock()
	// The only send so far is the queue card in the department, not the company chat.
	mu.Lock()
	if len(sent) != 1 {
		mu.Unlock()
		t.Fatalf("sends: %+v", sent)
	}
	card := sent[0]
	mu.Unlock()
	if card["chat_id"] != float64(-1002) || card["message_thread_id"] != float64(2) {
		t.Fatalf("queue card destination: %+v", card)
	}
	text, _ := card["text"].(string)
	rawMarkup, _ := json.Marshal(card["reply_markup"])
	if !strings.Contains(text, "Чат компании:") || !strings.Contains(text, "Обратился:") || !strings.Contains(string(rawMarkup), "https://t.me/c/500/55") || !strings.Contains(string(rawMarkup), "В чат компании") {
		t.Fatalf("group card: text=%q markup=%s", text, rawMarkup)
	}

	handle(groupMessage("Уточнение от клиента", 56, 100))
	handle(groupMessage("Проверяю", 57, 2))
	active, err := store.Ticket(ctx, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != domain.InProgress || active.AssigneeID != 2 || active.FirstResponseAt == nil {
		t.Fatalf("first reply did not claim: %+v", active)
	}

	mu.Lock()
	beforeCloseSends := len(sent)
	mu.Unlock()
	if err = svc.Close(ctx, 2, ticket.ID, "Перезагрузили шлюз"); err != nil {
		t.Fatal(err)
	}
	drain()
	closed, err := store.Ticket(ctx, ticket.ID)
	if err != nil || closed.Status != domain.Closed || closed.Resolution != "Перезагрузили шлюз" {
		t.Fatalf("closed: %+v %v", closed, err)
	}
	mu.Lock()
	afterCloseSends := sent[beforeCloseSends:]
	mu.Unlock()
	for _, body := range afterCloseSends {
		if body["chat_id"] == float64(-100500) {
			t.Fatal("closure posted into the company chat")
		}
	}
	mu.Lock()
	for _, method := range calls {
		if method == "closeForumTopic" {
			mu.Unlock()
			t.Fatal("group closure closed a topic")
		}
	}
	if edited == 0 {
		mu.Unlock()
		t.Fatal("queue card was not refreshed after closure")
	}
	mu.Unlock()

	handle(groupMessage("#1с Новая проблема", 58, 101))
	second, err := store.ActiveTicket(ctx, -100500)
	if err != nil || second.ID == ticket.ID || second.Department != domain.OneC {
		t.Fatalf("second ticket: %+v %v", second, err)
	}
}
