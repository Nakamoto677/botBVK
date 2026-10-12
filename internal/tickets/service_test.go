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
	m, e, o := len(f.messages), len(f.events), len(f.ops)
	err := fn(fakeTx{f})
	if err != nil {
		f.users = users
		f.tickets = tickets
		f.messages = f.messages[:m]
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
	if f.ops[len(f.ops)-1].Kind != "client_message" || f.tickets[ticket.ID].FirstResponseAt == nil {
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
	ctx := context.Background()
	ticket, _ := s.Create(ctx, 1, domain.IT, "help")
	v := f.tickets[ticket.ID]
	v.TopicChatID = -100
	v.TopicID = 5
	f.tickets[v.ID] = v
	if err := s.Transfer(ctx, 2, v.ID, domain.OneC, "wrong team"); err != nil {
		t.Fatal(err)
	}
	op := f.ops[len(f.ops)-2]
	var payload map[string]any
	if err := json.Unmarshal(op.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if op.Kind != "create_topic" || payload["old_topic_id"] != float64(5) {
		t.Fatal(op)
	}
	if err := s.Claim(ctx, 3, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx, 3, v.ID, "fixed"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reopen(ctx, 1, v.ID); err != nil {
		t.Fatal(err)
	}
	if f.tickets[v.ID].Status != domain.New {
		t.Fatal("not reopened")
	}
	if len(f.events) != 5 {
		t.Fatal(len(f.events))
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
