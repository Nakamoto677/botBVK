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
	buttons := engineerQueueButtons(ticket)
	if len(buttons) != 2 || buttons[0][0].Data != "claim:42" || buttons[1][0].Data != "transfer:42" {
		t.Fatalf("unclaimed controls: %+v", buttons)
	}
	ticket.Status = domain.InProgress
	ticket.AssigneeID = 2
	ticket.TopicChatID = -1001234567890
	ticket.TopicID = 7
	ticket.TopicMessageID = 88
	buttons = engineerQueueButtons(ticket)
	if len(buttons) != 2 || len(buttons[1]) != 2 || buttons[0][0].Text != "В чат" || buttons[0][0].URL != "https://t.me/c/1234567890/88?thread=7" || buttons[1][0].Text != "Закрыть задачу" {
		t.Fatalf("assigned controls: %+v", buttons)
	}
	topic := engineerTopicButtons(ticket)
	if len(topic) != 1 || len(topic[0]) != 2 || topic[0][0].Text != engineerCloseLabel || topic[0][1].Data != "transfer:42" {
		t.Fatal(topic)
	}
	ticket.RoutingPending = true
	if len(engineerQueueButtons(ticket)) != 0 {
		t.Fatal("actions before provisioning")
	}
	for _, status := range []domain.Status{domain.Closed, domain.Cancelled} {
		ticket.Status = status
		if len(engineerQueueButtons(ticket))+len(engineerTopicButtons(ticket)) != 0 {
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
		for _, text := range []string{ticketText(ticket, name, "Компания", ""), engineerCardText(ticket, name, "Компания", "")} {
			if !strings.Contains(text, want) {
				t.Fatal(text)
			}
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
		a := NewAdapter(client, s, svc, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 1}}})
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
			want := "Передача заявки #7\n\nОтдел: 1С\n\nНапишите комментарий к передаче следующим сообщением.\nЕсли комментарий не нужен, отправьте прочерк «-»."
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

func TestClosedQueueCardEscapesPrivateCommentAndReopenClearsIt(t *testing.T) {
	ticket := domain.Ticket{ID: 7, Department: domain.IT, Status: domain.Closed, Resolution: "Исправлено <пароль> & доступ"}
	text := engineerCardText(ticket, "Клиент", "Компания", "Инженер")
	if !strings.Contains(text, "Служебный комментарий закрытия") || !strings.Contains(text, "Исправлено &lt;пароль&gt; &amp; доступ") || strings.Contains(text, "<пароль>") {
		t.Fatal("closed queue card missing or unsafe", text)
	}
	ticket.Status = domain.New
	if strings.Contains(engineerCardText(ticket, "Клиент", "Компания", "Инженер"), "пароль") {
		t.Fatal("reopened card kept stale close comment")
	}
}
