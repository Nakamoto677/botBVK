package telegram

import (
	"context"
	"errors"
	"strings"
	"testing"

	"bot-bvk/internal/domain"
)

type recordedGroupMessage struct {
	ChatID    int64
	CompanyID int64
	TicketID  int64
	Author    domain.GroupAuthor
	Message   domain.Message
}

type groupAdapterService struct {
	Service
	created   []domain.GroupTicketRequest
	messages  []recordedGroupMessage
	createErr error
	addErr    error
}

func (s *groupAdapterService) CreateFromGroup(_ context.Context, req domain.GroupTicketRequest) (domain.Ticket, error) {
	s.created = append(s.created, req)
	return domain.Ticket{ID: 7, ClientID: req.ChatID, Channel: domain.ChannelGroup, Department: req.Department}, s.createErr
}

func (s *groupAdapterService) AddGroupMessage(_ context.Context, chatID, companyID, ticketID int64, author domain.GroupAuthor, msg domain.Message) error {
	s.messages = append(s.messages, recordedGroupMessage{ChatID: chatID, CompanyID: companyID, TicketID: ticketID, Author: author, Message: msg})
	return s.addErr
}

func groupTestAdapter(client *Client, store domain.Store, service Service) *Adapter {
	return NewAdapter(client, store, service, Config{
		Departments: map[domain.Department]Team{
			domain.IT:           {ChatID: -1001, QueueTopicID: 2, Tags: []string{"#it", "#ит"}},
			domain.OneC:         {ChatID: -1002, QueueTopicID: 2, Tags: []string{"#1c", "#1с"}},
			domain.Installation: {ChatID: -1003, QueueTopicID: 2, Tags: []string{"#монтаж"}},
			domain.Field:        {ChatID: -1004, QueueTopicID: 2, Tags: []string{"#выезд"}},
		},
		ClientGroups: map[int64]ClientGroup{-100500: {CompanyID: 10}},
		BotUsername:  "supportbot",
	})
}

func groupTestMessage(text string, id int64) *Message {
	return &Message{
		ID:   id,
		From: Person{ID: 100, FirstName: "Иван", LastName: "Петров", Username: "ivan"},
		Chat: Chat{ID: -100500, Type: "supergroup", Title: "ООО Ромашка"},
		Text: text,
	}
}

func TestGroupTagCreatesTicketWithoutBotMessage(t *testing.T) {
	store := &adapterStore{}
	service := &groupAdapterService{}
	adapter := groupTestAdapter(nil, store, service)
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("#1C Не работает VPN", 55)}); err != nil {
		t.Fatal(err)
	}
	if len(service.created) != 1 {
		t.Fatalf("created: %+v", service.created)
	}
	got := service.created[0]
	if got.ChatID != -100500 || got.CompanyID != 10 || got.Department != domain.OneC || got.Urgency != domain.Routine || got.Text != "Не работает VPN" || got.SourceMessageID != 55 {
		t.Fatalf("request: %+v", got)
	}
	if got.RequesterID != 100 || got.RequesterName != "Иван Петров" || got.RequesterUsername != "ivan" || got.ChatTitle != "ООО Ромашка" {
		t.Fatalf("requester: %+v", got)
	}
}

func TestGroupReceiptIsOptional(t *testing.T) {
	client, capture := capturedClient()
	service := &groupAdapterService{}
	a := groupTestAdapter(client, &adapterStore{}, service)
	a.cfg.GroupReceipt = true
	if err := a.Handle(context.Background(), Update{Message: groupTestMessage("#it Не работает VPN", 55)}); err != nil {
		t.Fatal(err)
	}
	if len(capture.bodies) != 1 || capture.bodies[0]["text"] != "Заявка №7 создана для отдела IT. Ответим здесь." {
		t.Fatalf("group receipt: %v", capture.bodies)
	}
}

func TestGroupUrgentTagAndAttachment(t *testing.T) {
	store := &adapterStore{}
	service := &groupAdapterService{}
	adapter := groupTestAdapter(nil, store, service)
	message := groupTestMessage("#it #срочно", 56)
	message.Document = &File{ID: "doc", Name: "screen.png", Size: 100}
	if err := adapter.Handle(context.Background(), Update{Message: message}); err != nil {
		t.Fatal(err)
	}
	if len(service.created) != 1 {
		t.Fatalf("created: %+v", service.created)
	}
	req := service.created[0]
	if req.Department != domain.IT || req.Urgency != domain.Urgent || req.Attachment == nil || req.Attachment.FileID != "doc" {
		t.Fatalf("request: %+v", req)
	}
}

func TestGroupMentionWithoutTagSendsHintOnly(t *testing.T) {
	client, capture := capturedClient()
	store := &adapterStore{}
	service := &groupAdapterService{}
	adapter := groupTestAdapter(client, store, service)
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("@supportbot помогите", 57)}); err != nil {
		t.Fatal(err)
	}
	if len(service.created) != 0 || len(capture.bodies) != 1 {
		t.Fatalf("created=%d sends=%d", len(service.created), len(capture.bodies))
	}
	text, _ := capture.bodies[0]["text"].(string)
	if !strings.Contains(text, "Чтобы создать заявку") || !strings.Contains(text, "#1c") {
		t.Fatalf("hint: %q", text)
	}
}

func TestGroupNoTagWithoutMentionIsSilent(t *testing.T) {
	client, capture := capturedClient()
	store := &adapterStore{}
	service := &groupAdapterService{}
	adapter := groupTestAdapter(client, store, service)
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("просто разговор", 58)}); err != nil {
		t.Fatal(err)
	}
	if len(service.created) != 0 || len(capture.bodies) != 0 {
		t.Fatalf("unexpected action: %d %d", len(service.created), len(capture.bodies))
	}
}

func TestGroupActiveTicketRecordsMessagesAndSkipsTriggerRetry(t *testing.T) {
	store := &adapterStore{ticket: domain.Ticket{ID: 7, ClientID: -100500, Channel: domain.ChannelGroup, CompanyID: 10, Department: domain.IT, Status: domain.InProgress, SourceMessageID: 55}}
	service := &groupAdapterService{}
	adapter := groupTestAdapter(nil, store, service)
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("Уточнение", 59)}); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 1 {
		t.Fatalf("messages: %+v", service.messages)
	}
	got := service.messages[0]
	if got.ChatID != -100500 || got.CompanyID != 10 || got.TicketID != 7 || got.Author.ID != 100 || got.Message.TelegramMessageID != 59 {
		t.Fatalf("message: %+v", got)
	}
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("Повтор", 55)}); err != nil {
		t.Fatal(err)
	}
	if len(service.messages) != 1 {
		t.Fatal("trigger retry was recorded as a new message")
	}
}

func TestGroupStaffTagDoesNotCreateTicket(t *testing.T) {
	client, capture := capturedClient()
	store := &adapterStore{}
	service := &groupAdapterService{createErr: domain.ErrForbidden}
	adapter := groupTestAdapter(client, store, service)
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("#1c служебная проверка", 60)}); err != nil {
		t.Fatal(err)
	}
	if len(service.created) != 1 || len(capture.bodies) != 0 {
		t.Fatalf("staff trigger leaked: created=%d sends=%d", len(service.created), len(capture.bodies))
	}
}

func TestGroupCommandIsIgnored(t *testing.T) {
	store := &adapterStore{}
	service := &groupAdapterService{}
	adapter := groupTestAdapter(nil, store, service)
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("/start", 61)}); err != nil {
		t.Fatal(err)
	}
	if len(service.created) != 0 || len(service.messages) != 0 {
		t.Fatal("command processed in group")
	}
}

func TestGroupDuplicateMessageConflictIsAccepted(t *testing.T) {
	store := &adapterStore{
		ticket:         domain.Ticket{ID: 7, ClientID: -100500, Channel: domain.ChannelGroup, Status: domain.InProgress, SourceMessageID: 55},
		storedMessages: []domain.Message{{TicketID: 7, AuthorID: 100, Direction: "inbound", TelegramChatID: -100500, TelegramMessageID: 62}},
	}
	service := &groupAdapterService{addErr: domain.ErrConflict}
	adapter := groupTestAdapter(nil, store, service)
	if err := adapter.Handle(context.Background(), Update{Message: groupTestMessage("Повтор", 62)}); err != nil {
		t.Fatal(err)
	}
}

func TestGroupCallbackRejected(t *testing.T) {
	adapter := groupTestAdapter(nil, &adapterStore{}, &groupAdapterService{})
	callback := &Callback{ID: "x", From: Person{ID: 100}, Message: &Message{ID: 5, Chat: Chat{ID: -100500, Type: "supergroup"}}, Data: "close:7"}
	if err := adapter.Handle(context.Background(), Update{Callback: callback}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("callback accepted: %v", err)
	}
}

func TestGroupChatURL(t *testing.T) {
	if got := groupChatURL(ClientGroup{}, -1001234567890, 55); got != "https://t.me/c/1234567890/55" {
		t.Fatalf("derived: %q", got)
	}
	if got := groupChatURL(ClientGroup{Link: "https://t.me/+invite"}, -1001234567890, 55); got != "https://t.me/+invite" {
		t.Fatalf("configured: %q", got)
	}
	if got := groupChatURL(ClientGroup{}, -500, 55); got != "" {
		t.Fatalf("basic group without link: %q", got)
	}
}
