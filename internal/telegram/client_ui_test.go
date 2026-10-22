package telegram

import (
	"bot-bvk/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type minimalClientService struct {
	*cancelUIService
	createCalls int
	createErr   error
}

func (s *minimalClientService) Create(_ context.Context, actor int64, d domain.Department, text string) (domain.Ticket, error) {
	if s.createErr != nil {
		return domain.Ticket{}, s.createErr
	}
	s.createCalls++
	s.store.ticket = domain.Ticket{ID: 7, ClientID: actor, CompanyID: 10, Department: d, Description: text, Status: domain.New}
	return s.store.ticket, nil
}
func assertReplyLabels(t *testing.T, p map[string]any, want ...string) {
	t.Helper()
	raw, _ := json.Marshal(p["reply_markup"])
	var markup struct {
		Keyboard [][]struct {
			Text string `json:"text"`
		} `json:"keyboard"`
	}
	json.Unmarshal(raw, &markup)
	got := []string{}
	for _, row := range markup.Keyboard {
		for _, button := range row {
			got = append(got, button.Text)
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("labels %v want %v: %s", got, want, raw)
	}
}
func TestMinimalClientFlowDraftCancelAcceptedAndFinished(t *testing.T) {
	ctx := context.Background()
	a, s, cancel, m, requests := clientUIFixture(t)
	s.ticket = domain.Ticket{}
	svc := &minimalClientService{cancelUIService: cancel}
	a.service = svc
	send := func(text string) {
		t.Helper()
		m.Text = text
		if err := a.Handle(ctx, Update{Message: m}); err != nil {
			t.Fatal(err)
		}
	}
	callback := func(data string) {
		t.Helper()
		botMessage := &Message{ID: 101, From: Person{ID: 999, IsBot: true}, Chat: m.Chat}
		if err := a.callback(ctx, &Callback{ID: "c", From: m.From, Message: botMessage, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	send("/start")
	assertReplyLabels(t, (*requests)[len(*requests)-1], "Создать заявку", "Помощь")
	send("Создать заявку")
	assertReplyLabels(t, (*requests)[len(*requests)-2], clientCancelDraftLabel)
	raw, _ := json.Marshal((*requests)[len(*requests)-1]["reply_markup"])
	var options Markup
	json.Unmarshal(raw, &options)
	if len(options.Buttons) != 3 || len(options.Buttons[0]) != 2 || len(options.Buttons[1]) != 2 {
		t.Fatal(string(raw))
	}
	send(clientCancelDraftLabel)
	assertReplyLabels(t, (*requests)[len(*requests)-1], "Создать заявку", "Помощь")
	if svc.createCalls != 0 || len(a.drafts) != 0 {
		t.Fatal("draft cancellation created ticket")
	}
	send("Создать заявку")
	callback("new:it:" + a.drafts[1].Generation)
	callback("urgency:urgent:" + a.drafts[1].Generation)
	// callback message is sent by the bot, but temporary messages belong to client.
	if len(s.aux[auxKey{999, 1, 0, "client_kept"}]) != 0 {
		t.Fatal("bot author used as client scenario actor")
	}
	if a.drafts[1].Step != "description" {
		t.Fatal("urgency choice did not advance to description")
	}
	send("Не работает компьютер")
	previous := a.drafts[1]
	callback("changeurgency:" + previous.Generation)
	if a.drafts[1].Step != "change_urgency" || a.drafts[1].Text != previous.Text {
		t.Fatal("description lost while changing urgency")
	}
	callback("urgency:routine:" + a.drafts[1].Generation)
	if a.drafts[1].Step != "confirm" || a.drafts[1].Urgency != domain.Routine || a.drafts[1].Text != previous.Text || a.drafts[1].Generation == previous.Generation {
		t.Fatal("urgency change did not preserve draft or invalidate old buttons")
	}
	raw, _ = json.Marshal((*requests)[len(*requests)-1]["reply_markup"])
	if !strings.Contains(string(raw), clientCancelDraftLabel) || !strings.Contains(string(raw), "submit") {
		t.Fatal(string(raw))
	}
	svc.createErr = errors.New("temporary storage failure")
	botMsg := &Message{From: Person{ID: 999}, Chat: m.Chat}
	if err := a.callback(ctx, &Callback{From: m.From, Message: botMsg, Data: "submit:" + a.drafts[1].Generation}); err == nil {
		t.Fatal("failed submission accepted")
	}
	if len(a.drafts) != 1 {
		t.Fatal("failure removed retry UI")
	}
	svc.createErr = nil
	callback("submit:" + a.drafts[1].Generation)
	if a.drafts[1].Step != "submitted" || svc.createCalls != 1 {
		t.Fatal("draft remained after creation")
	}
	w := NewWorker(a.client, s, Config{Departments: map[domain.Department]Team{domain.IT: {ChatID: -100, QueueTopicID: 2}}})
	w.interval = 0
	if err := w.clientNotification(ctx, s.ticket, "Заявка принята и поставлена в очередь."); err != nil {
		t.Fatal(err)
	}
	last := (*requests)[len(*requests)-1]
	if last["text"] != clientAcceptedText {
		t.Fatal(last)
	}
	assertReplyLabels(t, last, "Отменить заявку")
	send("Отменить заявку")
	if (*requests)[len(*requests)-1]["text"] != "Точно проблема решена?" {
		t.Fatal("wrong confirmation")
	}
	parts := currentParts(s)
	callback("cancelconfirm:7:" + parts[2])
	// Only notifications are UI: normal transcript records aren't registered.
	s.storedMessages = []domain.Message{{ID: 44, Direction: "inbound", Text: "Сохранить историю", FileID: "file"}}
	if err := w.clientNotification(ctx, s.ticket, "Заявка завершена по вашему запросу."); err != nil {
		t.Fatal(err)
	}
	last = (*requests)[len(*requests)-1]
	if last["text"] != clientFinishedText {
		t.Fatal(last)
	}
	assertReplyLabels(t, last, "Создать заявку", "Помощь")
	if len(s.aux[auxKey{1, 1, 0, "client_home"}]) != 1 {
		t.Fatalf("auxiliary UI retained: %v", s.aux)
	}
	if len(s.storedMessages) != 1 || s.storedMessages[0].ID != 44 {
		t.Fatal("history deleted")
	}
}
func TestStaleDraftCancelAndDepartmentCannotAffectActiveTicket(t *testing.T) {
	a, s, svc, m, requests := clientUIFixture(t)
	s.ticket.Status = domain.New
	a.drafts[1] = draft{Department: domain.IT, Text: "saved"}
	if err := a.cancelClientDraft(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if len(a.drafts) != 1 || svc.calls != 0 {
		t.Fatal("stale draft cancellation changed ticket")
	}
	botMsg := &Message{From: Person{ID: 999}, Chat: m.Chat}
	// A stale department screen is a rejected action, not a silent success: the
	// typed error stops the caller and lets the user be told the screen is
	// outdated instead of leaving the callback unanswered.
	before := len(*requests)
	if err := a.callback(context.Background(), &Callback{From: m.From, Message: botMsg, Data: "new:1c"}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale department accepted: %v", err)
	}
	// Only the outdated-screen notice and the button cleanup may follow. A delivered
	// screen carries text and a chat_id, while the notice is a callback answer, so
	// that pair identifies a new prompt unambiguously.
	for _, req := range (*requests)[before:] {
		_, hasText := req["text"]
		_, hasChat := req["chat_id"]
		if hasText && hasChat {
			t.Fatalf("stale department sent new prompt: %v", req)
		}
	}
}

func TestLateApprovalPreservesActiveMenuAndRegistersTemporaryMessage(t *testing.T) {
	a, s, _, _, requests := clientUIFixture(t)
	w := NewWorker(a.client, s, Config{})
	w.interval = 0
	if err := w.clientAccessNotification(context.Background(), 1, "Доступ одобрен"); err != nil {
		t.Fatal(err)
	}
	assertReplyLabels(t, (*requests)[len(*requests)-1], "Отменить заявку")
	if len(s.aux[auxKey{1, 1, 0, clientTicketScenario(7)}]) != 1 {
		t.Fatal("approval notification not tracked")
	}
}

func TestClientMilestonesSurviveCompletionAndTemporaryQuestionsAreCleaned(t *testing.T) {
	ctx := context.Background()
	a, s, _, m, _ := clientUIFixture(t)
	texts := []string{"Создание заявки.", "Выберите направление.", "Опишите проблему текстом. Вложения отправьте после создания заявки.", "Создать заявку?\nОписание", clientAcceptedText, "Ваше обращение передали в отдел 1С\nИнженеры увидели вашу заявку, скоро возьмут в работу!", "Инженер Мария Петрова взял вашу заявку в работу\nВаша переписка продолжится прямо в этом чате:", "Подача заявки отменена. Вы можете создать новую."}
	for _, text := range texts {
		if err := a.sayAux(ctx, m, 1, "draft", text, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.sayAux(ctx, m, 1, "draft", "Временная подсказка", nil); err != nil {
		t.Fatal(err)
	}
	a.cleanupBestEffort(ctx, scope(1, m), "draft")
	kept := s.aux[auxKey{1, 1, 0, "client_kept"}]
	if len(kept) != len(texts) {
		t.Fatal("milestones not retained", s.aux)
	}
	protected := map[int64]bool{}
	for _, msg := range kept {
		protected[msg.MessageID] = true
	}
	s.ticket.Status = domain.Closed
	w := NewWorker(a.client, s, Config{})
	w.interval = 0
	if err := w.clientNotification(ctx, s.ticket, "Заявка закрыта. Результат: Решено"); err != nil {
		t.Fatal(err)
	}
	for _, op := range s.operations {
		var p struct {
			ID int64 `json:"message_id"`
		}
		json.Unmarshal(op.Payload, &p)
		if protected[p.ID] {
			t.Fatal("milestone queued for deletion", p.ID)
		}
	}
	if len(s.operations) != 1 {
		t.Fatal("temporary question not cleaned", s.operations)
	}
	// Exact private-client matching must not preserve an engineer service prompt.
	group := &Message{Chat: Chat{ID: -100, Type: "supergroup"}, ThreadID: 10}
	if got := clientAuxScenario(group, "action:7", texts[0]); got != "action:7" {
		t.Fatal(got)
	}
}
