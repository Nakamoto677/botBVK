package telegram

import (
	"bot-bvk/internal/domain"
	"bot-bvk/internal/postgres"
	"bot-bvk/internal/tickets"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

func reliabilityDatabase(t *testing.T) *postgres.Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	base, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("reliability_%x", time.Now().UnixNano())
	if _, err = base.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(func() {
		store.Close()
		base.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE")
		base.Close(context.Background())
	})
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.Bootstrap(ctx, 99, []string{"Example"}); err != nil {
		t.Fatal(err)
	}
	companies, err := store.Companies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		if e := tx.SaveUser(ctx, domain.User{ID: 1, Role: "client", Approved: true, CompanyID: companies[0].ID}); e != nil {
			return e
		}
		return tx.SaveUser(ctx, domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT, domain.OneC}})
	}); err != nil {
		t.Fatal(err)
	}
	return store
}
func TestLongTextKeepsEmojiAndAllContent(t *testing.T) {
	text := strings.Repeat("👩‍💻é 🇷🇺 ", 900)
	parts := splitText(text, messageLimit)
	if strings.Join(parts, "") != text || len(parts) < 2 {
		t.Fatal("text lost")
	}
	for _, part := range parts {
		if utf16Length(part) > messageLimit {
			t.Fatal("oversize")
		}
		r := []rune(part)
		if unicode.Is(unicode.Mn, r[0]) || r[0] == 0x200d || r[len(r)-1] == 0x200d {
			t.Fatal("grapheme split")
		}
	}
}
func TestRetainedButtonsCannotMutateAnotherForm(t *testing.T) {
	a, s, c, m, _ := clientUIFixture(t)
	s.ticket = domain.Ticket{}
	a.service = &minimalClientService{cancelUIService: c}
	ctx := context.Background()
	m.Text = "/new"
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	first := a.drafts[1].Generation
	if err := a.cancelClientDraft(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	second := a.drafts[1]
	for _, payload := range []string{"new:it:" + first, "submit:" + first, "cancel:" + first, "new:it", "submit", "cancel"} {
		// Every payload here belongs to the previous form generation, so each one is
		// a rejected screen. The error is expected; what matters is that none of
		// them may touch the form that replaced it.
		if err := a.callback(ctx, &Callback{From: m.From, Message: &Message{Chat: m.Chat, ID: 55}, Data: payload}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("old callback not rejected: %v (%s)", err, payload)
		}
		got := a.drafts[1]
		if got.Generation != second.Generation || got.Step != second.Step || got.Text != second.Text {
			t.Fatal("old callback changed new form", payload)
		}
	}
}
func TestHistoryNeverLeaksServiceTextRevisionOrFile(t *testing.T) {
	client, capture := capturedClient()
	ctx := context.Background()
	u := domain.User{ID: 1, Role: "client", Approved: true, CompanyID: 10}
	ticket := domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT}
	s := &adapterStore{user: u, ticket: ticket, storedMessages: []domain.Message{{ID: 1, Direction: "service", Text: "SECRET", FileID: "SECRET_FILE", FileName: "SECRET_NAME", Revisions: []domain.MessageRevision{{Text: "SECRET_REVISION"}}}, {ID: 2, Direction: "outbound", Text: "Public answer"}}}
	a := NewAdapter(client, s, nil, Config{})
	m := &Message{Chat: Chat{ID: 1, Type: "private"}}
	for page := 0; page < 2; page++ {
		if err := a.historyPage(ctx, m, u, ticket, page, 0); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(capture.bodies)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal("service data exposed", string(raw))
	}
	if err := a.historyFile(ctx, m, u, ticket, 1); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("service file exposed", err)
	}
	u.ID = 3
	if err := a.historyFile(ctx, m, u, ticket, 2); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("other client authorized", err)
	}
}
func TestHistoryLocalFileVerifiesHashAndRoot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "attachment.bin")
	data := []byte("saved file")
	os.WriteFile(path, data, 0600)
	hash := sha256.Sum256(data)
	m := domain.Message{ArchivePath: path, SHA256: hex.EncodeToString(hash[:])}
	if _, ok := verifiedHistoryFile(root, m); !ok {
		t.Fatal("valid file refused")
	}
	os.WriteFile(path, []byte("corrupt"), 0600)
	if _, ok := verifiedHistoryFile(root, m); ok {
		t.Fatal("corrupt file issued")
	}
	if _, ok := verifiedHistoryFile(t.TempDir(), m); ok {
		t.Fatal("outside root accepted")
	}
}
func TestConfirmedLongPartsSurviveWorkerRestart(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx := context.Background()
	svc := tickets.New(store)
	ticket, err := svc.Create(ctx, 1, domain.IT, "Problem")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := []string{}
	failed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		text, _ := p["text"].(string)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, text)
		if len(calls) == 2 && !failed {
			failed = true
			w.Write([]byte(`{"ok":false,"error_code":500,"description":"temporary"}`))
			return
		}
		w.Write([]byte(fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, len(calls))))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	message := domain.Message{ID: 44, AuthorID: 2, Direction: "outbound", Text: strings.Repeat("😀", 3000)}
	raw, _ := json.Marshal(message)
	op := domain.Operation{ID: 44, TicketID: ticket.ID, Kind: "client_message", Payload: raw}
	cfg := Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}}
	worker := NewWorker(client, store, cfg)
	worker.interval = 0
	if err = worker.Handle(ctx, op); err == nil {
		t.Fatal("expected partial failure")
	}
	worker = NewWorker(client, store, cfg)
	worker.interval = 0
	if err = worker.Handle(ctx, op); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 || calls[0] == calls[2] {
		t.Fatal("confirmed part repeated", len(calls))
	}
	if calls[0]+calls[2] != message.Text {
		t.Fatal("long answer lost")
	}
}
func TestPostgresFirstAndFinalDeliveryNotifications(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx := context.Background()
	svc := tickets.New(store)
	ticket, err := svc.Create(ctx, 1, domain.IT, "Problem")
	if err != nil {
		t.Fatal(err)
	}
	// Drain unrelated operations without network; this test targets the real lease counter.
	for {
		op, e := store.NextOperation(ctx)
		if errors.Is(e, domain.ErrNotFound) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if e = store.FinishOperation(ctx, op.ID); e != nil {
			t.Fatal(e)
		}
	}
	raw, _ := json.Marshal(domain.Message{ID: 17, AuthorID: 2, Text: "answer", Direction: "outbound"})
	if err = store.Transact(ctx, func(tx domain.Tx) error {
		return tx.Enqueue(ctx, domain.Operation{Kind: "client_message", TicketID: ticket.ID, Payload: raw})
	}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clientAttempts := 0
	notices := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		defer mu.Unlock()
		if p["chat_id"] == float64(1) {
			clientAttempts++
			code := 500
			if clientAttempts > 1 {
				code = 403
			}
			fmt.Fprintf(w, `{"ok":false,"error_code":%d,"description":"delivery failure"}`, code)
			return
		}
		text, _ := p["text"].(string)
		notices = append(notices, text)
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100}}, AdminIDs: []int64{99}})
	worker.interval = 0
	deadline, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	err = worker.Run(deadline)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	first, final := 0, 0
	for _, text := range notices {
		if strings.Contains(text, "пока не доставлен") {
			first++
		}
		if strings.Contains(text, "не доставлен клиенту. Администратору") {
			final++
		}
	}
	if first != 1 || final != 1 || clientAttempts != 2 {
		t.Fatalf("first=%d final=%d attempts=%d notices=%v", first, final, clientAttempts, notices)
	}
}

func TestPermanentUpdateDoesNotStopPollingOrOtherClients(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendDocument") {
			w.Write([]byte(`{"ok":false,"error_code":403,"description":"blocked"}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			polls++
			if polls == 1 {
				w.Write([]byte(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":1,"from":{"id":99},"chat":{"id":99,"type":"private"},"text":"/report"}},{"update_id":2,"message":{"message_id":2,"from":{"id":99},"chat":{"id":99,"type":"private"},"text":"/report"}},{"update_id":3,"message":{"message_id":3,"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"/new"}}]}`))
				return
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				d, e := store.LoadDraft(context.Background(), 1)
				pending, pendingErr := store.PendingUpdates(context.Background(), 100)
				if e == nil && d.Step == "department" && pendingErr == nil && len(pending) == 0 {
					break
				}
				select {
				case <-deadline.C:
					t.Error("independent update was not processed")
					cancel()
					return
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
			cancel()
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	adapter := NewAdapter(client, store, tickets.New(store), Config{AdminIDs: []int64{99}})
	if err := adapter.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if polls != 2 {
		t.Fatal("polling stopped", polls)
	}
	d, err := store.LoadDraft(context.Background(), 1)
	if err != nil || d.Step != "department" {
		t.Fatal("independent client blocked", d, err)
	}
	pending, err := store.PendingUpdates(context.Background(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatal("dependent update escaped quarantine", pending, err)
	}
	last, err := store.LastUpdateID(context.Background())
	if err != nil || last != 3 {
		t.Fatal("inbox not durable", last, err)
	}
}

type outageStore struct {
	domain.Store
	retryCalls int
}

func (s *outageStore) User(context.Context, int64) (domain.User, error) {
	return domain.User{}, errors.New("database unavailable")
}
func (s *outageStore) Ping(context.Context) error { return errors.New("database unavailable") }
func (s *outageStore) RetryUpdate(context.Context, int64, string, time.Duration, bool) error {
	s.retryCalls++
	return nil
}
func TestDatabaseOutageNeverQuarantinesBusinessActions(t *testing.T) {
	store := &outageStore{}
	adapter := NewAdapter(nil, store, nil, Config{})
	raw := json.RawMessage(`{"message":{"from":{"id":1},"chat":{"id":1,"type":"private"},"text":"/new"}}`)
	if err := adapter.processUpdate(context.Background(), domain.StoredUpdate{ID: 1, Payload: raw, Attempts: 4}); err == nil || store.retryCalls != 0 {
		t.Fatal("database outage consumed retry", err)
	}
}

type historyListStore struct {
	*adapterStore
	tickets []domain.Ticket
}

func (s *historyListStore) Tickets(context.Context) ([]domain.Ticket, error) { return s.tickets, nil }
func TestTicketPagesKeepStableIDsAndRecheckRights(t *testing.T) {
	client, capture := capturedClient()
	ctx := context.Background()
	u := domain.User{ID: 1, Role: "client", Approved: true, CompanyID: 10}
	s := &historyListStore{adapterStore: &adapterStore{user: u}}
	for id := int64(1); id <= 23; id++ {
		s.tickets = append(s.tickets, domain.Ticket{ID: id, ClientID: 1, CompanyID: 10, Department: domain.IT})
	}
	s.tickets = append(s.tickets, domain.Ticket{ID: 50, ClientID: 3, CompanyID: 10, Department: domain.IT})
	a := NewAdapter(client, s, nil, Config{})
	m := &Message{Chat: Chat{ID: 1, Type: "private"}}
	seen := map[int64]bool{}
	for page := 0; page < 3; page++ {
		if err := a.ticketListPage(ctx, m, u, page, 50); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(capture.bodies[len(capture.bodies)-1]["reply_markup"])
		var markup Markup
		json.Unmarshal(raw, &markup)
		for _, row := range markup.Buttons {
			for _, button := range row {
				if strings.HasPrefix(button.Data, "ticket:") {
					var id int64
					fmt.Sscanf(button.Data, "ticket:%d", &id)
					if seen[id] || id == 50 {
						t.Fatal("duplicate or foreign ticket", id)
					}
					seen[id] = true
				}
			}
		}
	}
	if len(seen) != 23 {
		t.Fatal("missing tickets", len(seen))
	}
	s.user.Approved = false
	if err := a.callback(ctx, &Callback{From: Person{ID: 1}, Message: m, Data: "tickets:1:50"}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("revoked user read next page", err)
	}
}

func TestTransferUsesLatestCycleAndKeepsFilePrivate(t *testing.T) {
	client, capture := capturedClient()
	ctx := context.Background()
	s := &adapterStore{ticket: domain.Ticket{ID: 7, Department: domain.OneC, TopicChatID: -100, TopicID: 12}, events: []domain.Event{{ID: 1, Kind: "transferred", Data: json.RawMessage(`{"from":"it","to":"1c","reason":"old","message_id":1}`)}, {ID: 2, Kind: "transferred", Data: json.RawMessage(`{"from":"1c","to":"it","reason":"middle"}`)}, {ID: 3, Kind: "transferred", Data: json.RawMessage(`{"from":"it","to":"1c","reason":"-","message_id":9}`)}}}
	worker := NewWorker(client, s, Config{})
	worker.interval = 0
	if err := worker.ensureTransferContext(ctx, s.ticket); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(capture.bodies)
	if !strings.Contains(string(raw), "Без комментария") || !strings.Contains(string(raw), "historyfile:7:9") || strings.Contains(string(raw), "old") {
		t.Fatal("wrong transfer context", string(raw))
	}
	s.events = append(s.events, domain.Event{ID: 4, Kind: "reopened"})
	_, exists, err := worker.transferContext(ctx, s.ticket)
	if err != nil || exists {
		t.Fatal("old transfer leaked into reopened cycle", err)
	}
}

func TestDraftFileReplacementRestartSubmitAndStaleScreens(t *testing.T) {
	store := reliabilityDatabase(t)
	ctx := context.Background()
	svc := tickets.New(store)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"result":{"message_id":100}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	a := NewAdapter(client, store, svc, Config{})
	m := &Message{ID: 1, From: Person{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Text: "/new"}
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	d, err := store.LoadDraft(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	departmentToken := d.Generation
	callback := func(data string) {
		t.Helper()
		if err := a.callback(ctx, &Callback{ID: "callback", From: m.From, Message: &Message{ID: 100, Chat: m.Chat}, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	callback("new:it:" + d.Generation)
	m.ID = 2
	m.Text = ""
	m.Caption = "File description"
	m.Document = &File{ID: "draft-file", Name: "draft.txt"}
	if err = a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	d, err = store.LoadDraft(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldConfirm := d.Generation
	m.ID = 3
	m.Text = "Replacement description"
	m.Caption = ""
	m.Document = nil
	if err = a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	d, err = store.LoadDraft(ctx, 1)
	if err != nil || len(d.Attachments) != 1 || d.Attachments[0].FileID != "draft-file" {
		t.Fatal("replacement lost file", d, err)
	}
	a = NewAdapter(client, store, svc, Config{})
	if err = a.resumeDraft(ctx, m); err != nil {
		t.Fatal(err)
	}
	// A stale screen is rejected now, so the payloads carrying the previous
	// generation must surface ErrConflict while the current game continues.
	stale := func(data string) {
		t.Helper()
		if err := a.callback(ctx, &Callback{ID: "callback", From: m.From, Message: &Message{ID: 100, Chat: m.Chat}, Data: data}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("stale screen not rejected: %v (%s)", err, data)
		}
	}
	current := func(data string) {
		t.Helper()
		if err := a.callback(ctx, &Callback{ID: "callback", From: m.From, Message: &Message{ID: 100, Chat: m.Chat}, Data: data}); err != nil {
			t.Fatalf("current screen rejected: %v (%s)", err, data)
		}
	}
	stale("submit:" + oldConfirm)
	stale("new:1c:" + departmentToken)
	stale("cancel:" + oldConfirm)
	current("submit:" + d.Generation)
	stale("submit:" + d.Generation)
	active, err := store.ActiveTicket(ctx, 1)
	if err != nil || active.Description != "File description\n\nReplacement description" {
		t.Fatal(active, err)
	}
	messages, err := store.Messages(ctx, active.ID)
	if err != nil || len(messages) != 1 || messages[0].Text != "File description" {
		t.Fatal("draft attachment not committed exactly once", messages, err)
	}
	if err = svc.Cancel(ctx, 1, active.ID, ""); err != nil {
		t.Fatal(err)
	}
	m.ID = 4
	m.Text = "/new"
	if err = a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	newDraft, err := store.LoadDraft(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"submit:" + d.Generation, "cancel:" + d.Generation, "new:it:" + departmentToken, "submit", "cancel"} {
		stale(payload)
	}
	final, err := store.LoadDraft(ctx, 1)
	if err != nil || final.Generation != newDraft.Generation || final.Step != "department" {
		t.Fatal("old submitted form changed new one", final, err)
	}
}

func TestQueuedCallbackScreenUsesHumanActorAndValidPrivatePayload(t *testing.T) {
	client, capture := capturedClient()
	ctx := context.Background()
	s := &adapterStore{}
	a := NewAdapter(client, s, nil, Config{})
	a.activeUpdate = 10
	a.activeActor = 1
	m := &Message{From: Person{ID: 999, IsBot: true}, Chat: Chat{ID: 1, Type: "private"}}
	if err := a.queueScreen(ctx, m, m.From.ID, "", "History page", nil); err != nil {
		t.Fatal(err)
	}
	if len(s.operations) != 1 {
		t.Fatal("screen not queued")
	}
	var p struct {
		UserID int64 `json:"user_id"`
	}
	if err := json.Unmarshal(s.operations[0].Payload, &p); err != nil || p.UserID != 1 {
		t.Fatal("bot used as dependency actor", p, err)
	}
	worker := NewWorker(client, s, Config{})
	worker.interval = 0
	if err := worker.uiScreen(ctx, s.operations[0]); err != nil {
		t.Fatal(err)
	}
	payload := capture.bodies[0]
	if _, exists := payload["reply_markup"]; exists {
		t.Fatal("null markup sent", payload)
	}
	if _, exists := payload["message_thread_id"]; exists {
		t.Fatal("zero topic sent to private chat", payload)
	}
}
