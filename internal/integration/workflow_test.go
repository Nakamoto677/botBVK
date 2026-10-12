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

func TestWorkflowQueuesUntilClaimAndPreservesMessagesAcrossTransfer(t *testing.T) {
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
	name := fmt.Sprintf("bvk_workflow_%x", time.Now().UnixNano())
	if _, err = base.Exec(ctx, `CREATE SCHEMA "`+name+`"`); err != nil {
		t.Fatal(err)
	}
	defer base.Exec(ctx, `DROP SCHEMA "`+name+`" CASCADE`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	store, err := postgres.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, 99, []string{"Client company"}); err != nil {
		t.Fatal(err)
	}
	companies, _ := store.Companies(ctx)
	svc := tickets.New(store)
	if err = svc.Register(ctx, 1, "Client", companies[0].ID); err != nil {
		t.Fatal(err)
	}
	requests, _ := store.AccessRequests(ctx)
	if err = svc.ApproveAccess(ctx, 99, requests[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		return tx.SaveUser(ctx, domain.User{ID: 2, Name: "Engineer", Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT, domain.OneC}})
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sends []map[string]any
	createdTopics, reopenedTopics := 0, 0
	counter := 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/file/") {
			_, _ = w.Write([]byte("resolution-file"))
			return
		}
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		mu.Lock()
		defer mu.Unlock()
		counter++
		var result any = true
		switch method {
		case "createForumTopic":
			createdTopics++
			result = map[string]any{"message_thread_id": counter}
		case "reopenForumTopic":
			reopenedTopics++
		case "getFile":
			result = map[string]any{"file_id": "resolution-file", "file_path": "resolution.bin", "file_size": 15}
		case "sendMessage":
			sends = append(sends, p)
			result = map[string]any{"message_id": counter, "chat": map[string]any{"id": p["chat_id"]}}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer server.Close()
	client := telegram.NewClient("test")
	client.BaseURL = server.URL
	cfg := telegram.Config{Departments: map[domain.Department]telegram.Team{domain.IT: {ChatID: -1001, QueueTopicID: 2}, domain.OneC: {ChatID: -1002, QueueTopicID: 2}}, ArchiveDir: t.TempDir()}
	worker := telegram.NewWorker(client, store, cfg)
	drain := func() {
		t.Helper()
		time.Sleep(30 * time.Millisecond)
		for range 30 {
			op, e := store.NextOperation(ctx)
			if errors.Is(e, domain.ErrNotFound) {
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if e = worker.Handle(ctx, op); e != nil {
				t.Fatalf("operation %s failed: %v", op.Kind, e)
			}
			if e = store.FinishOperation(ctx, op.ID); e != nil {
				t.Fatal(e)
			}
		}
		t.Fatal("queue did not drain")
	}
	drain()
	ticket, err := svc.Create(ctx, 1, domain.IT, "Cannot log in")
	if err != nil {
		t.Fatal(err)
	}
	drain()
	queued, _ := store.Ticket(ctx, ticket.ID)
	if queued.TopicID != 0 || queued.QueueMessageID == 0 || createdTopics != 0 {
		t.Fatalf("unclaimed ticket not queue-only: %+v, created=%d", queued, createdTopics)
	}
	if err = svc.AddMessage(ctx, 1, ticket.ID, domain.Message{Text: "before-claim", TelegramChatID: 1, TelegramMessageID: 9}, false); err != nil {
		t.Fatal(err)
	}
	drain()
	if createdTopics != 0 {
		t.Fatal("client supplement created a topic before claim")
	}
	if err = svc.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	if createdTopics != 1 {
		t.Fatalf("claim did not create exactly one topic: %d", createdTopics)
	}
	if err = svc.AddMessage(ctx, 1, ticket.ID, domain.Message{Text: "before-transfer", TelegramChatID: 1, TelegramMessageID: 10}, false); err != nil {
		t.Fatal(err)
	}
	if err = svc.Transfer(ctx, 2, ticket.ID, domain.OneC, "1C account issue"); err != nil {
		t.Fatal(err)
	}
	if err = svc.AddMessage(ctx, 1, ticket.ID, domain.Message{Text: "after-transfer", TelegramChatID: 1, TelegramMessageID: 11}, false); err != nil {
		t.Fatal(err)
	}
	drain()
	transferred, _ := store.Ticket(ctx, ticket.ID)
	if transferred.TopicID != 0 || transferred.Status != domain.New || createdTopics != 1 {
		t.Fatal("transfer created target topic before new team claimed")
	}
	if err = svc.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	if createdTopics != 2 {
		t.Fatalf("second team claim created %d total topics", createdTopics)
	}
	for text, chat := range map[string]float64{"before-transfer": -1001, "after-transfer": -1002} {
		found := false
		mu.Lock()
		for _, send := range sends {
			body, _ := send["text"].(string)
			if strings.Contains(body, text) {
				found = true
				if send["chat_id"] != chat {
					t.Errorf("%s routed to %v instead of %v", text, send["chat_id"], chat)
				}
			}
		}
		mu.Unlock()
		if !found {
			t.Errorf("lost %s", text)
		}
	}
	if err = svc.AddMessage(ctx, 2, ticket.ID, domain.Message{Text: "Password reset", TelegramChatID: -1002, TelegramMessageID: 20}, true); err != nil {
		t.Fatal(err)
	}
	pending, _ := store.Ticket(ctx, ticket.ID)
	if pending.FirstResponseAt != nil {
		t.Fatal("first response recorded before delivery")
	}
	drain()
	delivered, _ := store.Ticket(ctx, ticket.ID)
	if delivered.FirstResponseAt == nil {
		t.Fatal("delivered response missing timestamp")
	}
	if err = svc.CloseWithMessage(ctx, 2, ticket.ID, "Reset credentials", domain.Message{Text: "Reset credentials", TelegramChatID: -1002, TelegramMessageID: 21, FileID: "resolution-file", MediaType: "document", FileName: "resolution.txt", FileSize: 15}); err != nil {
		t.Fatal(err)
	}
	drain()
	closed, _ := store.Ticket(ctx, ticket.ID)
	if closed.Status != domain.Closed || closed.ClosedBy != 2 {
		t.Fatalf("closure missing: %+v", closed)
	}
	if err = svc.Reopen(ctx, 1, ticket.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	reopened, _ := store.Ticket(ctx, ticket.ID)
	if reopened.Status != domain.New || reopenedTopics != 0 {
		t.Fatal("reopen must queue ticket without reopening topic before claim")
	}
	messages, _ := store.Messages(ctx, ticket.ID)
	if len(messages) != 5 {
		t.Fatalf("archive lost messages: %d", len(messages))
	}
	last := messages[len(messages)-1]
	if last.FileID != "resolution-file" || last.ArchivePath == "" || len(last.SHA256) != 64 {
		t.Fatalf("closure attachment was not archived: %+v", last)
	}
}
