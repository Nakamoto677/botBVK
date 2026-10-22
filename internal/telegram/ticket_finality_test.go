package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"bot-bvk/internal/domain"
)

// replyStore adds message-to-ticket resolution to the lightweight adapter store.
type replyStore struct {
	*adapterStore
	original domain.Ticket
}

func (s *replyStore) MessageTicket(_ context.Context, _ int64, id int64) (domain.Ticket, error) {
	switch id {
	case 500:
		return s.original, nil
	case 501:
		return s.ticket, nil
	default:
		return domain.Ticket{}, domain.ErrNotFound
	}
}

func TestLegacyReopenCommandAndButtonOnlyExplainFinality(t *testing.T) {
	ctx := context.Background()
	a, s, svc, m, requests := clientUIFixture(t)
	m.Text = "/reopen 7"
	if err := a.Handle(ctx, Update{Message: m}); err != nil {
		t.Fatal(err)
	}
	last := (*requests)[len(*requests)-1]
	if last["text"] != "Заявка завершена. Создайте новую заявку через меню." {
		t.Fatalf("/reopen answer = %v", last)
	}
	if s.ticket.Status != domain.InProgress || svc.calls != 0 {
		t.Fatal("/reopen mutated the ticket")
	}

	if err := a.callback(ctx, &Callback{ID: "legacy", From: m.From, Message: &Message{ID: 200, Chat: m.Chat}, Data: "reopen:7"}); err != nil {
		t.Fatal(err)
	}
	found := ""
	for _, request := range *requests {
		if text, _ := request["text"].(string); strings.HasPrefix(text, "Заявка завершена.") {
			found = text
		}
	}
	if !strings.Contains(found, "Создайте новую заявку") {
		t.Fatalf("legacy reopen button answered %q", found)
	}
	if s.ticket.Status != domain.InProgress || svc.calls != 0 {
		t.Fatal("legacy reopen button mutated the ticket")
	}
}

func TestReplyToAnotherTicketsMessageIsNotRelayed(t *testing.T) {
	ctx := context.Background()
	client, capture := capturedClient()
	store := &replyStore{
		adapterStore: &adapterStore{
			user:   domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}},
			ticket: domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, Status: domain.InProgress},
		},
		original: domain.Ticket{ID: 6, ClientID: 1, CompanyID: 10, Department: domain.IT, Status: domain.Closed, TopicChatID: -100, TopicID: 10},
	}
	service := &adapterService{}
	a := NewAdapter(client, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	if err := a.Handle(ctx, Update{Message: &Message{ID: 20, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "Ответ на прошлую заявку", Reply: &Message{ID: 500}}}); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 0 {
		t.Fatal("reply to another ticket was relayed to the client")
	}
	found := false
	for _, body := range capture.bodies {
		if text, _ := body["text"].(string); strings.Contains(text, "относится к заявке #6") && strings.Contains(text, "текущей заявки #7") {
			found = true
		}
	}
	if !found {
		t.Fatalf("engineer was not told to answer the current ticket: %v", capture.bodies)
	}

	if err := a.Handle(ctx, Update{Message: &Message{ID: 21, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "Ответ по текущей заявке", Reply: &Message{ID: 501}}}); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 1 || !service.public[0] {
		t.Fatal("reply inside the current ticket was not relayed")
	}
}

func TestUnknownReplyTargetKeepsOrdinaryReplyWorking(t *testing.T) {
	ctx := context.Background()
	client, _ := capturedClient()
	store := &replyStore{
		adapterStore: &adapterStore{
			user:   domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}},
			ticket: domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, Status: domain.InProgress},
		},
	}
	service := &adapterService{}
	a := NewAdapter(client, store, service, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	// An unknown replied-to message is not proof of another ticket, so an ordinary
	// answer still goes through.
	if err := a.Handle(ctx, Update{Message: &Message{ID: 22, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: "Обычный ответ", Reply: &Message{ID: 999}}}); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 1 || !service.public[0] {
		t.Fatal("unknown reply target blocked an ordinary answer")
	}
}

func TestHistoryOfClosedTicketLinksItsCardAndKeepsServiceNotice(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{
		ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT, Status: domain.Closed,
		TopicChatID: -1001234567890, TopicID: 10, TopicMessageID: 77, Description: "Описание",
	}
	store := &adapterStore{
		user:   domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}},
		ticket: ticket,
		events: []domain.Event{
			{ID: 1, TicketID: 7, Kind: "closed", Data: json.RawMessage(`{"resolution":"Готово"}`), CreatedAt: time.Now().UTC()},
			{ID: 2, TicketID: 7, Kind: "topic_closing_notice", Data: json.RawMessage(`{"text":"Заявка #7 закрыта. Итог: Готово"}`), CreatedAt: time.Now().UTC()},
		},
	}
	a := NewAdapter(client, store, nil, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -1001, QueueTopicID: 2}}})
	m := &Message{ID: 100, From: Person{ID: 2}, Chat: Chat{ID: -1001, Type: "supergroup"}, ThreadID: 10}
	for page := 0; page < 2; page++ {
		if err := a.historyPage(context.Background(), m, store.user, ticket, page, 0); err != nil {
			t.Fatal(err)
		}
	}
	foundNotice, foundLink, foundUrgency := false, false, false
	for _, body := range capture.bodies {
		text, _ := body["text"].(string)
		foundNotice = foundNotice || strings.Contains(text, "Служебное") && strings.Contains(text, "Заявка #7 закрыта. Итог: Готово")
		foundUrgency = foundUrgency || strings.Contains(text, "Срочность:")
		raw, _ := json.Marshal(body["reply_markup"])
		foundLink = foundLink || strings.Contains(string(raw), "Открыть карточку в топике") && strings.Contains(string(raw), "t.me/c/1234567890/77")
	}
	if !foundNotice || !foundLink || !foundUrgency {
		t.Fatalf("history lost card link or service notice: notice=%v link=%v urgency=%v %v", foundNotice, foundLink, foundUrgency, capture.bodies)
	}
}

func TestUrgentAcceptanceDoesNotWaitForTheQueueCard(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{ID: 7, ClientID: 1, CompanyID: 10, Department: domain.IT, Status: domain.New, Urgency: domain.Urgent, CreatedAt: time.Now().UTC()}
	store := &adapterStore{user: domain.User{ID: 1, Role: "client", CompanyID: 10, Approved: true}, ticket: ticket}
	worker := NewWorker(client, store, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 2}}})
	worker.interval = 0
	if err := worker.clientNotification(context.Background(), ticket, "Заявка принята и поставлена в очередь."); err != nil {
		t.Fatal(err)
	}
	if len(capture.bodies) == 0 {
		t.Fatal("client was not told that the ticket was accepted")
	}
	text, _ := capture.bodies[0]["text"].(string)
	if !strings.HasPrefix(text, "Заявка №7 принята") || !strings.Contains(text, "Срочность: Срочно") {
		t.Fatalf("acceptance text = %q", text)
	}
}
