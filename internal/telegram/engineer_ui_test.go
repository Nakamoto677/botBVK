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
func TestEngineerReplyButtonsStartServiceInputAndNeverRelay(t *testing.T) {
	for _, label := range []string{engineerCloseLabel, engineerTransferLabel} {
		t.Run(label, func(t *testing.T) {
			client, capture := capturedClient()
			store := &adapterStore{user: domain.User{ID: 2, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}
			svc := &adapterService{}
			a := NewAdapter(client, store, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
			message := &Message{ID: 80, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: label}
			if err := a.Handle(context.Background(), Update{Message: message}); err != nil {
				t.Fatal(err)
			}
			if len(svc.messages) != 0 || len(a.prompts) != 1 {
				t.Fatal("button relayed or prompt absent")
			}
			raw, _ := json.Marshal(capture.bodies[len(capture.bodies)-1]["reply_markup"])
			if !strings.Contains(string(raw), "Отменить") {
				t.Fatal("no escape button")
			}
			if label == engineerCloseLabel {
				message.Text = "Исправили доступ"
				if err := a.Handle(context.Background(), Update{Message: message}); err != nil {
					t.Fatal(err)
				}
				if svc.closedID != 7 || len(svc.messages) != 0 {
					t.Fatal("resolution relayed")
				}
			}
		})
	}
}
func TestEngineerKeyboardDeniedOutsideCurrentTopicAndToNonAssignee(t *testing.T) {
	client, _ := capturedClient()
	store := &adapterStore{user: domain.User{ID: 3, Role: "engineer", Approved: true, Departments: []domain.Department{domain.IT}}, ticket: domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, AssigneeID: 2, TopicChatID: -100, TopicID: 10}}
	svc := &adapterService{}
	a := NewAdapter(client, store, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
	for _, topic := range []int64{1, 9, 10} {
		err := a.Handle(context.Background(), Update{Message: &Message{From: Person{ID: 3}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: topic, Text: engineerCloseLabel}})
		if err == nil {
			t.Fatalf("unauthorized close accepted topic %d", topic)
		}
	}
	if len(svc.messages) != 0 || svc.closedID != 0 || len(a.prompts) != 0 {
		t.Fatal("forbidden button changed state")
	}
}
func TestEngineerKeyboardDurableOncePerBindingAndTopicMarkupSeparate(t *testing.T) {
	client, capture := capturedClient()
	ticket := domain.Ticket{ID: 7, Status: domain.InProgress, Department: domain.IT, TopicChatID: -100, TopicID: 10, TopicMessageID: 99}
	store := &workerStore{current: ticket}
	worker := NewWorker(client, store, Config{})
	worker.interval = 0
	if _, err := worker.ensureTopicCard(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(capture.bodies) != 1 {
		t.Fatal("keyboard not installed")
	}
	keyboard := capture.bodies[0]["reply_markup"].(map[string]any)
	raw, _ := json.Marshal(keyboard)
	if keyboard["is_persistent"] != true || capture.bodies[0]["message_thread_id"] != float64(10) || !strings.Contains(string(raw), engineerCloseLabel) {
		t.Fatal(capture.bodies)
	}
	// A new worker models a restart; recorded binding suppresses a duplicate.
	worker = NewWorker(client, store, Config{})
	worker.interval = 0
	if _, err := worker.ensureTopicCard(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(capture.bodies) != 1 {
		t.Fatal("keyboard duplicated on restart")
	}
	ticket.TopicID = 20
	ticket.RoutingGeneration++
	store.current = ticket
	if _, err := worker.ensureTopicCard(context.Background(), ticket); err != nil {
		t.Fatal(err)
	}
	if len(capture.bodies) != 2 {
		t.Fatal("new topic did not receive controls")
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
		m := &Message{ID: 80, From: Person{ID: 2}, Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10, Text: engineerTransferLabel}
		if err := a.Handle(ctx, Update{Message: m}); err != nil {
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
			if p.ID == 80 {
				removed = true
			}
			if p.ID == 81 {
				t.Fatal("free-text reason deleted")
			}
		}
		if !removed {
			t.Fatal("transfer control retained")
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
