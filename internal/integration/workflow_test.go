package integration

import (
	"context"
	"encoding/csv"
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
	var mediaCards []map[string]any
	createdTopics, reopenedTopics, closedTopics := 0, 0, 0
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
		case "reopenForumTopic":
			reopenedTopics++
		case "closeForumTopic":
			closedTopics++
		case "getFile":
			result = map[string]any{"file_id": "resolution-file", "file_path": "resolution.bin", "file_size": 15}
		case "sendDocument":
			mediaCards = append(mediaCards, p)
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
	if err = store.PutDraft(ctx, 1, domain.Draft{Generation: "integration-form", Step: "confirm", Department: domain.IT, Urgency: domain.Urgent, Text: "Cannot log in", Attachments: []domain.Message{{TelegramChatID: 1, TelegramMessageID: 8, FileID: "initial-file", MediaType: "document", FileName: "initial.txt", Text: "Initial file"}}}); err != nil {
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
	if queued.QueueMediaType != "document" {
		t.Fatalf("queue card did not keep the form attachment: %+v", queued)
	}
	mediaCard := false
	for _, card := range mediaCards {
		caption, _ := card["caption"].(string)
		mediaCard = mediaCard || card["chat_id"] == float64(-1001) && card["message_thread_id"] == float64(2) && strings.Contains(caption, "<b>Описание</b>") && strings.Contains(caption, "Cannot log in")
	}
	if !mediaCard {
		t.Fatalf("queue card was not sent as a media message: %+v", mediaCards)
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
	firstDept, _ := store.Ticket(ctx, ticket.ID)
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
	foundContact, foundTransfer, foundClose, foundCloseAuthor := false, false, false, false
	mu.Lock()
	for _, send := range sends {
		text, _ := send["text"].(string)
		if strings.Contains(text, "@client_contact") && send["chat_id"] == float64(-1001) {
			foundContact = true
		}
		if strings.Contains(text, "передана в отдел 1С") && send["chat_id"] == float64(-1001) {
			foundTransfer = true
		}
		if strings.Contains(text, "Заявка #") && strings.Contains(text, "закрыта. Итог:") && send["chat_id"] == float64(-1002) && send["message_thread_id"] == float64(active.TopicID) {
			foundClose = true
		}
		if strings.Contains(text, "Завершил:") && strings.Contains(text, "(Иркутск)") && send["chat_id"] == float64(-1002) {
			foundCloseAuthor = true
		}
	}
	mu.Unlock()
	if !foundContact || !foundTransfer || !foundClose || !foundCloseAuthor {
		t.Fatalf("missing UI: contact=%v transfer=%v close=%v author=%v", foundContact, foundTransfer, foundClose, foundCloseAuthor)
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
	newTicket, err := svc.Create(ctx, 1, domain.IT, "New problem")
	if err != nil || newTicket.ID == ticket.ID {
		t.Fatal(err, newTicket)
	}
	drain()
	queuedAgain, _ := store.Ticket(ctx, newTicket.ID)
	if queuedAgain.TopicID != 0 {
		t.Fatal("topic reopened before claim")
	}
	messages, _ := store.Messages(ctx, ticket.ID)
	if len(messages) != 7 {
		t.Fatalf("archive lost messages: %d", len(messages))
	}
	last := messages[len(messages)-1]
	if last.FileID != "resolution-file" || last.ArchivePath == "" || len(last.SHA256) != 64 {
		t.Fatalf("closure attachment was not archived: %+v", last)
	}

	if err = svc.Claim(ctx, 2, newTicket.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	newCycle, _ := store.Ticket(ctx, newTicket.ID)
	if newCycle.TopicID != firstDept.TopicID || createdTopics != 2 || reopenedTopics != 1 {
		t.Fatalf("permanent topic not reused: %+v, created=%d reopened=%d", newCycle, createdTopics, reopenedTopics)
	}
	// The ticket left IT in between, so its current card lives in the 1C topic and
	// the IT card history has no previous ticket bound to this department. The
	// dedicated same-department scenario covers the "Предыдущая заявка" link.
	if err = svc.Cancel(ctx, 1, newTicket.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	foundCancel := false
	mu.Lock()
	for _, send := range sends {
		text, _ := send["text"].(string)
		if send["chat_id"] == float64(-1001) && send["message_thread_id"] == float64(newCycle.TopicID) && strings.Contains(text, fmt.Sprintf("Заявка #%d отменена.", newTicket.ID)) {
			foundCancel = true
		}
	}
	mu.Unlock()
	if !foundCancel {
		t.Fatal("cancellation left no notice with the ticket number")
	}
	if closedTopics != 3 {
		// Transfer closed the IT topic, completion closed the 1C topic, and the
		// cancelled second ticket closed the reused IT topic.
		t.Fatalf("topics closed after completion and cancellation = %d", closedTopics)
	}
	finalHistory, _ := store.Messages(ctx, ticket.ID)
	if len(finalHistory) != len(messages) {
		t.Fatal("history lost after deletion")
	}
	csvText, err := report.CSV(ctx, store, tickets.DefaultCalendar())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(csvText)).ReadAll()
	if err != nil || len(rows) != 3 || rows[1][len(rows[1])-2] != "1" || rows[1][len(rows[1])-1] != "0" || rows[2][len(rows[2])-2] != "0" || rows[2][len(rows[2])-1] != "1" {
		t.Fatalf("ticket metrics were mixed: %s: %v", csvText, err)
	}
}

// TestClosedTicketAndNewTicketShareOneDepartmentTopic covers the primary
// persistent-topic scenario from the plan: a first ticket is closed in its own
// department, and the next ticket gets a new number and its own card but continues
// in the same topic, linking back to the previous card.
func TestClosedTicketAndNewTicketShareOneDepartmentTopic(t *testing.T) {
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
	name := fmt.Sprintf("bvk_same_dept_%x", time.Now().UnixNano())
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
		return tx.SaveUser(ctx, domain.User{ID: 2, Name: "Engineer", Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}})
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sends, edits []map[string]any
	createdTopics, reopenedTopics := 0, 0
	counter := 100
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
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
		case "sendMessage":
			sends = append(sends, p)
			result = map[string]any{"message_id": counter, "chat": map[string]any{"id": p["chat_id"]}}
		case "editMessageText":
			edits = append(edits, p)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer server.Close()
	client := telegram.NewClient("test")
	client.BaseURL = server.URL
	cfg := telegram.Config{Departments: map[domain.Department]telegram.Team{domain.IT: {ChatID: -1001, QueueTopicID: 2}}, ArchiveDir: t.TempDir()}
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

	first, err := svc.Create(ctx, 1, domain.IT, "Первый вопрос")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Claim(ctx, 2, first.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	firstBound, _ := store.Ticket(ctx, first.ID)
	if createdTopics != 1 || firstBound.TopicID == 0 {
		t.Fatalf("first ticket has no permanent topic: %+v created=%d", firstBound, createdTopics)
	}
	cardFields := map[string]bool{}
	mu.Lock()
	for _, send := range sends {
		if send["chat_id"] != float64(-1001) || send["message_thread_id"] != float64(firstBound.TopicID) {
			continue
		}
		text, _ := send["text"].(string)
		if !strings.Contains(text, fmt.Sprintf("Заявка #%d", first.ID)) {
			continue
		}
		for _, marker := range []string{"Начало:", "Отдел:</b> IT", "Исполнитель:</b> Engineer", "Срочность:", "Первый вопрос"} {
			if strings.Contains(text, marker) {
				cardFields[marker] = true
			}
		}
	}
	mu.Unlock()
	if len(cardFields) != 5 {
		t.Fatalf("ticket card misses plan fields: %v", cardFields)
	}
	if err = svc.AddMessage(ctx, 1, first.ID, domain.Message{Text: "Уточнение 1", TelegramChatID: 1, TelegramMessageID: 5}, false); err != nil {
		t.Fatal(err)
	}
	drain()
	if err = svc.Close(ctx, 2, first.ID, "Готово"); err != nil {
		t.Fatal(err)
	}
	drain()
	closed, _ := store.Ticket(ctx, first.ID)
	if closed.Status != domain.Closed {
		t.Fatalf("first ticket not closed: %+v", closed)
	}
	mu.Lock()
	foundClose, foundAuthor := false, false
	lastCardEdit := map[string]any{}
	for _, send := range sends {
		text, _ := send["text"].(string)
		if send["chat_id"] == float64(-1001) && strings.Contains(text, fmt.Sprintf("Заявка #%d закрыта. Итог: Готово", first.ID)) {
			foundClose = true
		}
		if strings.Contains(text, "Завершил:") && strings.Contains(text, "(Иркутск)") {
			foundAuthor = true
		}
	}
	for _, edit := range edits {
		if edit["chat_id"] == float64(-1001) && edit["message_id"] == float64(firstBound.TopicMessageID) {
			lastCardEdit = edit
		}
	}
	mu.Unlock()
	if !foundClose || !foundAuthor {
		t.Fatalf("closing notice missing number/author: close=%v author=%v", foundClose, foundAuthor)
	}
	if markup, _ := json.Marshal(lastCardEdit["reply_markup"]); strings.Contains(string(markup), "close:") || strings.Contains(string(markup), "transfer:") {
		t.Fatalf("closed ticket card kept controls: %s", markup)
	}

	second, err := svc.Create(ctx, 1, domain.IT, "Второй вопрос")
	if err != nil || second.ID == first.ID {
		t.Fatal(err, second)
	}
	drain()
	queued, _ := store.Ticket(ctx, second.ID)
	if queued.TopicID != 0 {
		t.Fatal("new ticket opened the topic before claim")
	}
	if err = svc.Claim(ctx, 2, second.ID); err != nil {
		t.Fatal(err)
	}
	drain()
	secondBound, _ := store.Ticket(ctx, second.ID)
	if createdTopics != 1 || reopenedTopics != 1 || secondBound.TopicID != firstBound.TopicID {
		t.Fatalf("new ticket did not continue the permanent topic: %+v created=%d reopened=%d", secondBound, createdTopics, reopenedTopics)
	}
	if err = svc.AddMessage(ctx, 1, second.ID, domain.Message{Text: "Уточнение 2", TelegramChatID: 1, TelegramMessageID: 6}, false); err != nil {
		t.Fatal(err)
	}
	drain()
	mu.Lock()
	cardStarted, cardLinked := false, false
	for _, send := range sends {
		text, _ := send["text"].(string)
		if send["chat_id"] == float64(-1001) && send["message_thread_id"] == float64(secondBound.TopicID) {
			cardStarted = cardStarted || strings.Contains(text, fmt.Sprintf("Заявка #%d", second.ID))
			cardLinked = cardLinked || strings.Contains(text, fmt.Sprintf("Предыдущая заявка в этом отделе: #%d", first.ID))
		}
	}
	mu.Unlock()
	if !cardStarted || !cardLinked {
		t.Fatalf("second card missing number or previous link: started=%v link=%v", cardStarted, cardLinked)
	}
	firstHistory, _ := store.Messages(ctx, first.ID)
	secondHistory, _ := store.Messages(ctx, second.ID)
	if len(firstHistory) != 1 || firstHistory[0].Text != "Уточнение 1" || len(secondHistory) != 1 || secondHistory[0].Text != "Уточнение 2" {
		t.Fatalf("per-ticket history mixed: first=%v second=%v", firstHistory, secondHistory)
	}
	firstEvents, _ := store.Events(ctx, first.ID)
	secondEvents, _ := store.Events(ctx, second.ID)
	hasKind := func(events []domain.Event, kind string) bool {
		for _, event := range events {
			if event.Kind == kind {
				return true
			}
		}
		return false
	}
	if !hasKind(firstEvents, "closed") || !hasKind(secondEvents, "created") || hasKind(secondEvents, "closed") {
		t.Fatal("events are not separated per ticket")
	}
}
