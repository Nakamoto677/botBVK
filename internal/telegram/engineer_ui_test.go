package telegram

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"bot-bvk/internal/domain"
)

func TestEngineerQueueAndTopicActionMatrix(t *testing.T) {
	ticket := domain.Ticket{ID: 42, Status: domain.New, Department: domain.IT}
	buttons := engineerQueueButtons(ticket, "")
	if len(buttons) != 2 || buttons[0][0].Data != "claim:42" || buttons[1][0].Data != "transfer:42" {
		t.Fatalf("unclaimed controls: %+v", buttons)
	}
	ticket.Status = domain.InProgress
	ticket.AssigneeID = 2
	ticket.TopicChatID = -1001234567890
	ticket.TopicID = 7
	ticket.TopicMessageID = 88
	buttons = engineerQueueButtons(ticket, "")
	if len(buttons) != 2 || len(buttons[1]) != 2 || buttons[0][0].Text != "В чат" || buttons[0][0].URL != "https://t.me/c/1234567890/88?thread=7" || buttons[1][0].Text != engineerCloseLabel {
		t.Fatalf("assigned controls: %+v", buttons)
	}
	topic := engineerTopicButtons(ticket)
	if len(topic) != 1 || len(topic[0]) != 2 || topic[0][0].Text != engineerCloseLabel || topic[0][1].Data != "transfer:42" {
		t.Fatal(topic)
	}
	ticket.RoutingPending = true
	if len(engineerQueueButtons(ticket, "")) != 0 {
		t.Fatal("actions before provisioning")
	}
	for _, status := range []domain.Status{domain.Closed, domain.Cancelled} {
		ticket.Status = status
		if len(engineerQueueButtons(ticket, ""))+len(engineerTopicButtons(ticket)) != 0 {
			t.Fatal("terminal actions")
		}
	}
}
func TestEngineerCardEscapesPersonalDataAndBoundsRenderedUnicode(t *testing.T) {
	ticket := domain.Ticket{ID: 42, Status: domain.New, Department: domain.OneC, Description: "<b>evil</b> & " + strings.Repeat("🧑", 3000)}
	text := engineerCardText(ticket, "Иван\nИсполнитель: подмена", "<Company> & Co", "")
	if strings.Contains(text, "<Company>") || strings.Contains(text, "<b>evil</b>") || !strings.Contains(text, "&lt;b&gt;evil&lt;/b&gt;") || !strings.Contains(text, "<b>Описание</b>\n") {
		t.Fatal("unsafe card", text[:200])
	}
	bounded := engineerTextBound(ticket.Description, 2400)
	if len(utf16.Encode([]rune(bounded))) > 2401 || !strings.HasSuffix(bounded, "…") {
		t.Fatal("unicode bound wrong")
	}
	if strings.Contains(text, "Иван\nИсполнитель:") {
		t.Fatal("name injected card field")
	}
}

// The transfer label is only for the retired queue card. A card of a really
// closed ticket must keep saying «Закрыта».
func TestEngineerCardKeepsClosedStatus(t *testing.T) {
	ticket := domain.Ticket{ID: 42, Status: domain.Closed, Department: domain.IT, Description: "Решено"}
	text := engineerCardText(ticket, "Клиент", "Компания", "Специалист")
	if !strings.Contains(text, "Заявка #42 · Закрыта") || strings.Contains(text, transferredStatusName) {
		t.Fatalf("closed card status = %q", text)
	}
}
func TestEngineerReplyButtonsStartActionInClientTopic(t *testing.T) {
	for label, action := range map[string]string{engineerCloseLabel: "close", engineerTransferLabel: "transfer"} {
		t.Run(label, func(t *testing.T) {
			client, _ := capturedClient()
			store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}
			svc := &adapterService{}
			a := NewAdapter(client, store, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
			message := &Message{ID: 80, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: label}
			if err := a.Handle(context.Background(), Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			if len(svc.messages) != 0 || svc.closedID != 0 || svc.transferID != 0 {
				t.Fatal("reply button was relayed or completed action without confirmation")
			}
			p, ok := a.prompts[scope(2, message)]
			if !ok || p.TicketID != 7 || p.Action != action {
				t.Fatal("reply button did not start action", a.prompts)
			}
		})
	}
}
func TestEngineerReplyButtonsCannotActInQueue(t *testing.T) {
	client, capture := capturedClient()
	store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}
	svc := &adapterService{}
	a := NewAdapter(client, store, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	for _, label := range []string{engineerCloseLabel, engineerTransferLabel} {
		err := a.Handle(context.Background(), Update{Message: &Message{From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 1, Text: label}})
		if err != nil {
			t.Fatalf("queue button was not retired: %v", err)
		}
	}
	if len(svc.messages) != 0 || svc.closedID != 0 || svc.transferID != 0 || len(a.prompts) != 0 || len(capture.bodies) != 2 {
		t.Fatal("queue button changed state", capture.bodies)
	}
	for _, body := range capture.bodies {
		raw, _ := json.Marshal(body["reply_markup"])
		if body["message_thread_id"] != float64(1) || !strings.Contains(string(raw), `"remove_keyboard":true`) {
			t.Fatal("queue keyboard was not removed", body)
		}
	}
}

func TestEngineerReplyButtonCannotActInDifferentTopic(t *testing.T) {
	client, _ := capturedClient()
	store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}
	svc := &adapterService{}
	a := NewAdapter(client, store, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	m := &Message{ID: 80, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 9, Text: engineerCloseLabel}
	if err := a.Handle(context.Background(), Update{Message: m}); err != domain.ErrConflict {
		t.Fatalf("different topic accepted button: %v", err)
	}
	if len(a.prompts) != 0 || len(svc.messages) != 0 || svc.closedID != 0 {
		t.Fatal("different topic changed ticket")
	}
}

func TestClaimUsesOnlyEditableInlineCards(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{ID: 7, ClientID: 12, CompanyID: 10, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, QueueMessageID: 77, RoutingPending: true, RoutingGeneration: 1}
	store := &workerStore{ticket: ticket, current: ticket}
	cfg := Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}}
	worker := NewWorker(client, store, cfg)
	worker.interval = 0
	if err := worker.Handle(context.Background(), domain.Operation{ID: 5, Kind: "create_topic", TicketID: 7}); err != nil {
		t.Fatal(err)
	}
	if store.current.RoutingPending || store.current.TopicMessageID == 0 {
		t.Fatal("claim did not finish", store.current)
	}
	if len(capture.calls) != 3 || !strings.HasSuffix(capture.calls[0], "sendMessage") || !strings.HasSuffix(capture.calls[1], "editMessageText") || !strings.HasSuffix(capture.calls[2], "editMessageText") {
		t.Fatal("claim must send one client card and edit both cards without an extra message", capture.calls, capture.bodies)
	}
	for i, body := range capture.bodies {
		raw, _ := json.Marshal(body["reply_markup"])
		if strings.Contains(string(raw), `"keyboard"`) || strings.Contains(string(raw), `"remove_keyboard"`) {
			t.Fatal("claim installed a group-wide reply keyboard", capture.calls[i], body)
		}
		if i == 0 && (body["message_thread_id"] != float64(10) || strings.Contains(string(raw), "close:7")) {
			t.Fatal("client card sent to wrong topic or actions enabled before provisioning", body)
		}
		if i > 0 && (!strings.Contains(string(raw), `"callback_data":"close:7"`) || !strings.Contains(string(raw), `"callback_data":"transfer:7"`)) {
			t.Fatal("inline actions missing from an edited card", body)
		}
	}
}

func TestClientContactTracksUsernameChangesAndMissingUsername(t *testing.T) {
	ctx := context.Background()
	store := &adapterStore{user: domain.User{ID: 1, Name: "Иван", Username: "old", Role: "client", Approved: true}}
	a := NewAdapter(nil, store, nil, Config{})
	for _, username := range []string{"new_name", ""} {
		if err := a.updateName(ctx, store.user, Person{ID: 1, FirstName: "Иван", Username: username}); err != nil {
			t.Fatal(err)
		}
		if store.user.Username != username || !store.user.Approved {
			t.Fatal(store.user)
		}
		name, err := clientContact(ctx, store, 1)
		if err != nil {
			t.Fatal(err)
		}
		want := "Telegram: не указан"
		if username != "" {
			want = "@" + username
		}
		ticket := domain.Ticket{ID: 7, Department: domain.IT}
		if text := engineerCardText(ticket, name, "Компания", ""); !strings.Contains(text, want) {
			t.Fatal(text)
		}
	}
	var person Person
	if err := json.Unmarshal([]byte(`{"id":1,"first_name":"Иван","username":"incoming"}`), &person); err != nil || person.Username != "incoming" {
		t.Fatal(person, err)
	}
}

func TestTransferPromptCommentAndControlCleanup(t *testing.T) {
	for _, cancelBeforeDepartment := range []bool{true, false} {
		ctx := context.Background()
		client, requests := uiClient(t)
		s := &durableUIStore{adapterStore: &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}, workflows: map[promptKey][]byte{}, aux: map[auxKey][]domain.AuxiliaryMessage{}}
		svc := &adapterService{}
		a := NewAdapter(client, s, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}, domain.OneC: {ChatID: -200, QueueTopicID: 1}}})
		m := &Message{ID: 80, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10}
		if err := a.beginStaffPrompt(ctx, &Callback{From: m.From, Message: m}, 7, "transfer"); err != nil {
			t.Fatal(err)
		}
		// Restart must retain the engineer control's cleanup scope.
		a = NewAdapter(client, s, svc, a.cfg)
		if !cancelBeforeDepartment {
			if err := a.promptCallback(ctx, &Callback{From: m.From, Message: m}, "transferdept", 7, []string{"transferdept", "7", "1c"}); err != nil {
				t.Fatal(err)
			}
			want := "Передача заявки №7 → 1С\nНапишите причину передачи. Её увидят инженеры нового отдела. На ввод есть 15 минут."
			if (*requests)[len(*requests)-1]["text"] != want {
				t.Fatal(*requests)
			}
			m.ID = 81
			m.Text = "-"
			if _, err := a.consumePrompt(ctx, m, s.user); err != nil {
				t.Fatal(err)
			}
			if svc.transferReason != "-" || len(svc.messages) != 0 {
				t.Fatal("comment sent as public reply")
			}
		} else if err := a.promptCallback(ctx, &Callback{From: m.From, Message: m}, "promptcancel", 7, []string{"promptcancel", "7"}); err != nil {
			t.Fatal(err)
		} else if got := (*requests)[len(*requests)-1]["text"]; got != "Передача отменена — заявка #7 осталась в текущем отделе." {
			t.Fatalf("declined transfer left no receipt: %v", got)
		}
		removed := false
		for _, op := range s.operations {
			var p struct {
				ID int64 `json:"message_id"`
			}
			json.Unmarshal(op.Payload, &p)
			if op.Kind == "delete_aux_message" {
				removed = true
			}
			if p.ID == 80 || p.ID == 81 {
				t.Fatal("ticket card or free-text reason deleted")
			}
		}
		if !removed {
			t.Fatal("transfer prompt retained")
		}
	}
}

func TestTransferWithoutCommentButtonCompletesPrompt(t *testing.T) {
	ctx := context.Background()
	client, _ := uiClient(t)
	s := &durableUIStore{adapterStore: &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}, workflows: map[promptKey][]byte{}, aux: map[auxKey][]domain.AuxiliaryMessage{}}
	svc := &adapterService{}
	a := NewAdapter(client, s, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}, domain.OneC: {ChatID: -200, QueueTopicID: 1}}})
	m := &Message{ID: 80, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10}
	if err := a.beginStaffPrompt(ctx, &Callback{From: m.From, Message: m}, 7, "transfer"); err != nil {
		t.Fatal(err)
	}
	if err := a.promptCallback(ctx, &Callback{From: m.From, Message: m}, "transferdept", 7, []string{"transferdept", "7", "1c"}); err != nil {
		t.Fatal(err)
	}
	if err := a.promptCallback(ctx, &Callback{From: m.From, Message: m}, "transferwithout", 7, []string{"transferwithout", "7"}); err != nil {
		t.Fatal(err)
	}
	if svc.transferID != 7 || svc.transferDepartment != domain.OneC || svc.transferReason != "-" || len(s.workflows) != 0 || len(svc.messages) != 0 {
		t.Fatal("button did not complete a silent transfer")
	}
}

func TestCloseWithoutCommentButtonCompletesPrompt(t *testing.T) {
	client, capture := capturedClient()
	s := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, QueueMessageID: 77}}
	svc := &adapterService{}
	a := NewAdapter(client, s, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	card := &Message{ID: 77, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10}
	if err := a.Handle(context.Background(), Update{Callback: &Callback{ID: "close", From: card.From, Message: card, Data: "close:7"}}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, body := range capture.bodies {
		if text, _ := body["text"].(string); strings.Contains(text, "напишите итог решения") {
			raw, _ := json.Marshal(body["reply_markup"])
			found = strings.Contains(string(raw), `"callback_data":"closewithout:7"`) && strings.Contains(string(raw), `"text":"Без комментария"`) && strings.Contains(string(raw), `"callback_data":"promptcancel:7"`)
		}
	}
	if !found {
		t.Fatal("client-topic close prompt has no skip button", capture.bodies)
	}
	promptMessage := &Message{ID: 77, Chat: card.Chat, ThreadID: 10}
	cb := &Callback{ID: "skip", From: card.From, Message: promptMessage, Data: "closewithout:7"}
	if err := a.Handle(context.Background(), Update{Callback: cb}); err != nil {
		t.Fatal(err)
	}
	if svc.closedID != 7 || svc.closeText != "Без комментария" || len(a.prompts) != 0 || len(svc.messages) != 0 {
		t.Fatal("skip button did not close the ticket without a message", svc, a.prompts)
	}
	if err := a.Handle(context.Background(), Update{Callback: cb}); err != domain.ErrConflict {
		t.Fatalf("old button was accepted again: %v", err)
	}
}

func TestQueueClosePromptKeepsTextOnly(t *testing.T) {
	client, capture := capturedClient()
	s := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10, QueueMessageID: 77}}
	svc := &adapterService{}
	a := NewAdapter(client, s, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	card := &Message{ID: 77, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 1}
	if err := a.Handle(context.Background(), Update{Callback: &Callback{ID: "close", From: card.From, Message: card, Data: "close:7"}}); err != nil {
		t.Fatal(err)
	}
	foundPrompt := false
	for _, body := range capture.bodies {
		if text, _ := body["text"].(string); strings.Contains(text, "напишите итог решения") {
			foundPrompt = true
			raw, _ := json.Marshal(body["reply_markup"])
			if strings.Contains(string(raw), "closewithout:7") || !strings.Contains(string(raw), "promptcancel:7") {
				t.Fatal("queue prompt gained a skip button", body)
			}
		}
	}
	if !foundPrompt {
		t.Fatal("queue close prompt missing", capture.bodies)
	}
	if err := a.Handle(context.Background(), Update{Callback: &Callback{ID: "old-skip", From: card.From, Message: card, Data: "closewithout:7"}}); err != domain.ErrConflict {
		t.Fatalf("skip callback in queue accepted: %v", err)
	}
	if svc.closedID != 0 {
		t.Fatal("queue ticket closed without a text result")
	}
	if err := a.Handle(context.Background(), Update{Message: &Message{ID: 81, From: card.From, Chat: card.Chat, ThreadID: 1, Text: "Готово"}}); err != nil {
		t.Fatal(err)
	}
	if svc.closedID != 7 || svc.closeText != "Готово" {
		t.Fatal("queue text close stopped working", svc)
	}
}

func TestCloseWithoutCommentRechecksAssignee(t *testing.T) {
	client, _ := capturedClient()
	s := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}
	svc := &adapterService{}
	a := NewAdapter(client, s, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	m := &Message{From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10}
	if err := a.beginStaffPrompt(context.Background(), &Callback{From: m.From, Message: m}, 7, "close"); err != nil {
		t.Fatal(err)
	}
	s.ticket.AssigneeID = 3
	if err := a.promptCallback(context.Background(), &Callback{From: m.From, Message: m}, "closewithout", 7, []string{"closewithout", "7"}); err != domain.ErrForbidden {
		t.Fatalf("unassigned engineer could close without comment: %v", err)
	}
	if svc.closedID != 0 {
		t.Fatal("unassigned engineer closed ticket")
	}
}

func TestGroupQueueCardLinksToCompanyChatAndHidesTopicControls(t *testing.T) {
	ticket := domain.Ticket{ID: 42, ClientID: -100500, Channel: domain.ChannelGroup, Status: domain.New, Department: domain.OneC, SourceMessageID: 55}
	buttons := engineerQueueButtons(ticket, "https://t.me/c/500/55")
	if len(buttons) != 3 || buttons[0][0].Text != "В чат компании" || buttons[0][0].URL != "https://t.me/c/500/55" || buttons[1][0].Data != "claim:42" || buttons[2][0].Data != "transfer:42" {
		t.Fatalf("new group controls: %+v", buttons)
	}
	ticket.Status = domain.InProgress
	ticket.AssigneeID = 2
	buttons = engineerQueueButtons(ticket, "https://t.me/c/500/55")
	if len(buttons) != 2 || buttons[0][0].Text != "В чат компании" || buttons[1][0].Text != engineerCloseLabel || buttons[1][1].Data != "transfer:42" {
		t.Fatalf("active group controls: %+v", buttons)
	}
	if len(engineerQueueButtons(ticket, "")) != 1 {
		t.Fatal("missing-link group card should still expose close and transfer")
	}
}

func TestGroupCardShowsCompanyChatAndRequester(t *testing.T) {
	ticket := domain.Ticket{
		ID:                42,
		ClientID:          -100500,
		Channel:           domain.ChannelGroup,
		Status:            domain.New,
		Department:        domain.OneC,
		Description:       "Не открывается 1С",
		RequesterName:     "Иван",
		RequesterUsername: "ivan",
	}
	text := engineerCardText(ticket, "ООО Ромашка", "Компания", "")
	for _, want := range []string{"Чат компании:", "ООО Ромашка", "Обратился:", "Иван · @ivan"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "<b>Клиент:</b>") {
		t.Fatalf("group card still labels the chat as a private client: %s", text)
	}
}
