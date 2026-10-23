package tickets

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Embedded Store covers unused read methods. Transact snapshots all mutated data to exercise rollback.
type fakeStore struct {
	domain.Store
	mu          sync.Mutex
	users       map[int64]domain.User
	tickets     map[int64]domain.Ticket
	messages    []domain.Message
	events      []domain.Event
	ops         []domain.Operation
	failEnqueue bool
}
type fakeTx struct{ f *fakeStore }

func (f *fakeStore) Transact(ctx context.Context, fn func(domain.Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	users := map[int64]domain.User{}
	for k, v := range f.users {
		users[k] = v
	}
	tickets := map[int64]domain.Ticket{}
	for k, v := range f.tickets {
		tickets[k] = v
	}
	messages := append([]domain.Message(nil), f.messages...)
	e, o := len(f.events), len(f.ops)
	err := fn(fakeTx{f})
	if err != nil {
		f.users = users
		f.tickets = tickets
		f.messages = messages
		f.events = f.events[:e]
		f.ops = f.ops[:o]
	}
	return err
}
func (tx fakeTx) GetUser(_ context.Context, id int64) (domain.User, error) {
	u, ok := tx.f.users[id]
	if !ok {
		return u, domain.ErrNotFound
	}
	return u, nil
}
func (tx fakeTx) SaveUser(_ context.Context, u domain.User) error { tx.f.users[u.ID] = u; return nil }
func (tx fakeTx) GetTicket(_ context.Context, id int64) (domain.Ticket, error) {
	t, ok := tx.f.tickets[id]
	if !ok {
		return t, domain.ErrNotFound
	}
	return t, nil
}
func (tx fakeTx) ActiveTicket(_ context.Context, id int64) (domain.Ticket, error) {
	for _, t := range tx.f.tickets {
		if t.ClientID == id && t.Status.Active() {
			return t, nil
		}
	}
	return domain.Ticket{}, domain.ErrNotFound
}
func (tx fakeTx) SaveTicket(_ context.Context, t *domain.Ticket) error {
	if t.ID == 0 {
		t.ID = int64(len(tx.f.tickets) + 1)
	}
	t.Version++
	tx.f.tickets[t.ID] = *t
	return nil
}
func (tx fakeTx) AddMessage(_ context.Context, m *domain.Message) error {
	if m.TelegramChatID != 0 && m.TelegramMessageID != 0 {
		for _, old := range tx.f.messages {
			if old.TelegramChatID == m.TelegramChatID && old.TelegramMessageID == m.TelegramMessageID && old.Direction == m.Direction {
				return domain.ErrConflict
			}
		}
	}
	m.ID = int64(len(tx.f.messages) + 1)
	tx.f.messages = append(tx.f.messages, *m)
	return nil
}
func (tx fakeTx) EditMessage(_ context.Context, m *domain.Message) error {
	for i, old := range tx.f.messages {
		if old.TicketID == m.TicketID && old.AuthorID == m.AuthorID && old.TelegramChatID == m.TelegramChatID && old.TelegramMessageID == m.TelegramMessageID {
			m.ID = old.ID
			m.Direction = old.Direction
			m.DeferredDelivery = old.DeferredDelivery
			m.RoutingDepartment = old.RoutingDepartment
			m.CreatedAt = old.CreatedAt
			tx.f.messages[i] = *m
			return nil
		}
	}
	return domain.ErrNotFound
}
func (tx fakeTx) AddEvent(_ context.Context, e domain.Event) error {
	tx.f.events = append(tx.f.events, e)
	return nil
}
func (tx fakeTx) Enqueue(_ context.Context, o domain.Operation) error {
	if tx.f.failEnqueue {
		return errors.New("outbox unavailable")
	}
	tx.f.ops = append(tx.f.ops, o)
	return nil
}
func (tx fakeTx) RequestAccess(context.Context, int64, int64) error { return nil }
func (tx fakeTx) ResolveAccess(context.Context, int64, bool) (domain.AccessRequest, error) {
	return domain.AccessRequest{}, nil
}
func setup() (*Service, *fakeStore) {
	f := &fakeStore{users: map[int64]domain.User{1: {ID: 1, Role: "client", Approved: true, CompanyID: 10}, 2: {ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, 3: {ID: 3, Role: "engineer", Approved: true, Departments: []domain.Department{domain.OneC}}, 4: {ID: 4, Role: "dispatcher", Approved: true}}, tickets: map[int64]domain.Ticket{}}
	s := New(f)
	s.now = func() time.Time { return time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC) }
	return s, f
}

func notificationText(t *testing.T, op domain.Operation) string {
	t.Helper()
	if op.Kind != "notify_client" {
		t.Fatalf("operation kind = %s, want notify_client", op.Kind)
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(op.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.Text
}

func TestClaimNotifiesClientWithEngineerTelegramName(t *testing.T) {
	for _, tt := range []struct {
		name, username, want string
	}{
		{"Анна Иванова", "anna", "Анна Иванова (@anna)"},
		{"Анна Иванова", "@anna", "Анна Иванова (@anna)"},
		{"", "anna", "@anna"},
		{"", "", "служба поддержки"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			s, f := setup()
			user := f.users[2]
			user.Name, user.Username = tt.name, tt.username
			f.users[2] = user
			ticket, err := s.Create(t.Context(), 1, domain.IT, "help")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Claim(t.Context(), 2, ticket.ID); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("Заявка №%d · В работе\nНа связи: %s. Продолжайте переписку в этом чате.", ticket.ID, tt.want)
			if got := notificationText(t, f.ops[len(f.ops)-1]); got != want {
				t.Fatalf("notice = %q, want %q", got, want)
			}
		})
	}
}
func TestClaimConcurrentAndScope(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, err := s.Create(ctx, 1, domain.IT, "Help")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(ctx, 3, ticket.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- s.Claim(ctx, 2, ticket.ID) }()
	}
	success := 0
	for range 2 {
		err := <-errs
		if err == nil {
			success++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 || f.tickets[ticket.ID].AssigneeID != 2 {
		t.Fatal("claim not atomic")
	}
}
func TestRollbackWhenOutboxFails(t *testing.T) {
	s, f := setup()
	f.failEnqueue = true
	if _, err := s.Create(context.Background(), 1, domain.IT, "help"); err == nil {
		t.Fatal("expected failure")
	}
	if len(f.tickets)+len(f.events)+len(f.ops) != 0 {
		t.Fatal("partial transaction")
	}
}
func TestMessagesAndFirstResponse(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	if _, err := s.Create(t.Context(), 1, domain.OneC, "second"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	if err := s.AddMessage(t.Context(), 2, ticket.ID, domain.Message{Text: "secret"}, false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if len(f.messages) != 0 {
		t.Fatal("legacy note path accepted")
	}
	_ = s.Claim(t.Context(), 2, ticket.ID)
	bind(f, ticket.ID, -100, 42)
	f.users[6] = domain.User{ID: 6, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}
	if err := s.AddMessage(t.Context(), 6, ticket.ID, domain.Message{Text: "ordinary reply", TelegramChatID: -100, TelegramTopicID: 42, TelegramMessageID: 123}, true); err != nil {
		t.Fatal(err)
	}
	if f.ops[len(f.ops)-1].Kind != "client_message" || f.messages[0].Direction != "outbound" || f.tickets[ticket.ID].FirstResponseAt != nil {
		t.Fatal("public reply failed")
	}
	if err := s.SetWaiting(t.Context(), 2, ticket.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(t.Context(), 1, ticket.ID, domain.Message{Text: "details"}, false); err != nil {
		t.Fatal(err)
	}
	if f.tickets[ticket.ID].Status != domain.InProgress {
		t.Fatal("client did not resume")
	}
}

func TestTransferRetainsHistoryAndClosedTicketAllowsNew(t *testing.T) {
	s, f := setup()
	engineer := f.users[3]
	engineer.Name = "Мария Петрова"
	engineer.Username = "maria"
	f.users[3] = engineer
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	v := f.tickets[ticket.ID]
	v.RoutingPending = false
	v.TopicChatID = -100
	v.TopicID = 5
	v.QueueMessageID = 55
	v.TopicMessageID = 56
	f.tickets[v.ID] = v
	before := len(f.ops)
	if err := s.Transfer(t.Context(), 2, v.ID, domain.OneC, "wrong team"); err != nil {
		t.Fatal(err)
	}
	got := f.tickets[v.ID]
	if got.Status != domain.New || got.AssigneeID != 0 || got.TopicID != 0 || got.TopicChatID != 0 || got.QueueMessageID != 0 || got.TopicMessageID != 0 || got.RoutingPending {
		t.Fatal(got)
	}
	kinds := []string{"close_topic", "retire_queue_card", "refresh_card", "notify_client"}
	if len(f.ops)-before != len(kinds) {
		t.Fatal("transfer operations", f.ops[before:])
	}
	for i, kind := range kinds {
		if f.ops[before+i].Kind != kind {
			t.Fatal(f.ops[before+i])
		}
	}
	if got, want := notificationText(t, f.ops[before+3]), fmt.Sprintf("Заявка №%d передана в отдел «1С». Специалисты увидят обращение в очереди.", v.ID); got != want {
		t.Fatalf("transfer notice = %q, want %q", got, want)
	}
	var payload map[string]any
	_ = json.Unmarshal(f.ops[before].Payload, &payload)
	if payload["topic_id"] != float64(5) || payload["retire"] != true || payload["to_department"] != string(domain.OneC) {
		t.Fatal(payload)
	}
	if err := s.Claim(t.Context(), 3, v.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := notificationText(t, f.ops[len(f.ops)-1]), fmt.Sprintf("Заявка №%d · В работе\nНа связи: Мария Петрова (@maria). Продолжайте переписку в этом чате.", v.ID); got != want {
		t.Fatalf("claim notice = %q, want %q", got, want)
	}
	if f.ops[len(f.ops)-3].Kind != "create_topic" {
		t.Fatal("claim did not create target")
	}
	if err := s.Close(t.Context(), 3, v.ID, "fixed"); err != nil {
		t.Fatal(err)
	}
	newTicket, err := s.Create(t.Context(), 1, domain.IT, "new problem")
	if err != nil || newTicket.ID == v.ID || f.tickets[v.ID].Status != domain.Closed {
		t.Fatal("closed ticket was reused", err, newTicket)
	}
}

func TestEditArchivesAndOnlyClientCorrectionIsDelivered(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, _ := s.Create(ctx, 1, domain.IT, "help")
	_ = s.Claim(ctx, 2, ticket.ID)
	bind(f, ticket.ID, -100, 42)
	if err := s.AddMessage(ctx, 1, ticket.ID, domain.Message{Text: "original", TelegramChatID: 1, TelegramMessageID: 42}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.EditMessage(ctx, 1, ticket.ID, domain.Message{Text: "corrected", TelegramChatID: 1, TelegramMessageID: 42}); err != nil {
		t.Fatal(err)
	}
	if f.ops[len(f.ops)-1].Kind != "team_message" || f.messages[0].Text != "corrected" {
		t.Fatal("missing correction")
	}
	if err := s.EditMessage(ctx, 2, ticket.ID, domain.Message{Text: "forged", TelegramChatID: 1, TelegramMessageID: 42}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.AddMessage(ctx, 2, ticket.ID, domain.Message{Text: "answer", TelegramChatID: -100, TelegramTopicID: 42, TelegramMessageID: 100}, true); err != nil {
		t.Fatal(err)
	}
	count := len(f.ops)
	if err := s.EditMessage(ctx, 2, ticket.ID, domain.Message{Text: "changed answer", TelegramChatID: -100, TelegramMessageID: 100}); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != count+1 || f.ops[len(f.ops)-1].Kind != "client_message" {
		t.Fatal("public correction missing")
	}
	_ = s.Close(ctx, 2, ticket.ID, "fixed")
	count = len(f.ops)
	if err := s.EditMessage(ctx, 1, ticket.ID, domain.Message{Text: "closed revision", TelegramChatID: 1, TelegramMessageID: 42}); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != count {
		t.Fatal("closed edit delivered")
	}
}
func TestLegacyInternalEditsStayArchivalAndNewNotesRejected(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	bind(f, ticket.ID, -100, 42)
	f.messages = append(f.messages, domain.Message{ID: 1, TicketID: ticket.ID, AuthorID: 2, Direction: "internal", Text: "old private note", TelegramChatID: -100, TelegramMessageID: 55, CreatedAt: s.now()})
	before := len(f.ops)
	if err := s.EditMessage(t.Context(), 2, ticket.ID, domain.Message{Text: "archival revision", TelegramChatID: -100, TelegramMessageID: 55}); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != before || f.messages[0].Direction != "internal" {
		t.Fatal("old note relayed")
	}
	if err := s.AddMessage(t.Context(), 2, ticket.ID, domain.Message{FileID: "file", TelegramChatID: -100, TelegramTopicID: 42}, false); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if len(f.messages) != 1 {
		t.Fatal("new private file accepted")
	}
}

func TestClaimRoutingPendingBlocksTransfer(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	if ticket.RoutingPending {
		t.Fatal("unclaimed requires no topology")
	}
	if err := s.Claim(t.Context(), 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if !f.tickets[ticket.ID].RoutingPending {
		t.Fatal("claim routing missing")
	}
	if err := s.Transfer(t.Context(), 2, ticket.ID, domain.OneC, "wrong"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
}

func TestCancellationAuthorizationAndAllowsNewTicket(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	if err := s.Cancel(t.Context(), 2, ticket.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.Cancel(t.Context(), 1, ticket.ID); err != nil {
		t.Fatal(err)
	}
	v := f.tickets[ticket.ID]
	if v.Status != domain.Cancelled || v.RoutingPending {
		t.Fatal(v)
	}
	if _, err := s.Create(t.Context(), 1, domain.IT, "next"); err != nil {
		t.Fatal(err)
	}

}
func TestAssignScopeOwnerAndDispatcher(t *testing.T) {
	s, f := setup()
	f.users[6] = domain.User{ID: 6, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	if err := s.Assign(t.Context(), 2, ticket.ID, 6); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.Assign(t.Context(), 4, ticket.ID, 2); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Claim(t.Context(), 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Assign(t.Context(), 4, ticket.ID, 3); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.Assign(t.Context(), 2, ticket.ID, 6); err != nil {
		t.Fatal(err)
	}
	if f.tickets[ticket.ID].AssigneeID != 6 {
		t.Fatal("owner cannot reassign")
	}
	if err := s.Assign(t.Context(), 2, ticket.ID, 2); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.Assign(t.Context(), 6, ticket.ID, 6); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
}
func TestQueuedMessagesAndEditsDeferWithoutBlockingClaim(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	if err := s.AddMessage(t.Context(), 1, ticket.ID, domain.Message{Text: "before", TelegramChatID: 1, TelegramMessageID: 42, FileID: "file"}, false); err != nil {
		t.Fatal(err)
	}
	if !f.messages[0].DeferredDelivery || f.messages[0].RoutingDepartment != domain.IT {
		t.Fatal("new message not deferred")
	}
	if err := s.EditMessage(t.Context(), 1, ticket.ID, domain.Message{Text: "corrected", TelegramChatID: 1, TelegramMessageID: 42}); err != nil {
		t.Fatal(err)
	}
	if !f.messages[0].DeferredDelivery || f.messages[0].Text != "corrected" {
		t.Fatal("deferred edit lost")
	}
	for _, op := range f.ops {
		if op.Kind == "team_message" || op.Kind == "create_topic" {
			t.Fatal("unclaimed traffic blocks claim", op)
		}
	}
	if err := s.Claim(t.Context(), 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(t.Context(), 1, ticket.ID, domain.Message{Text: "active", TelegramChatID: 1, TelegramMessageID: 43}, false); err != nil {
		t.Fatal(err)
	}
	if f.messages[1].DeferredDelivery || f.ops[len(f.ops)-1].Kind != "team_message" {
		t.Fatal("active routing not enabled")
	}
}

func TestCreateQueuesWithoutTopicAndClaimCreatesExactlyOnce(t *testing.T) {
	s, f := setup()
	ticket, err := s.Create(t.Context(), 1, domain.IT, "help")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.RoutingPending || ticket.TopicID != 0 || len(f.ops) != 2 || f.ops[0].Kind != "refresh_card" || f.ops[1].Kind != "notify_client" {
		t.Fatal(f.ops)
	}
	if _, err := s.Create(t.Context(), 1, domain.IT, "duplicate"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- s.Claim(t.Context(), 2, ticket.ID) }()
	}
	success := 0
	for range 2 {
		err := <-errs
		if err == nil {
			success++
		} else if !errors.Is(err, domain.ErrConflict) {
			t.Fatal(err)
		}
	}
	count := 0
	for _, op := range f.ops {
		if op.Kind == "create_topic" {
			count++
		}
	}
	if success != 1 || count != 1 {
		t.Fatal(success, count)
	}
}
func TestTransferUnclaimedQueueNeverCreatesOrClosesTopic(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	v := f.tickets[ticket.ID]
	v.QueueMessageID = 44
	f.tickets[v.ID] = v
	before := len(f.ops)
	if err := s.Transfer(t.Context(), 2, v.ID, domain.OneC, "wrong team"); err != nil {
		t.Fatal(err)
	}
	for _, op := range f.ops[before:] {
		if op.Kind == "create_topic" || op.Kind == "reopen_topic" || op.Kind == "close_topic" {
			t.Fatal("unexpected topology", op)
		}
	}
	var payload map[string]any
	_ = json.Unmarshal(f.ops[before].Payload, &payload)
	if f.ops[before].Kind != "retire_queue_card" || payload["from_department"] != "it" || payload["message_id"] != float64(44) {
		t.Fatal(payload)
	}
	if err := s.Transfer(t.Context(), 3, v.ID, domain.Field, "second validation"); err != nil {
		t.Fatal(err)
	}
}
func TestAssignQueuedTicketRejectedWithoutCreatingTopic(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	before := len(f.ops)
	if err := s.Assign(t.Context(), 4, ticket.ID, 2); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	if f.tickets[ticket.ID].RoutingPending || f.tickets[ticket.ID].Status != domain.New || len(f.ops) != before {
		t.Fatal("assign took queued request")
	}
	for _, op := range f.ops {
		if op.Kind == "create_topic" || op.Kind == "reopen_topic" {
			t.Fatal("assign provisioned topic")
		}
	}
}
func TestCloseAttachmentPreservesSourceAndArchivesAtomically(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	bind(f, ticket.ID, -100, 42)
	before := len(f.ops)
	msg := domain.Message{ID: 99, AuthorID: 999, TicketID: 999, Text: "result screenshot", FileID: "file", MediaType: "photo", TelegramChatID: -100, TelegramMessageID: 123, ArchivePath: "spoof", SHA256: "spoof", DeferredDelivery: true}
	if err := s.CloseWithMessage(t.Context(), 2, ticket.ID, "fixed", msg); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != 1 {
		t.Fatal(f.messages)
	}
	got := f.messages[0]
	if got.ID == 99 || got.AuthorID != 2 || got.TicketID != ticket.ID || got.Direction != "service" || got.TelegramChatID != -100 || got.TelegramMessageID != 123 || got.FileID != "file" || got.Text != msg.Text || got.ArchivePath != "" || got.SHA256 != "" || got.DeferredDelivery || !got.CreatedAt.Equal(s.now()) {
		t.Fatal(got)
	}
	if f.ops[before].Kind != "archive_message" || f.ops[before+1].Kind != "close_topic" || f.tickets[ticket.ID].Status != domain.Closed {
		t.Fatal("action order")
	}
}
func TestActionAttachmentRollbackOnForbiddenOrOutboxFailure(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	msg := domain.Message{FileID: "file", TelegramChatID: -100, TelegramMessageID: 123}
	beforeEvents, beforeOps := len(f.events), len(f.ops)
	if err := s.CloseWithMessage(t.Context(), 3, ticket.ID, "fixed", msg); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if len(f.messages) != 0 || len(f.events) != beforeEvents || len(f.ops) != beforeOps {
		t.Fatal("forbidden file accepted")
	}
	f.failEnqueue = true
	if err := s.CloseWithMessage(t.Context(), 2, ticket.ID, "fixed", msg); err == nil {
		t.Fatal("expected outbox error")
	}
	if len(f.messages) != 0 || len(f.events) != beforeEvents || len(f.ops) != beforeOps || f.tickets[ticket.ID].Status != domain.InProgress {
		t.Fatal("partial close")
	}
	f.failEnqueue = false
	v := f.tickets[ticket.ID]
	v.RoutingPending = false
	f.tickets[v.ID] = v
	f.failEnqueue = true
	if err := s.TransferWithMessage(t.Context(), 2, v.ID, domain.OneC, "wrong team", msg); err == nil {
		t.Fatal("expected transfer failure")
	}
	if len(f.messages) != 0 || len(f.events) != beforeEvents || len(f.ops) != beforeOps || f.tickets[ticket.ID].Department != domain.IT {
		t.Fatal("partial transfer")
	}
}
func TestTransferAttachmentAndDuplicateSourceRollback(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	msg := domain.Message{Text: "diagnostics", FileID: "file", TelegramChatID: -100, TelegramMessageID: 123}
	if err := s.TransferWithMessage(t.Context(), 2, ticket.ID, domain.OneC, "wrong team", msg); err != nil {
		t.Fatal(err)
	}
	if f.tickets[ticket.ID].Department != domain.OneC || f.messages[0].AuthorID != 2 || f.messages[0].FileID != "file" {
		t.Fatal("transfer lost attachment")
	}
	beforeEvents, beforeOps := len(f.events), len(f.ops)
	if err := s.TransferWithMessage(t.Context(), 3, ticket.ID, domain.IT, "return", msg); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	if len(f.messages) != 1 || len(f.events) != beforeEvents || len(f.ops) != beforeOps || f.tickets[ticket.ID].Department != domain.OneC {
		t.Fatal("duplicate source committed transfer")
	}
	if err := s.TransferWithMessage(t.Context(), 3, ticket.ID, domain.IT, "return", domain.Message{Text: "plain reason"}); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != 1 {
		t.Fatal("plain reason duplicated row")
	}
}

func bind(f *fakeStore, ticketID, chatID, topicID int64) {
	t := f.tickets[ticketID]
	t.TopicChatID = chatID
	t.TopicID = topicID
	t.RoutingPending = false
	f.tickets[ticketID] = t
}
func TestCancellationWithoutReasonIsIdempotentAndScoped(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	bind(f, ticket.ID, -100, 42)
	if err := s.Cancel(t.Context(), 1, ticket.ID); err != nil {
		t.Fatal(err)
	}
	v := f.tickets[ticket.ID]
	if v.Status != domain.Cancelled || v.Resolution != "" || v.ClosedBy != 1 {
		t.Fatal(v)
	}
	events, ops, version := len(f.events), len(f.ops), v.Version
	if err := s.Cancel(t.Context(), 1, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != events || len(f.ops) != ops || f.tickets[ticket.ID].Version != version {
		t.Fatal("repeated cancel duplicated action")
	}
	f.users[5] = domain.User{ID: 5, Role: "client", Approved: true, CompanyID: 10}
	if err := s.Cancel(t.Context(), 5, ticket.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range f.events {
		kinds[e.Kind]++
	}
	if kinds["cancelled"] != 1 || kinds["closed"] != 0 || kinds["completion_cleanup_requested"] != 0 {
		t.Fatal(kinds)
	}
}
func TestPublicRelayRequiresCurrentSourceTopicForEveryEngineer(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	bind(f, ticket.ID, -100, 42)
	f.users[6] = domain.User{ID: 6, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}
	cases := []struct{ actor, chat, topic int64 }{{6, -100, 41}, {6, -101, 42}, {6, -100, 0}, {3, -100, 42}}
	for _, c := range cases {
		if err := s.AddMessage(t.Context(), c.actor, ticket.ID, domain.Message{Text: "wrong source", TelegramChatID: c.chat, TelegramTopicID: c.topic}, true); !errors.Is(err, domain.ErrForbidden) {
			t.Fatal(c, err)
		}
	}
	if len(f.messages) != 0 {
		t.Fatal("foreign source archived as public")
	}
	if err := s.AddMessage(t.Context(), 6, ticket.ID, domain.Message{Text: "valid colleague", TelegramChatID: -100, TelegramTopicID: 42}, true); err != nil {
		t.Fatal(err)
	}
	if f.messages[0].AuthorID != 6 || f.messages[0].Direction != "outbound" || f.messages[0].TelegramTopicID != 42 {
		t.Fatal(f.messages[0])
	}
	f.users[6] = domain.User{ID: 6, Role: "engineer", Approved: false, Departments: []domain.Department{domain.IT}}
	if err := s.AddMessage(t.Context(), 6, ticket.ID, domain.Message{Text: "revoked", TelegramChatID: -100, TelegramTopicID: 42}, true); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
}
func TestFailedCompletionRollbackLeavesTicketActive(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	_ = s.Claim(t.Context(), 2, ticket.ID)
	bind(f, ticket.ID, -100, 42)
	events, ops := len(f.events), len(f.ops)
	f.failEnqueue = true
	if err := s.Cancel(t.Context(), 1, ticket.ID); err == nil {
		t.Fatal("expected failure")
	}
	if f.tickets[ticket.ID].Status != domain.InProgress || len(f.events) != events || len(f.ops) != ops {
		t.Fatal("partial completion intent")
	}
}

func (f *fakeStore) Ticket(_ context.Context, id int64) (domain.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ticket, ok := f.tickets[id]
	if !ok {
		return ticket, domain.ErrNotFound
	}
	return ticket, nil
}

type lockOrderStore struct {
	*fakeStore
	t *testing.T
}
type lockOrderTx struct {
	domain.Tx
	t       *testing.T
	locked  map[int64]bool
	ticket  bool
	largest int64
}

func (s *lockOrderStore) Transact(ctx context.Context, fn func(domain.Tx) error) error {
	return s.fakeStore.Transact(ctx, func(tx domain.Tx) error { return fn(&lockOrderTx{Tx: tx, t: s.t, locked: map[int64]bool{}}) })
}
func (tx *lockOrderTx) GetUser(ctx context.Context, id int64) (domain.User, error) {
	if !tx.locked[id] {
		if tx.ticket {
			tx.t.Errorf("user %d locked after ticket", id)
		}
		if id < tx.largest {
			tx.t.Errorf("user locks out of order: %d after %d", id, tx.largest)
		}
		tx.largest = id
		tx.locked[id] = true
	}
	return tx.Tx.GetUser(ctx, id)
}
func (tx *lockOrderTx) GetTicket(ctx context.Context, id int64) (domain.Ticket, error) {
	tx.ticket = true
	return tx.Tx.GetTicket(ctx, id)
}
func TestAssignLocksUsersBeforeTicket(t *testing.T) {
	s, f := setup()
	ctx := t.Context()
	ticket, err := s.Create(ctx, 1, domain.IT, "problem")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	u := f.users[3]
	u.Departments = []domain.Department{domain.IT}
	f.users[3] = u
	s.store = &lockOrderStore{fakeStore: f, t: t}
	if err = s.Assign(ctx, 4, ticket.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(ctx, 4, ticket.ID, "fixed"); err != nil {
		t.Fatal(err)
	}

}
