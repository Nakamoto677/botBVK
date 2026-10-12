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

type adapterStore struct {
	domain.Store
	user           domain.User
	ticket         domain.Ticket
	events         []domain.Event
	storedMessages []domain.Message
}

func (s *adapterStore) User(context.Context, int64) (domain.User, error) { return s.user, nil }
func (s *adapterStore) TopicTicket(context.Context, int64, int64) (domain.Ticket, error) {
	return s.ticket, nil
}
func (s *adapterStore) Ticket(context.Context, int64) (domain.Ticket, error) { return s.ticket, nil }
func (s *adapterStore) Messages(context.Context, int64) ([]domain.Message, error) {
	return s.storedMessages, nil
}
func (s *adapterStore) Events(context.Context, int64) ([]domain.Event, error) { return s.events, nil }

type adapterService struct {
	Service
	messages []domain.Message
	public   []bool
}

func (s *adapterService) AddMessage(_ context.Context, _ int64, _ int64, m domain.Message, public bool) error {
	s.messages = append(s.messages, m)
	s.public = append(s.public, public)
	return nil
}
func TestOrdinaryReplyRemainsInternal(t *testing.T) {
	store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, Status: domain.InProgress}}
	service := &adapterService{}
	a := NewAdapter(nil, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	err := a.Handle(context.Background(), Update{Message: &Message{ID: 20, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "Внутренняя заметка", Reply: &Message{ID: 19}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(service.public) != 1 || service.public[0] {
		t.Fatal("ordinary reply escaped internally")
	}
}
func TestPublicReplyRequiresDeliveryMappingAndAssignee(t *testing.T) {
	store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, Status: domain.InProgress}}
	service := &adapterService{}
	a := NewAdapter(nil, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	update := Update{Message: &Message{ID: 20, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "/reply Решено", Reply: &Message{ID: 19}}}
	if err := a.Handle(context.Background(), update); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("unmapped reply accepted: %v", err)
	}
	data, _ := json.Marshal(map[string]any{"telegram_chat_id": -100, "telegram_message_id": 19, "direction": "inbound"})
	store.events = []domain.Event{{Kind: "message_delivered", Data: data}}
	if err := a.Handle(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 1 || service.messages[0].Text != "Решено" || !service.public[0] {
		t.Fatal("public message wasn't normalized")
	}
	store.ticket.AssigneeID = 3
	if err := a.Handle(context.Background(), update); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("unassigned engineer accepted: %v", err)
	}
}
func TestAnonymousSenderRejected(t *testing.T) {
	a := NewAdapter(nil, nil, nil, Config{})
	err := a.Handle(context.Background(), Update{Message: &Message{From: Person{ID: 2}, SenderChat: &Chat{ID: -100}, Chat: Chat{ID: -100, Type: "supergroup"}}})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("anonymous accepted: %v", err)
	}
}
func TestCardLengthBounded(t *testing.T) {
	ticket := domain.Ticket{Description: string(make([]rune, 5000))}
	if len([]rune(card(ticket))) > 4096 {
		t.Fatal("card exceeds Telegram text limit")
	}
}

func TestHistoryRequiresOwnAccountAndCompany(t *testing.T) {
	ticket := domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT}
	for _, tc := range []struct {
		u    domain.User
		want bool
	}{{domain.User{ID: 1, CompanyID: 10, Role: "client", Approved: true}, true}, {domain.User{ID: 2, CompanyID: 10, Role: "client", Approved: true}, false}, {domain.User{ID: 1, CompanyID: 11, Role: "client", Approved: true}, false}, {domain.User{ID: 1, CompanyID: 10, Role: "client", Approved: false}, false}, {domain.User{ID: 3, Role: "engineer", Approved: true, Departments: []domain.Department{domain.OneC}}, false}} {
		if got := historyAuthorized(tc.u, ticket); got != tc.want {
			t.Errorf("%#v: %v", tc.u, got)
		}
	}
}
func TestHistoryShowsInitialDescriptionAndRevisionsWithoutInternalNotes(t *testing.T) {
	store := &adapterStore{user: domain.User{ID: 1, CompanyID: 10, Role: "client", Approved: true}, ticket: domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT, Description: "Initial problem"}, storedMessages: []domain.Message{{AuthorID: 1, Direction: "inbound", Text: "Updated detail", Revisions: []domain.MessageRevision{{Text: "Original detail"}}}, {Direction: "internal", Text: "SECRET NOTE"}}}
	var transcript strings.Builder
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		transcript.WriteString(p["text"].(string))
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	client := NewClient("test")
	client.BaseURL = server.URL
	a := NewAdapter(client, store, nil, Config{})
	if err := a.Handle(context.Background(), Update{Message: &Message{From: Person{ID: 1}, Chat: Chat{ID: 1, Type: "private"}, Text: "/history 7"}}); err != nil {
		t.Fatal(err)
	}
	text := transcript.String()
	if !strings.Contains(text, "Initial problem") || !strings.Contains(text, "Original detail") || strings.Contains(text, "SECRET NOTE") {
		t.Fatal("wrong transcript", text)
	}
}
func TestUntrustedGroupRejectedBeforeStoreRead(t *testing.T) {
	a := NewAdapter(nil, nil, nil, Config{DispatcherChatID: -100})
	err := a.Handle(context.Background(), Update{Message: &Message{From: Person{ID: 3}, Chat: Chat{ID: -200, Type: "supergroup"}, Text: "/report"}})
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatal(err)
	}
}
func TestHistoryCannotCrossDepartmentGroups(t *testing.T) {
	a := NewAdapter(nil, nil, nil, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100}, domain.OneC: {ChatID: -200}}})
	u := domain.User{ID: 3, Role: "admin", Approved: true}
	ticket := domain.Ticket{Department: domain.IT}
	if a.historyDestination(&Message{Chat: Chat{ID: -200, Type: "supergroup"}}, u, ticket) {
		t.Fatal("IT history allowed in 1C group")
	}
}
