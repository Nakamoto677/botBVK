package tickets

import (
	"context"
	"errors"
	"testing"

	"bot-bvk/internal/domain"
)

func groupRequest(chatID int64, requester int64, text string) domain.GroupTicketRequest {
	return domain.GroupTicketRequest{
		ChatID:          chatID,
		ChatTitle:       "ООО Ромашка · поддержка",
		CompanyID:       10,
		RequesterID:     requester,
		RequesterName:   "Иван Петров",
		Department:      domain.IT,
		Urgency:         domain.Routine,
		Text:            text,
		SourceMessageID: 55,
	}
}

func hasOperation(ops []domain.Operation, kind string) bool {
	for _, op := range ops {
		if op.Kind == kind {
			return true
		}
	}
	return false
}

func TestCreateFromGroupCreatesVirtualClientAndOneActiveTicket(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, err := s.CreateFromGroup(ctx, groupRequest(-100500, 100, "Не работает VPN"))
	if err != nil {
		t.Fatal(err)
	}
	if ticket.ClientID != -100500 || !ticket.IsGroup() || ticket.CompanyID != 10 || ticket.Department != domain.IT || ticket.RequesterID != 100 || ticket.SourceMessageID != 55 {
		t.Fatalf("ticket: %+v", ticket)
	}
	requester := f.users[100]
	if requester.Role != "client" || requester.Approved || requester.CompanyID != 10 {
		t.Fatalf("requester must stay unapproved for private access: %+v", requester)
	}
	group := f.users[-100500]
	if group.Role != "client" || !group.Approved || group.CompanyID != 10 || group.Name == "" {
		t.Fatalf("group client: %+v", group)
	}
	if len(f.tickets) != 1 || len(f.ops) != 1 || f.ops[0].Kind != "refresh_card" {
		t.Fatalf("tickets/ops: %d %+v", len(f.tickets), f.ops)
	}
	if hasOperation(f.ops, "notify_client") || hasOperation(f.ops, "create_topic") {
		t.Fatalf("group creation must be silent and topicless: %+v", f.ops)
	}
	if _, err = s.CreateFromGroup(ctx, groupRequest(-100500, 101, "Ещё одна проблема")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second active group ticket: %v", err)
	}
}

func TestCreateFromGroupRejectsSupportStaffTrigger(t *testing.T) {
	s, _ := setup()
	_, err := s.CreateFromGroup(context.Background(), groupRequest(-100500, 2, "Проверка"))
	if !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("staff trigger: %v", err)
	}
}

func TestGroupMessagesRecordAndFirstReplyClaims(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, err := s.CreateFromGroup(ctx, groupRequest(-100500, 100, "Не работает VPN"))
	if err != nil {
		t.Fatal(err)
	}
	f.ops = nil
	if err = s.AddGroupMessage(ctx, -100500, 10, ticket.ID, domain.GroupAuthor{ID: 100, Name: "Иван Петров"}, domain.Message{Text: "Уточняю: ошибка 619", TelegramChatID: -100500, TelegramMessageID: 56}); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != 1 || f.messages[0].Direction != "inbound" || f.messages[0].AuthorID != 100 {
		t.Fatalf("inbound: %+v", f.messages)
	}
	if !hasOperation(f.ops, "refresh_card") {
		t.Fatal("new-ticket supplement must refresh the queue card")
	}
	f.ops = nil
	if err = s.AddGroupMessage(ctx, -100500, 10, ticket.ID, domain.GroupAuthor{ID: 2, Name: "Анна Инженер"}, domain.Message{Text: "Проверяю", TelegramChatID: -100500, TelegramMessageID: 57}); err != nil {
		t.Fatal(err)
	}
	stored := f.tickets[ticket.ID]
	if stored.Status != domain.InProgress || stored.AssigneeID != 2 || stored.FirstResponseAt == nil {
		t.Fatalf("first reply did not claim: %+v", stored)
	}
	if len(f.messages) != 2 || f.messages[1].Direction != "outbound" || f.messages[1].AuthorID != 2 {
		t.Fatalf("outbound: %+v", f.messages)
	}
	for _, kind := range []string{"claimed", "first_response", "message_delivered"} {
		found := false
		for _, event := range f.events {
			found = found || event.Kind == kind
		}
		if !found {
			t.Fatalf("missing event %s: %+v", kind, f.events)
		}
	}
	if hasOperation(f.ops, "client_message") || hasOperation(f.ops, "team_message") {
		t.Fatalf("group chat must not relay: %+v", f.ops)
	}
	if err = s.AddGroupMessage(ctx, -100500, 10, ticket.ID, domain.GroupAuthor{ID: 3, Name: "Инженер 1С"}, domain.Message{Text: "Чужой отдел", TelegramChatID: -100500, TelegramMessageID: 58}); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != 2 {
		t.Fatal("engineer from another department was recorded")
	}
	if err = s.AddGroupMessage(ctx, -100500, 10, ticket.ID, domain.GroupAuthor{ID: 100}, domain.Message{Text: "Повтор", TelegramChatID: -100500, TelegramMessageID: 56}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestGroupCloseIsSilentAndDoesNotCloseTopic(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, err := s.CreateFromGroup(ctx, groupRequest(-100500, 100, "Проблема"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddGroupMessage(ctx, -100500, 10, ticket.ID, domain.GroupAuthor{ID: 2}, domain.Message{Text: "Беру", TelegramChatID: -100500, TelegramMessageID: 60}); err != nil {
		t.Fatal(err)
	}
	f.ops = nil
	if err = s.Close(ctx, 2, ticket.ID, "Перезагрузили шлюз"); err != nil {
		t.Fatal(err)
	}
	if f.tickets[ticket.ID].Status != domain.Closed {
		t.Fatal("not closed")
	}
	if hasOperation(f.ops, "notify_client") || hasOperation(f.ops, "close_topic") {
		t.Fatalf("group close is silent: %+v", f.ops)
	}
	if !hasOperation(f.ops, "refresh_card") {
		t.Fatal("closed card not refreshed")
	}
}

func TestGroupClaimAndTransferHaveNoTopicOperations(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, err := s.CreateFromGroup(ctx, groupRequest(-100500, 100, "Проблема"))
	if err != nil {
		t.Fatal(err)
	}
	f.ops = nil
	if err = s.Claim(ctx, 2, ticket.ID); err != nil {
		t.Fatal(err)
	}
	claimed := f.tickets[ticket.ID]
	if claimed.Status != domain.InProgress || claimed.RoutingPending || claimed.AssigneeID != 2 {
		t.Fatalf("claim: %+v", claimed)
	}
	if hasOperation(f.ops, "create_topic") || hasOperation(f.ops, "notify_client") {
		t.Fatalf("group claim must be topicless and silent: %+v", f.ops)
	}
	f.ops = nil
	if err = s.Transfer(ctx, 2, ticket.ID, domain.OneC, "Нужен 1С"); err != nil {
		t.Fatal(err)
	}
	if f.tickets[ticket.ID].Department != domain.OneC || f.tickets[ticket.ID].Status != domain.New {
		t.Fatalf("transfer: %+v", f.tickets[ticket.ID])
	}
	if hasOperation(f.ops, "close_topic") || hasOperation(f.ops, "notify_client") {
		t.Fatalf("group transfer must be silent: %+v", f.ops)
	}
}

func TestCreateFromGroupStoresAttachmentWithoutPublishing(t *testing.T) {
	s, f := setup()
	req := groupRequest(-100500, 100, "Скриншот ошибки")
	req.Attachment = &domain.Message{FileID: "doc", FileName: "error.png", FileSize: 10, MediaType: "document", TelegramChatID: -100500, TelegramMessageID: 55}
	ticket, err := s.CreateFromGroup(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != 1 || f.messages[0].TicketID != ticket.ID || f.messages[0].Direction != "inbound" || f.messages[0].FileID != "doc" {
		t.Fatalf("attachment message: %+v", f.messages)
	}
	if !hasOperation(f.ops, "refresh_card") || !hasOperation(f.ops, "archive_message") {
		t.Fatalf("ops: %+v", f.ops)
	}
	if hasOperation(f.ops, "notify_client") || hasOperation(f.ops, "client_message") {
		t.Fatalf("attachment was published: %+v", f.ops)
	}
}

func TestGroupEditUpdatesHistoryWithoutRelay(t *testing.T) {
	s, f := setup()
	ctx := context.Background()
	ticket, err := s.CreateFromGroup(ctx, groupRequest(-100500, 100, "Проблема"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddGroupMessage(ctx, -100500, 10, ticket.ID, domain.GroupAuthor{ID: 100}, domain.Message{Text: "Первая версия", TelegramChatID: -100500, TelegramMessageID: 56}); err != nil {
		t.Fatal(err)
	}
	f.ops = nil
	when := s.now()
	if err = s.EditMessage(ctx, 100, ticket.ID, domain.Message{Text: "Исправленная версия", TelegramChatID: -100500, TelegramMessageID: 56, EditedAt: &when}); err != nil {
		t.Fatal(err)
	}
	if len(f.messages) != 1 || f.messages[0].Text != "Исправленная версия" {
		t.Fatalf("edited message: %+v", f.messages)
	}
	if hasOperation(f.ops, "team_message") || hasOperation(f.ops, "client_message") {
		t.Fatalf("group edit relayed: %+v", f.ops)
	}
}
