package tickets

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"errors"
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
	ctx := context.Background()
	ticket, _ := s.Create(ctx, 1, domain.IT, "help")
	if _, err := s.Create(ctx, 1, domain.OneC, "second"); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	if err := s.AddMessage(ctx, 2, ticket.ID, domain.Message{Text: "secret"}, false); err != nil {
		t.Fatal(err)
	}
	if f.messages[0].Direction != "internal" || f.tickets[ticket.ID].FirstResponseAt != nil {
		t.Fatal("note leaked")
	}
	if err := s.AddMessage(ctx, 2, ticket.ID, domain.Message{Text: "reply"}, true); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(ctx, 2, ticket.ID, domain.Message{Text: "reply"}, true); err != nil {
		t.Fatal(err)
	}
	if f.ops[len(f.ops)-1].Kind != "client_message" || f.tickets[ticket.ID].FirstResponseAt != nil {
		t.Fatal("public response missing")
	}
	if err := s.SetWaiting(ctx, 2, ticket.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMessage(ctx, 1, ticket.ID, domain.Message{Text: "new details"}, false); err != nil {
		t.Fatal(err)
	}
	if f.tickets[ticket.ID].Status != domain.InProgress {
		t.Fatal("client did not resume")
	}
}
func TestTransferRetainsHistoryAndReopen(t *testing.T) {
	s, f := setup()
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
	var payload map[string]any
	_ = json.Unmarshal(f.ops[before].Payload, &payload)
	if payload["topic_id"] != float64(5) || payload["retire"] != true {
		t.Fatal(payload)
	}
	if err := s.Claim(t.Context(), 3, v.ID); err != nil {
		t.Fatal(err)
	}
	if f.ops[len(f.ops)-3].Kind != "create_topic" {
		t.Fatal("claim did not create target")
	}
	if err := s.Close(t.Context(), 3, v.ID, "fixed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reopen(t.Context(), 1, v.ID); err != nil {
		t.Fatal(err)
	}
	if f.tickets[v.ID].Status != domain.New || f.ops[len(f.ops)-2].Kind != "refresh_card" {
		t.Fatal("reopen not queued")
	}
}

func TestReopenWindowAndForeignClient(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, _ := s.Create(ctx, 1, domain.IT, "help")
	_ = s.Claim(ctx, 2, ticket.ID)
	_ = s.Close(ctx, 2, ticket.ID, "fixed")
	f.users[5] = domain.User{ID: 5, Role: "client", Approved: true, CompanyID: 10}
	if err := s.Reopen(ctx, 5, ticket.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC) }
	if err := s.Reopen(ctx, 1, ticket.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Reopen(ctx, 4, ticket.ID); err != nil {
		t.Fatal(err)
	}
}
func TestEditArchivesAndOnlyClientCorrectionIsDelivered(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, _ := s.Create(ctx, 1, domain.IT, "help")
	_ = s.Claim(ctx, 2, ticket.ID)
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
	if err := s.AddMessage(ctx, 2, ticket.ID, domain.Message{Text: "answer", TelegramChatID: -100, TelegramMessageID: 100}, true); err != nil {
		t.Fatal(err)
	}
	count := len(f.ops)
	if err := s.EditMessage(ctx, 2, ticket.ID, domain.Message{Text: "changed answer", TelegramChatID: -100, TelegramMessageID: 100}); err != nil {
		t.Fatal(err)
	}
	if len(f.ops) != count {
		t.Fatal("outbound edit resent")
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
func TestInternalAttachmentArchival(t *testing.T) {
	s, f := setup()
	ticket, _ := s.Create(t.Context(), 1, domain.IT, "help")
	if err := s.AddMessage(t.Context(), 2, ticket.ID, domain.Message{FileID: "file"}, false); err != nil {
		t.Fatal(err)
	}
	if f.ops[len(f.ops)-1].Kind != "archive_message" {
		t.Fatal("internal file missing archive")
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
	if err := s.Cancel(t.Context(), 2, ticket.ID, "not my role"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
	if err := s.Cancel(t.Context(), 1, ticket.ID, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	v := f.tickets[ticket.ID]
	if v.Status != domain.Cancelled || v.RoutingPending {
		t.Fatal(v)
	}
	if _, err := s.Create(t.Context(), 1, domain.IT, "next"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reopen(t.Context(), 1, ticket.ID); !errors.Is(err, domain.ErrConflict) {
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
