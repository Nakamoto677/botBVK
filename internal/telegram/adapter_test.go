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
	operations     []domain.Operation
}

func (s *adapterStore) Companies(context.Context) ([]domain.Company, error) {
	return []domain.Company{{ID: 10, Name: "Компания Тест"}}, nil
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

func (s *adapterStore) Transact(ctx context.Context, f func(domain.Tx) error) error {
	return f(&adapterTx{store: s})
}

// FinishUpdate lets a test drive the real processUpdate pipeline instead of calling
// Handle directly, so callback acknowledgement and error reporting are covered.
func (s *adapterStore) FinishUpdate(context.Context, int64) error { return nil }

type adapterTx struct {
	domain.Tx
	store *adapterStore
}

func (t *adapterTx) Enqueue(_ context.Context, op domain.Operation) error {
	t.store.operations = append(t.store.operations, op)
	return nil
}
func (t *adapterTx) GetUser(context.Context, int64) (domain.User, error) { return t.store.user, nil }
func (t *adapterTx) SaveUser(_ context.Context, u domain.User) error     { t.store.user = u; return nil }

type adapterService struct {
	closedID           int64
	closeText          string
	transferID         int64
	transferDepartment domain.Department
	transferReason     string
	cancelID           int64
	Service
	messages []domain.Message
	public   []bool
}

type claimNameService struct {
	Service
	store *adapterStore
	name  string
}

func (s *claimNameService) Claim(_ context.Context, _, _ int64) error {
	s.name = s.store.user.Name
	return nil
}

func TestClaimCallbackUsesCurrentTelegramName(t *testing.T) {
	client, _ := capturedClient()
	store := &adapterStore{user: domain.User{ID: 2, Name: "Сотрудник 2", Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, Status: domain.New, QueueMessageID: 77}}
	service := &claimNameService{store: store}
	a := NewAdapter(client, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	callback := &Callback{ID: "claim", From: Person{ID: 2, FirstName: "Анна", LastName: "Иванова", Username: "anna"}, Message: &Message{ID: 77, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 1}, Data: "claim:7"}
	if err := a.callback(t.Context(), callback); err != nil {
		t.Fatal(err)
	}
	if service.name != "Анна Иванова" || store.user.Username != "anna" {
		t.Fatalf("claim saw stale Telegram profile: %+v", store.user)
	}
}

func (s *adapterService) Close(_ context.Context, _ int64, id int64, text string) error {
	s.closedID = id
	s.closeText = text
	return nil
}
func (s *adapterService) Transfer(_ context.Context, _ int64, id int64, d domain.Department, text string) error {
	s.transferID = id
	s.transferDepartment = d
	s.transferReason = text
	return nil
}
func (s *adapterService) Cancel(_ context.Context, _ int64, id int64) error {
	s.cancelID = id
	return nil
}
func (s *adapterService) AddMessage(_ context.Context, _ int64, _ int64, m domain.Message, public bool) error {
	s.messages = append(s.messages, m)
	s.public = append(s.public, public)
	return nil
}
func TestOrdinaryReplyAutomaticallyReachesClient(t *testing.T) {
	store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, Status: domain.InProgress}}
	service := &adapterService{}
	a := NewAdapter(nil, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	err := a.Handle(context.Background(), Update{Message: &Message{ID: 20, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "Обычный ответ", Reply: &Message{ID: 19}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(service.public) != 1 || !service.public[0] || service.messages[0].TelegramTopicID != 10 {
		t.Fatal("ordinary reply not relayed from actual source")
	}
}
func TestNonAssigneeDepartmentEngineerCanRelayMediaWithoutReply(t *testing.T) {
	store := &adapterStore{user: domain.User{ID: 3, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, Status: domain.InProgress}}
	service := &adapterService{}
	a := NewAdapter(nil, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err := a.Handle(context.Background(), Update{Message: &Message{ID: 20, From: Person{ID: 3}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Caption: "Проверили доступ", Document: &File{ID: "doc", Name: "proof.pdf"}}}); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 1 || !service.public[0] || service.messages[0].FileID != "doc" {
		t.Fatal("nonassignee media not transparent")
	}
	for _, topic := range []int64{1, 9} {
		if err := a.Handle(context.Background(), Update{Message: &Message{ID: 22, From: Person{ID: 3}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: topic, Text: "Не пересылать"}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(service.messages) != 1 {
		t.Fatal("queue or stale topic relayed")
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
	if len([]rune(engineerCardText(ticket, "Клиент", "Компания", "Специалист"))) > 4096 {
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
	ticket.Status = domain.Closed
	if !historyAuthorized(domain.User{ID: 1, CompanyID: 11, Role: "client", Approved: true}, ticket) {
		t.Fatal("owner lost completed history after company change")
	}
	ticket.Status = domain.New
	if historyAuthorized(domain.User{ID: 1, CompanyID: 11, Role: "client", Approved: true}, ticket) {
		t.Fatal("old active company became accessible")
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
	if err := a.historyPage(context.Background(), &Message{From: Person{ID: 1}, Chat: Chat{ID: 1, Type: "private"}}, store.user, store.ticket, 1, 0); err != nil {
		t.Fatal(err)
	}
	text := transcript.String()
	if !strings.Contains(text, "Initial problem") || !strings.Contains(text, "Original detail") || strings.Contains(text, "SECRET NOTE") {
		t.Fatal("wrong transcript", text)
	}
}
func TestUntrustedGroupRejectedBeforeStoreRead(t *testing.T) {
	a := NewAdapter(nil, nil, nil, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100}}})
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

func (s *adapterStore) ActiveTicket(_ context.Context, id int64) (domain.Ticket, error) {
	if s.ticket.ID > 0 && s.ticket.ClientID == id && s.ticket.Status.Active() {
		return s.ticket, nil
	}
	return domain.Ticket{}, domain.ErrNotFound
}

func TestUnrecognisedGroupChatIsReportedOnce(t *testing.T) {
	adapter := NewAdapter(nil, nil, nil, Config{})
	message := &Message{Chat: Chat{ID: -100777, Type: "supergroup", Title: "Ромашки"}}
	for i := 0; i < 3; i++ {
		if err := adapter.Handle(context.Background(), Update{Message: message}); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("unknown group accepted: %v", err)
		}
	}
	if _, seen := adapter.unrecognised.Load(int64(-100777)); !seen {
		t.Fatal("the real chat_id must be recorded so the operator can find it in the log")
	}
}

func TestMembershipUpdateRevealsUnconfiguredChat(t *testing.T) {
	adapter := NewAdapter(nil, nil, nil, Config{ClientGroups: map[int64]ClientGroup{-100888: {CompanyID: 1}}})
	// Adding the bot to a group the operator typed wrong must still name the chat,
	// because this update arrives even with privacy mode on.
	raw := []byte(`{"update_id":1,"my_chat_member":{"chat":{"id":-100999,"type":"supergroup","title":"Ромашки"},"new_chat_member":{"status":"member"}}}`)
	var update Update
	if err := json.Unmarshal(raw, &update); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Handle(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if _, seen := adapter.unrecognised.Load(int64(-100999)); !seen {
		t.Fatal("membership update must report the real chat_id")
	}
}
