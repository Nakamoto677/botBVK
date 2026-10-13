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

	"bot-bvk/internal/archive"
	"bot-bvk/internal/config"
	"bot-bvk/internal/domain"
	"bot-bvk/internal/monitor"
	"bot-bvk/internal/postgres"
	"bot-bvk/internal/report"
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
	// Persist optional contact data in the same JSON profile as existing users.
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		user, e := tx.GetUser(ctx, 1)
		if e != nil {
			return e
		}
		user.Username = "client_contact"
		return tx.SaveUser(ctx, user)
	}); err != nil {
		t.Fatal(err)
	}
	var sends []map[string]any
	createdTopics, reopenedTopics := 0, 0
	var deletedTopics []int64
	counter := 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/file/") {
			_, _ = w.Write([]byte("resolution-file"))
			return
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			w.Write([]byte(`{"ok":true,"result":{"message_id":999}}`))
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
		case "deleteForumTopic":
			deletedTopics = append(deletedTopics, int64(p["message_thread_id"].(float64)))
		case "reopenForumTopic":
			reopenedTopics++
		case "getFile":
			result = map[string]any{"file_id": "resolution-file", "file_path": "resolution.bin", "file_size": 15}
		case "sendDocument":
			result = map[string]any{"message_id": counter}
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
	scheduler := &monitor.Scheduler{Store: store, Config: config.Config{AdminID: 99, BackupDir: t.TempDir(), ArchiveDir: cfg.ArchiveDir}, Calendar: tickets.DefaultCalendar()}
	scheduler.Backup = func(ctx context.Context) (string, error) {
		return archive.Create(ctx, u.String(), cfg.ArchiveDir, scheduler.Config.BackupDir)
	}
	cfg.BeforeDelete = scheduler.BeforeDelete
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
	if err = store.PutDraft(ctx, 1, domain.Draft{Generation: "integration-form", Step: "confirm", Department: domain.IT, Text: "Cannot log in", Attachments: []domain.Message{{TelegramChatID: 1, TelegramMessageID: 8, FileID: "initial-file", MediaType: "document", FileName: "initial.txt", Text: "Initial file"}}}); err != nil {
		t.Fatal(err)
	}
	ticket, err := svc.CreateDraft(ctx, 1, "integration-form")
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
	if err = svc.TransferWithMessage(ctx, 2, ticket.ID, domain.OneC, "1C account issue", domain.Message{TelegramChatID: -1001, TelegramMessageID: 15, FileID: "transfer-file", MediaType: "document", FileName: "private.txt", Text: "Private transfer"}); err != nil {
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
	active, _ := store.Ticket(ctx, ticket.ID)
	if err = svc.AddMessage(ctx, 2, ticket.ID, domain.Message{Text: "Password reset", TelegramChatID: -1002, TelegramTopicID: active.TopicID, TelegramMessageID: 20}, true); err != nil {
		t.Fatal(err)
	}
	if err = svc.EditMessage(ctx, 2, ticket.ID, domain.Message{TelegramChatID: -1002, TelegramMessageID: 20, Text: "Password reset corrected before delivery"}); err != nil {
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
	if err = svc.EditMessage(ctx, 2, ticket.ID, domain.Message{TelegramChatID: -1002, TelegramMessageID: 20, Text: "Password reset corrected after delivery"}); err != nil {
		t.Fatal(err)
	}
	drain()
	mu.Lock()
	responseTexts := []string{}
	for _, send := range sends {
		if send["chat_id"] == float64(1) {
			text, _ := send["text"].(string)
			if strings.Contains(text, "Password reset") {
				responseTexts = append(responseTexts, text)
			}
		}
	}
	mu.Unlock()
	if len(responseTexts) != 3 || responseTexts[0] != "Password reset" || !strings.Contains(responseTexts[1], "corrected before delivery") || !strings.Contains(responseTexts[2], "corrected after delivery") {
		t.Fatal("corrections out of order", responseTexts)
	}
	latestMessages, e := store.Messages(ctx, ticket.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, message := range latestMessages {
		if message.Direction == "outbound" {
			message.Text = "Password reset"
			message.EditedAt = nil
			raw, _ := json.Marshal(message)
			if err = worker.Handle(ctx, domain.Operation{ID: 99999, Kind: "client_message", TicketID: ticket.ID, Payload: raw}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mu.Lock()
	lateOriginals := 0
	for _, send := range sends {
		if send["chat_id"] == float64(1) && send["text"] == "Password reset" {
			lateOriginals++
		}
	}
	mu.Unlock()
	if lateOriginals != 1 {
		t.Fatal("obsolete original sent after correction", lateOriginals)
	}
	if err = svc.CloseWithMessage(ctx, 2, ticket.ID, "Reset credentials", domain.Message{Text: "Reset credentials", TelegramChatID: -1002, TelegramMessageID: 21, FileID: "resolution-file", MediaType: "document", FileName: "resolution.txt", FileSize: 15}); err != nil {
		t.Fatal(err)
	}
	drain()
	closed, _ := store.Ticket(ctx, ticket.ID)
	if closed.Status != domain.Closed || closed.ClosedBy != 2 {
		t.Fatalf("closure missing: %+v", closed)
	}
	foundContact, foundTransfer, foundClose := false, false, false
	mu.Lock()
	for _, send := range sends {
		text, _ := send["text"].(string)
		if strings.Contains(text, "@client_contact") && send["chat_id"] == float64(-1001) {
			foundContact = true
		}
		if text == "Передано в отдел 1С" && send["chat_id"] == float64(-1001) {
			foundTransfer = true
		}
		if text == "Тикет закрыт инженером" && send["chat_id"] == float64(-1002) && send["message_thread_id"] == float64(active.TopicID) {
			foundClose = true
		}
	}
	mu.Unlock()
	if !foundContact || !foundTransfer || !foundClose {
		t.Fatalf("missing UI: contact=%v transfer=%v close=%v", foundContact, foundTransfer, foundClose)
	}
	if os.Getenv("TEST_BACKUP_TOOLS") == "1" {
		if err = scheduler.CleanupTick(ctx); err != nil {
			t.Fatal(err)
		}
		drain()
		if len(deletedTopics) != 1 || deletedTopics[0] != active.TopicID {
			t.Fatalf("wrong deleted topic: %v expected %d", deletedTopics, active.TopicID)
		}
	}
	adapter := telegram.NewAdapter(client, store, svc, cfg)
	if err = adapter.Handle(ctx, telegram.Update{Message: &telegram.Message{ID: 80, From: telegram.Person{ID: 1}, Chat: telegram.Chat{ID: 1, Type: "private"}, Text: fmt.Sprintf("/ticket %d", ticket.ID)}}); err != nil {
		t.Fatal(err)
	}
	archivedMessages, e := store.Messages(ctx, ticket.ID)
	if e != nil {
		t.Fatal(e)
	}
	if err = adapter.Handle(ctx, telegram.Update{Callback: &telegram.Callback{ID: "file", From: telegram.Person{ID: 1}, Message: &telegram.Message{Chat: telegram.Chat{ID: 1, Type: "private"}}, Data: fmt.Sprintf("historyfile:%d:%d", ticket.ID, archivedMessages[0].ID)}}); err != nil {
		t.Fatal(err)
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
	if len(messages) != 7 {
		t.Fatalf("archive lost messages: %d", len(messages))
	}
	last := messages[len(messages)-1]
	if last.FileID != "resolution-file" || last.ArchivePath == "" || len(last.SHA256) != 64 {
		t.Fatalf("closure attachment was not archived: %+v", last)
	}

	if err = svc.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	newCycle, _ := store.Ticket(ctx, ticket.ID)
	if newCycle.TopicID == active.TopicID || newCycle.TopicID == 0 || createdTopics != 3 || reopenedTopics != 0 {
		t.Fatalf("reopen reused old topic: %+v", newCycle)
	}
	// Re-running preparation of an old completion never deletes the new binding.
	if os.Getenv("TEST_BACKUP_TOOLS") == "1" {
		if err = scheduler.CleanupTick(ctx); err != nil {
			t.Fatal(err)
		}
		drain()
		if len(deletedTopics) != 1 {
			t.Fatalf("old intent repeated deletion: %v", deletedTopics)
		}
	}
	if err = svc.Cancel(ctx, 1, ticket.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err = svc.Cancel(ctx, 1, ticket.ID, ""); err != nil {
		t.Fatal(err)
	}
	drain()
	if os.Getenv("TEST_BACKUP_TOOLS") == "1" {
		if err = scheduler.CleanupTick(ctx); err != nil {
			t.Fatal(err)
		}
		drain()
		if len(deletedTopics) != 2 || deletedTopics[1] != newCycle.TopicID {
			t.Fatalf("cancellation deletion wrong: %v", deletedTopics)
		}
	}
	finalHistory, _ := store.Messages(ctx, ticket.ID)
	if len(finalHistory) != len(messages) {
		t.Fatal("history lost after deletion")
	}
	csvText, err := report.CSV(ctx, store, tickets.DefaultCalendar())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(csvText, "engineer_closures,client_cancellations") || !strings.HasSuffix(strings.TrimSpace(csvText), ",1,1") {
		t.Fatalf("closure metrics combined incorrectly: %s", csvText)
	}
}
